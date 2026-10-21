// Package billing reconciles corrallm's per-request cost metering against what
// a provider actually billed.
//
// Metering is an estimate made from the usage each response reports, and a
// response whose usage never arrives is silently uncounted: Fireworks billed
// $0.72 in a month where corrallm recorded $0.22. The bill is the truth, so
// this package periodically fetches it, stores it beside the recorded figure,
// raises the spend ledger's calendar windows to it, and alerts on drift.
package billing

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/alert"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Cost is one hour of one upstream model on a provider's bill.
type Cost struct {
	Model string // the provider's own model id, as billed
	Hour  time.Time
	USD   float64
}

// Result is one fetch.
type Result struct {
	Costs []Cost
	// Incomplete: the provider says recent hours are still settling, so the
	// newest figures may rise. They are used as they are — the ledger floor
	// only ever rises, and a later fetch corrects the stored rows.
	Incomplete bool
}

// Provider fetches a provider's bill. Fireworks is the only implementation; a
// provider with a billing API adds one and is picked by its endpoint.
type Provider interface {
	Name() string
	// Matches reports whether the provider's endpoint belongs to this API.
	Matches(endpoint *url.URL) bool
	// Costs returns billed USD per hour and model in [from, to), authenticating
	// with the credential's resolved request headers.
	Costs(ctx context.Context, headers map[string]string, from, to time.Time) (Result, error)
}

// Floorer raises a budget scope's calendar-window spend (proxy.RaiseSpendFloor).
type Floorer interface {
	RaiseSpendFloor(scope, unit string, usd float64) float64
}

// Alert thresholds: the bill must exceed the recorded figure by more than
// max(minDriftUSD, driftFraction of recorded) before anyone is told. Small
// differences are normal — hours settle late and rounding differs.
const (
	minDriftUSD   = 0.05
	driftFraction = 0.05
)

// Reconciler runs the periodic comparison.
type Reconciler struct {
	Store     *store.Store
	Config    func() *config.Config
	Providers []Provider
	Ledger    Floorer
	Alert     *alert.Notifier
	Now       func() time.Time
}

// New builds a reconciler with the built-in providers.
func New(st *store.Store, cfg func() *config.Config, ledger Floorer, n *alert.Notifier) *Reconciler {
	return &Reconciler{Store: st, Config: cfg, Providers: []Provider{NewFireworks()},
		Ledger: ledger, Alert: n, Now: time.Now}
}

