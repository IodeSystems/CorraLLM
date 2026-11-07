package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/metrics"
	"github.com/iodesystems/corrallm/internal/store"
)

// Spend: what Claude has cost this month, for a client that may read that and
// nothing else. Anthropic gives an individual account no way to read its bill, so
// the ledger is the source: exact tokens from every reply, at the prices in
// corrallm's config, which is an estimate and says so. corrallm sees all the
// traffic through it, and a hard limit on the credential refuses past the budget.
// An operator can give the real figure off the Console; only what is spent after
// that is then estimated.
//
// A cost token is not a key. It has its own table and prefix, opens GET /v1/spend
// and is refused everywhere else, so it can be handed to a dashboard or a dun.

// SpendTokenPrefix marks a cost token.
const SpendTokenPrefix = "spend-"

// spendAnthropic names the provider in the reply and in the reconcile table.
const spendAnthropic = "anthropic"

// Where a provider's figure comes from.
const (
	spendFromEstimate   = "estimate"   // the ledger alone
	spendFromReconciled = "reconciled" // a real figure, plus the ledger after it
)

// SpendBudget is a timed budget on a provider.
type SpendBudget struct {
	ID       int64   `json:"id"`
	Provider string  `json:"provider"`
	USD      float64 `json:"usd"`
	SpentUSD float64 `json:"spent_usd"`
	LeftUSD  float64 `json:"left_usd"` // 0 once it has lapsed
	Expires  string  `json:"expires"`
	Active   bool    `json:"active"`
	Note     string  `json:"note,omitempty"`
}

// SpendProvider is one provider's period. A figure corrallm does not have is
// absent, never 0: a client must not read "no budget" as "nothing left".
type SpendProvider struct {
	Provider      string   `json:"provider"`
	Period        string   `json:"period"`
	BilledUSD     *float64 `json:"billed_usd,omitempty"`
	EstimatedUSD  float64  `json:"estimated_usd"`
	RemainingUSD  *float64 `json:"remaining_usd,omitempty"`
	LimitUSD      *float64 `json:"limit_usd,omitempty"`
	ResetsAt      string   `json:"resets_at,omitempty"`
	CostEstimated bool     `json:"cost_estimated"`
	Source        string   `json:"source"`
	StaleSeconds  *int64   `json:"stale_seconds,omitempty"`
	// TodayUSD is a paced day's allowance: what is left of the month, over the
	// days to its renewal. TodaySpentUSD is what today has spent of it.
	TodayUSD      *float64      `json:"today_usd,omitempty"`
	TodaySpentUSD *float64      `json:"today_spent_usd,omitempty"`
	Budgets       []SpendBudget `json:"budgets,omitempty"`
	// Account is the same money as chunks and ceilings (P74), from the ledger's
	// counters: what the gate admits. It can differ from remaining_usd, which is
	// read from the activity log, when a counter started later than the period.
	Account *Account `json:"account,omitempty"`

	names []string // the config's provider names this entry covers
	// for the Prometheus gauges; the JSON carries these as strings
	periodStart  time.Time
	resetsAt     *time.Time
	reconciledAt *time.Time
}

// SpendReport is the body of GET /v1/spend.
type SpendReport struct {
	Object    string          `json:"object"`
	AsOf      string          `json:"as_of"`
	Currency  string          `json:"currency"`
	Providers []SpendProvider `json:"providers"`
}

// claudeSpend is what corrallm pays Claude for: the models, the quota scopes that
// count them, and the tightest calendar-month dollar limit. A caller's own sign-in
// is theirs to pay for and is not here.
type claudeSpend struct {
	served []string
	scopes []string
	limit  config.Limit // USD 0 when there is none
	paced  bool         // a paced calendar-day limit sits beside it
	// providers are the config's provider names the models are declared under.
	providers []string
}

func claudeBackends(cfg *config.Config) claudeSpend {
	var out claudeSpend
	seen := map[string]bool{}
	scope := func(s string) {
		if !seen[s] {
			seen[s] = true
			out.scopes = append(out.scopes, s)
		}
	}
	limit := func(ls config.LimitSet) {
		for _, l := range ls {
			if l.Pace {
				out.paced = true
			}
			if l.USD > 0 && l.Calendar() == "month" && (out.limit.USD == 0 || l.USD < out.limit.USD) {
				out.limit = l
			}
		}
	}
	for name, m := range cfg.Models {
		t, err := m.ProxyTarget()
		if err != nil || t == nil || t.API != config.APIAnthropic || t.CallerAuth {
			continue
		}
		out.served = append(out.served, name)
		if m.ProviderName != "" && !slices.Contains(out.providers, m.ProviderName) {
			out.providers = append(out.providers, m.ProviderName)
		}
		if ext, ok := cfg.Extensions[m.Extension]; ok {
			if pv, ok := ext.Providers[m.ProviderName]; ok {
				limit(pv.Limits)
				if len(pv.Limits) > 0 {
					scope(config.ProviderScopeKey(m.ProviderName))
				}
				for _, c := range pv.CredentialList() {
					limit(c.Limits)
					scope(c.ScopeKey(m.ProviderName))
				}
			}
		}
	}
	sort.Strings(out.served)
	sort.Strings(out.scopes)
	sort.Strings(out.providers)
	return out
}

// ReportSpend reads the period so far from the config and the database, so the
// operator's CLI answers without the daemon. Claude comes first, as it always
// has; then every other provider with a dollar limit (its own or a
// credential's) or a timed budget, by name (P74).
func ReportSpend(cfg *config.Config, st *store.Store, now time.Time) (SpendReport, error) {
	now = now.UTC()
	rep := SpendReport{Object: "spend", AsOf: now.Format(time.RFC3339), Currency: "usd", Providers: []SpendProvider{}}
	if st == nil {
		return rep, nil
	}
	timed, err := providerBudgets(st, now)
	if err != nil {
		return rep, err
	}
	counters, err := counterIndex(st)
	if err != nil {
		return rep, err
	}
	claude := claudeBackends(cfg)
	sources := otherProviders(cfg, claude, timed)
	if len(claude.served) > 0 {
		sources = append([]namedSpend{{spendAnthropic, claude}}, sources...)
	}
	for _, src := range sources {
		out, err := reportProvider(cfg, st, src.name, src.spend, timed, counters, now)
		if err != nil {
			return rep, err
		}
		rep.Providers = append(rep.Providers, out)
	}
	return rep, nil
}

// reportProvider is one provider's period. Only Claude can have a reconciled
// figure (`corrallm spend reconcile` reads Anthropic's Console); the rest are
// the ledger alone.
func reportProvider(cfg *config.Config, st *store.Store, name string, claude claudeSpend, timed []SpendBudget,
	counters map[string]store.QuotaCounter, now time.Time) (SpendProvider, error) {
	start := config.PeriodStart("month", claude.limit.Renews, now)
	period := now.Format("2006-01")
	if claude.limit.Renews > 0 {
		period = start.Format("2006-01-02")
	}
	month := start.UnixMilli()
	out := SpendProvider{Provider: name, Period: period, CostEstimated: true, Source: spendFromEstimate, periodStart: start,
		names: claude.providers}
	from := month
	if name == spendAnthropic {
		usd, at, ok, err := st.SpendReconcile(spendAnthropic)
		if err != nil {
			return out, err
		}
		// Last period's figure says nothing about this one.
		if ok && at >= month {
			from = at
			out.BilledUSD = &usd
			out.EstimatedUSD = usd
			out.Source = spendFromReconciled
			stale := (now.UnixMilli() - at) / 1000
			out.StaleSeconds = &stale
			rat := time.UnixMilli(at).UTC()
			out.reconciledAt = &rat
		}
	}
	recorded, err := st.RecordedCostByHour(claude.served, from, now.UnixMilli()+1)
	if err != nil {
		return out, err
	}
	for _, hours := range recorded {
		for _, v := range hours {
			out.EstimatedUSD += v
		}
	}
	var timedLeft float64
	for _, b := range timed {
		if slices.Contains(claude.scopes, config.ProviderScopeKey(b.Provider)) {
			out.Budgets = append(out.Budgets, b)
			if b.Active {
				timedLeft += b.LeftUSD
			}
		}
	}
	if claude.limit.USD > 0 {
		out.LimitUSD = &claude.limit.USD
		left := max(claude.limit.USD-out.EstimatedUSD, 0) + timedLeft
		out.RemainingUSD = &left
		resets := start.AddDate(0, 1, 0)
		out.ResetsAt = resets.Format(time.RFC3339)
		out.resetsAt = &resets
		if claude.paced {
			day := config.PeriodStart("day", 0, now).UnixMilli()
			today, err := st.RecordedCostByHour(claude.served, day, now.UnixMilli()+1)
			if err != nil {
				return out, err
			}
			var spent float64
			for _, hours := range today {
				for _, v := range hours {
					spent += v
				}
			}
			allow := config.PacedAllowance(claude.limit.USD, out.EstimatedUSD, spent, claude.limit.Renews, now)
			out.TodayUSD, out.TodaySpentUSD = &allow, &spent
		}
	}
	out.Account = providerAccount(cfg, claude.scopes, out.Budgets, counters, now)
	return out, nil
}

type namedSpend struct {
	name  string
	spend claudeSpend
}

// otherProviders is every provider but Claude's with a dollar limit on it or on
// one of its credentials, or a timed budget, sorted by name. Its models are the
// declared ones and what discovery added (passthrough). A CLI run has no
// discovery, so there estimated_usd misses a passthrough model's spend; the
// account view reads the ledger's counters and misses nothing.
func otherProviders(cfg *config.Config, claude claudeSpend, timed []SpendBudget) []namedSpend {
	byName := map[string]*claudeSpend{}
	limit := func(out *claudeSpend, ls config.LimitSet) bool {
		has := false
		for _, l := range ls {
			if l.Dimension() != config.DimUSD {
				continue
			}
			has = true
			if l.Pace {
				out.paced = true
			}
			if l.USD > 0 && l.Calendar() == "month" && (out.limit.USD == 0 || l.USD < out.limit.USD) {
				out.limit = l
			}
		}
		return has
	}
	for _, ext := range cfg.Extensions {
		for pn, pv := range ext.Providers {
			if slices.Contains(claude.providers, pn) {
				continue
			}
			s := byName[pn]
			if s == nil {
				s = &claudeSpend{providers: []string{pn}}
			}
			has := limit(s, pv.Limits)
			if len(pv.Limits) > 0 && !slices.Contains(s.scopes, config.ProviderScopeKey(pn)) {
				s.scopes = append(s.scopes, config.ProviderScopeKey(pn))
			}
			for _, c := range pv.CredentialList() {
				has = limit(s, c.Limits) || has
				if !slices.Contains(s.scopes, c.ScopeKey(pn)) {
					s.scopes = append(s.scopes, c.ScopeKey(pn))
				}
			}
			if has || byName[pn] != nil || slices.ContainsFunc(timed, func(b SpendBudget) bool { return b.Provider == pn }) {
				byName[pn] = s
			}
		}
	}
	models := cfg.Discovered()
	for n, m := range cfg.Models {
		models[n] = m
	}
	out := make([]namedSpend, 0, len(byName))
	for name, s := range byName {
		for n, m := range models {
			if m.ProviderName != name {
				continue
			}
			if t, err := m.ProxyTarget(); err == nil && t != nil && t.CallerAuth {
				continue // the caller's own sign-in, not corrallm's spend
			}
			s.served = append(s.served, n)
		}
		sort.Strings(s.served)
		sort.Strings(s.scopes)
		out = append(out, namedSpend{name, *s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Spend reports the period so far.
func (p *Proxy) Spend(now time.Time) (SpendReport, error) {
	return ReportSpend(p.config(), p.store, now)
}

// SpendMetrics turns a report into the spend gauges' input.
func SpendMetrics(rep SpendReport) []metrics.Spend {
	out := make([]metrics.Spend, 0, len(rep.Providers))
	for _, p := range rep.Providers {
		out = append(out, metrics.Spend{
			Provider:     p.Provider,
			EstimatedUSD: p.EstimatedUSD,
			LimitUSD:     p.LimitUSD,
			RemainingUSD: p.RemainingUSD,
			ResetsAt:     p.resetsAt,
			PeriodStart:  p.periodStart,
			ReconciledAt: p.reconciledAt,
		})
	}
	return out
}

// SpentFromLeft turns what the Console says is left of the credit into what has
// been spent of the monthly limit.
func SpentFromLeft(cfg *config.Config, left float64) (float64, error) {
	limit := claudeBackends(cfg).limit.USD
	if limit == 0 {
		return 0, errors.New("no calendar-month USD limit on the Claude provider to measure from")
	}
	if left < 0 || left > limit {
		return 0, fmt.Errorf("left: $%.2f is not between $0 and the $%.2f limit", left, limit)
	}
	return limit - left, nil
}

// ReconcileSpend stores the real figure for the period and sets the budget counters
// to it, up or down, so a hard limit acts on the real spend.
func (p *Proxy) ReconcileSpend(usd float64, now time.Time) error {
	if p.store == nil {
		return errors.New("no store")
	}
	if err := p.store.SetSpendReconcile(spendAnthropic, usd, now.UnixMilli()); err != nil {
		return err
	}
	p.syncSpendCounters(now)
	return nil
}

// syncSpendCounters sets the counters to the reconciled figure plus what was
// recorded after it. It reads the figure, not a flag, so running it again changes
// nothing; a reload runs it to apply a figure the CLI stored.
func (p *Proxy) syncSpendCounters(now time.Time) {
	if p.store == nil || p.quota == nil {
		return
	}
	rep, err := p.Spend(now)
	if err != nil {
		slog.Warn("spend: counters not synced", "err", err)
		return
	}
	for _, r := range rep.Providers {
		if r.Source != spendFromReconciled {
			continue
		}
		for _, s := range claudeBackends(p.config()).scopes {
			p.quota.SetCalendarLevel(s, config.DimUSD, "month", r.EstimatedUSD)
		}
	}
}

// timedBudgetCovers reports whether a provider's timed budget pays past scope's
// full hard limit.
func (p *Proxy) timedBudgetCovers(scope, model string) bool {
	return config.IsProviderScope(scope) && p.quota.LimitFull(scope) && p.grants.usable(scope, model)
}

// A timed budget is a dollar amount on a provider with an expiry: "$50 more, until
// Friday". It pays once the provider's hard limit is full, and what is unspent at
// expiry is gone. It is a key_grant row with the provider's scope for its key, so
// the expiring-first spend and the persistence are the value add's.

// BudgetEdit is a change to a timed budget. Nil and empty fields stay as they are.
// Left sets the amount so that this much remains.
type BudgetEdit struct {
	USD, Left    *float64
	TTL, Expires string
	Note         *string
}

// AddProviderBudget stores a timed budget.
func AddProviderBudget(cfg *config.Config, st *store.Store, provider string, usd float64, ttl, expires, note string, now time.Time) (store.KeyGrant, error) {
	if !hardLimited(cfg, provider) {
		return store.KeyGrant{}, fmt.Errorf("provider %q has no hard limit for a timed budget to extend", provider)
	}
	if usd <= 0 {
		return store.KeyGrant{}, errors.New("usd must be more than 0")
	}
	if ttl == "" && expires == "" {
		return store.KeyGrant{}, errors.New("a timed budget needs an expiry: a ttl or an expires time")
	}
	exp, err := expiresAt(ttl, expires, now)
	if err != nil {
		return store.KeyGrant{}, err
	}
	g := store.KeyGrant{Key: config.ProviderScopeKey(provider), USD: usd, ExpiresMS: exp, CreatedMS: now.UnixMilli(), Note: note}
	g.ID, err = st.AddKeyGrant(g)
	return g, err
}

func hardLimited(cfg *config.Config, provider string) bool {
	for _, ext := range cfg.Extensions {
		if pv, ok := ext.Providers[provider]; ok {
			return slices.ContainsFunc(pv.Limits, func(l config.Limit) bool { return l.Hard })
		}
	}
	return false
}

// EditProviderBudget changes a timed budget.
func EditProviderBudget(st *store.Store, id int64, e BudgetEdit, now time.Time) error {
	g, err := providerBudget(st, id)
	if err != nil {
		return err
	}
	if e.USD != nil {
		g.USD = *e.USD
	}
	if e.Left != nil {
		g.USD = g.Spent + *e.Left
	}
	if g.USD <= 0 {
		return errors.New("usd must be more than 0; remove the budget to end it")
	}
	if e.TTL != "" || e.Expires != "" {
		if g.ExpiresMS, err = expiresAt(e.TTL, e.Expires, now); err != nil {
			return err
		}
	}
	if e.Note != nil {
		g.Note = *e.Note
	}
	return st.UpdateKeyGrant(id, g.USD, g.ExpiresMS, g.Note)
}

// RemoveProviderBudget deletes a timed budget.
func RemoveProviderBudget(st *store.Store, id int64) error {
	if _, err := providerBudget(st, id); err != nil {
		return err
	}
	return st.DeleteKeyGrant(id)
}

func providerBudget(st *store.Store, id int64) (store.KeyGrant, error) {
	rows, err := st.LoadKeyGrants()
	if err != nil {
		return store.KeyGrant{}, err
	}
	for _, g := range rows {
		if g.ID == id && config.IsProviderScope(g.Key) {
			return g, nil
		}
	}
	return store.KeyGrant{}, fmt.Errorf("no timed budget %d", id)
}

// providerBudgets lists every provider's timed budgets, the live ones and the
// lapsed ones of the last 90 days.
func providerBudgets(st *store.Store, now time.Time) ([]SpendBudget, error) {
	rows, err := st.LoadKeyGrants()
	if err != nil {
		return nil, err
	}
	var out []SpendBudget
	for _, g := range rows {
		if !config.IsProviderScope(g.Key) || g.ExpiresMS < now.AddDate(0, 0, -90).UnixMilli() {
			continue
		}
		active := grantActive(&g, now)
		b := SpendBudget{ID: g.ID, Provider: strings.TrimPrefix(g.Key, "prov:"), USD: g.USD, SpentUSD: g.Spent,
			Expires: time.UnixMilli(g.ExpiresMS).UTC().Format(time.RFC3339), Active: active, Note: g.Note}
		if active {
			b.LeftUSD = g.USD - g.Spent
		}
		out = append(out, b)
	}
	return out, nil
}

// AddBudget, EditBudget and RemoveBudget are the same for a running daemon, which
// holds budgets in memory.
func (p *Proxy) AddBudget(provider string, usd float64, ttl, expires, note string, now time.Time) error {
	if p.store == nil {
		return errors.New("no store")
	}
	_, err := AddProviderBudget(p.config(), p.store, provider, usd, ttl, expires, note, now)
	p.reloadGrants()
	return err
}

func (p *Proxy) EditBudget(id int64, e BudgetEdit, now time.Time) error {
	if p.store == nil {
		return errors.New("no store")
	}
	err := EditProviderBudget(p.store, id, e, now)
	p.reloadGrants()
	return err
}

func (p *Proxy) RemoveBudget(id int64) error {
	if p.store == nil {
		return errors.New("no store")
	}
	err := RemoveProviderBudget(p.store, id)
	p.reloadGrants()
	return err
}

// NewSpendToken mints a cost token and returns it, once, with its id. A zero ttl
// never expires.
func NewSpendToken(st *store.Store, label string, ttl time.Duration, now time.Time) (string, int64, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	token := SpendTokenPrefix + hex.EncodeToString(raw)
	var expires int64
	if ttl > 0 {
		expires = now.Add(ttl).UnixMilli()
	}
	id, err := st.CreateSpendToken(spendTokenHash(token), label, now.UnixMilli(), expires)
	return token, id, err
}

// CreateSpendToken is NewSpendToken on the proxy's store.
func (p *Proxy) CreateSpendToken(label string, ttl time.Duration, now time.Time) (string, int64, error) {
	if p.store == nil {
		return "", 0, errors.New("no store")
	}
	return NewSpendToken(p.store, label, ttl, now)
}

func spendTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) handleSpend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	token := callerKey(r)
	now := time.Now()
	ok := strings.HasPrefix(token, SpendTokenPrefix) && p.store != nil
	if ok {
		var err error
		if ok, err = p.store.UseSpendToken(spendTokenHash(token), now.UnixMilli()); err != nil {
			slog.Warn("spend token", "err", err)
			jsonErr(w, http.StatusInternalServerError, "could not check the token")
			return
		}
	}
	if !ok {
		jsonErr(w, http.StatusUnauthorized, "send a cost token as Authorization: Bearer spend-…")
		return
	}
	rep, err := p.Spend(now)
	if err != nil {
		slog.Warn("spend", "err", err)
		jsonErr(w, http.StatusInternalServerError, "could not read spend")
		return
	}
	jsonResp(w, http.StatusOK, rep)
}

// spendTokenGuard refuses a cost token on every route it wraps. Without it a token
// is only an unknown key, and `unknownKeys` may admit a stranger.
type spendTokenGuard struct {
	mux interface {
		Handle(pattern string, h http.Handler)
	}
}

func (g spendTokenGuard) Handle(pattern string, h http.Handler) {
	g.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(callerKey(r), SpendTokenPrefix) || strings.HasPrefix(r.Header.Get("X-Api-Key"), SpendTokenPrefix) {
			jsonErr(w, http.StatusUnauthorized, "a cost token reads GET /v1/spend and nothing else")
			return
		}
		h.ServeHTTP(w, r)
	}))
}