// Run reconciles after startDelay and then every interval until ctx ends. The
// delay keeps a slow billing API from competing with startup.
func (r *Reconciler) Run(ctx context.Context, startDelay, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// target is one credential of one recognised provider.
type target struct {
	provider string // provider name in the config
	cred     config.Credential
	api      Provider
	headers  map[string]string
	single   bool                // the provider has one credential, so recorded cost is unambiguous
	byUp     map[string][]string // normalised upstream id -> served names
	upOf     map[string]string   // served name -> upstream id as configured
	served   []string
}

// targets lists every credential whose provider has a billing implementation.
// This is the whole opt-in: a provider is reconciled when its endpoint is one a
// Provider recognises (today: host api.fireworks.ai). No config key — a bill
// exists whether or not anyone asked, and the only cost of reading it is one
// small API call an hour.
func (r *Reconciler) targets(cfg *config.Config) []target {
	var out []target
	models := cfg.AllModels()
	exts := make([]string, 0, len(cfg.Extensions))
	for e := range cfg.Extensions {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	for _, ext := range exts {
		for pn, pv := range cfg.Extensions[ext].Providers {
			base, err := (config.Model{Proxy: pv.Proxy}).ProxyTarget()
			if err != nil || base.URL == nil {
				continue
			}
			var api Provider
			for _, p := range r.Providers {
				if p.Matches(base.URL) {
					api = p
					break
				}
			}
			if api == nil {
				continue
			}
			byUp, upOf := map[string][]string{}, map[string]string{}
			var served []string
			for name, m := range models {
				if m.Extension != ext || m.ProviderName != pn || m.Upstream == "" {
					continue
				}
				k := NormModel(m.Upstream)
				byUp[k] = append(byUp[k], name)
				upOf[name] = m.Upstream
				served = append(served, name)
			}
			for _, v := range byUp {
				sort.Strings(v)
			}
			sort.Strings(served)
			creds := pv.CredentialList()
			for _, cr := range creds {
				_, t, _, err := cfg.ProviderTarget(pn, cr.Name)
				if err != nil || len(t.Headers) == 0 {
					continue // nothing to authenticate with
				}
				out = append(out, target{provider: pn, cred: cr, api: api, headers: t.Headers,
					single: len(creds) == 1, byUp: byUp, upOf: upOf, served: served})
			}
		}
	}
	return out
}

// NormModel reduces a model id to the form the bill and the config can both be
// compared in: lowercase, without the "accounts/<owner>/models/" prefix.
func NormModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(id, "/models/"); i >= 0 {
		id = id[i+len("/models/"):]
	}
	return id
}

// RunOnce reconciles every recognised credential.
func (r *Reconciler) RunOnce(ctx context.Context) {
	cfg := r.Config()
	if cfg == nil {
		return
	}
	for _, t := range r.targets(cfg) {
		if err := r.reconcile(ctx, t); err != nil {
			slog.Warn("billing reconcile failed", "provider", t.provider, "credential", t.cred.Name, "err", err)
			_ = r.Store.RecordBillingError(t.provider, t.cred.Name, r.Now().UnixMilli(), err.Error())
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context, t target) error {
	now := r.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	res, err := t.api.Costs(ctx, t.headers, month, now)
	if err != nil {
		return err
	}

	// Recorded cost is only attributable when the provider has ONE credential:
	// activity rows name the served model, not the account that paid, so with
	// several accounts a model's spend could belong to any of them. The bill
	// side (and so the ledger floor) is still per account and stays exact.
	var recorded map[string]map[int64]float64
	if t.single {
		recorded, err = r.Store.RecordedCostByHour(t.served, month.UnixMilli(), now.Add(time.Hour).UnixMilli())
		if err != nil {
			return err
		}
	}

	type key struct {
		model string
		hour  int64
	}
	rows := map[key]*store.BillingRow{}
	row := func(model string, hour int64) *store.BillingRow {
		k := key{NormModel(model), hour}
		if rows[k] == nil {
			rows[k] = &store.BillingRow{Provider: t.provider, Credential: t.cred.Name, Upstream: model,
				Served: strings.Join(t.byUp[k.model], ","), HourMS: hour, Incomplete: res.Incomplete}
		}
		return rows[k]
	}
	var billedMonth, billedDay, recMonth, recDay float64
	for _, c := range res.Costs {
		row(c.Model, c.Hour.UnixMilli()).BilledUSD += c.USD
		billedMonth += c.USD
		if !c.Hour.Before(day) {
			billedDay += c.USD
		}
	}
	// Recorded spend with no billed hour is still a row: it is the other half of
	// a disagreement (we charged for something the provider did not bill).
	for name, hours := range recorded {
		for hour, usd := range hours {
			row(t.upOf[name], hour).RecordedUSD += usd
			recMonth += usd
			if hour >= day.UnixMilli() {
				recDay += usd
			}
		}
	}
	out := make([]store.BillingRow, 0, len(rows))
	for _, rw := range rows {
		out = append(out, *rw)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HourMS != out[j].HourMS {
			return out[i].HourMS < out[j].HourMS
		}
		return out[i].Upstream < out[j].Upstream
	})
	if err := r.Store.ReplaceBilling(t.provider, t.cred.Name, month.UnixMilli(), now.UnixMilli(), out, res.Incomplete); err != nil {
		return err
	}

	scope := t.cred.ScopeKey(t.provider)
	if r.Ledger != nil {
		for _, w := range []struct {
			unit   string
			billed float64
		}{{"month", billedMonth}, {"day", billedDay}} {
			if d := r.Ledger.RaiseSpendFloor(scope, w.unit, w.billed); d > 0 {
				slog.Info("billing: spend ledger raised to the provider's bill", "scope", scope,
					"window", "calendar-"+w.unit, "billedUSD", round6(w.billed), "raisedUSD", round6(d), "settling", res.Incomplete)
			}
		}
	}
	if t.single {
		r.checkDrift(t, now, []window{
			{"month", month.Format("2006-01"), billedMonth, recMonth},
			{"day", day.Format("2006-01-02"), billedDay, recDay},
		})
	}
	return nil
}

type window struct {
	unit, period     string
	billed, recorded float64
}

// Drift is the amount the bill exceeds the recorded figure by, and whether that
// is enough to report.
func Drift(billed, recorded float64) (float64, bool) {
	d := billed - recorded
	thr := recorded * driftFraction
	if thr < minDriftUSD {
		thr = minDriftUSD
	}
	return d, d > thr
}

// newEpisode reports whether drift d in a window is news, given the episode
// on record for it. The rule:
//
//   - no episode on record, or one for an earlier period (a new day or
//     month): news;
//   - the drift has at least DOUBLED since the last alert: news again — the
//     unmetered amount is growing, not just still there;
//   - otherwise the same episode: quiet.
//
// A window that falls back under the threshold ends its episode (cleared in
// checkDrift), so a drift that reappears later alerts again.
func newEpisode(period string, d float64, recPeriod string, recUSD float64, ok bool) bool {
	return !ok || recPeriod != period || d >= 2*recUSD
}

// checkDrift sends ONE message when any window newly enters drift (see
// newEpisode), and clears a window's episode when it falls back under the
// threshold. Episodes are stored (billing_alert), so a restart or blue-green
// handoff does not re-announce an unchanged drift.
func (r *Reconciler) checkDrift(t target, now time.Time, ws []window) {
	var lines []string
	type mark struct {
		unit, period string
		d            float64
	}
	var marks []mark
	for _, w := range ws {
		d, over := Drift(w.billed, w.recorded)
		if !over {
			if err := r.Store.ClearBillingAlert(t.provider, t.cred.Name, w.unit); err != nil {
				slog.Warn("billing: clear drift episode", "err", err)
			}
			continue
		}
		// A read failure alerts: a repeated message beats a missed one.
		recPeriod, recUSD, ok, err := r.Store.BillingAlert(t.provider, t.cred.Name, w.unit)
		if err != nil {
			slog.Warn("billing: read drift episode", "err", err)
		}
		if !newEpisode(w.period, d, recPeriod, recUSD, ok) {
			continue
		}
		marks = append(marks, mark{w.unit, w.period, d})
		lines = append(lines, fmt.Sprintf("%s %s: billed $%.2f, recorded $%.2f, unmetered $%.2f",
			w.unit, w.period, w.billed, w.recorded, d))
	}
	for _, m := range marks {
		if err := r.Store.SetBillingAlert(t.provider, t.cred.Name, m.unit, m.period, m.d, now.UnixMilli()); err != nil {
			slog.Warn("billing: record drift episode", "err", err)
		}
	}
	if len(lines) == 0 {
		return
	}
	body := strings.Join(lines, "\n") + "\nThe spend ledger was raised to the billed figure. Some requests are not being metered."
	slog.Warn("billing drift", "provider", t.provider, "credential", t.cred.Name, "detail", strings.Join(lines, "; "))
	// The key carries the hour so the notifier's own 30-minute throttle never
	// swallows a message the episode logic already decided to send.
	r.Alert.Send("billing-drift/"+t.provider+"/"+t.cred.Name+"/"+now.Format("2006-01-02T15"),
		t.provider+": the bill is higher than corrallm recorded", body, "warning")
}

func round6(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }
