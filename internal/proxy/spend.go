package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
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

// SpendProvider is one provider's month. A figure corrallm does not have is
// absent, never 0: a client must not read "no budget" as "nothing left".
type SpendProvider struct {
	Provider      string   `json:"provider"`
	Period        string   `json:"period"`
	BilledUSD     *float64 `json:"billed_usd,omitempty"`
	EstimatedUSD  float64  `json:"estimated_usd"`
	RemainingUSD  *float64 `json:"remaining_usd,omitempty"`
	CostEstimated bool     `json:"cost_estimated"`
	Source        string   `json:"source"`
	StaleSeconds  *int64   `json:"stale_seconds,omitempty"`
}

// SpendReport is the body of GET /v1/spend.
type SpendReport struct {
	Object    string          `json:"object"`
	AsOf      string          `json:"as_of"`
	Currency  string          `json:"currency"`
	Providers []SpendProvider `json:"providers"`
}

// claudeBackends lists the models corrallm pays Claude for, their tightest
// calendar-month dollar limit, and the quota scopes of their credentials. A
// caller's own sign-in is theirs to pay for and is not here.
func claudeBackends(cfg *config.Config) (served []string, budget float64, scopes []string) {
	seen := map[string]bool{}
	limit := func(ls config.LimitSet) {
		for _, l := range ls {
			if l.USD > 0 && l.Calendar() == "month" && (budget == 0 || l.USD < budget) {
				budget = l.USD
			}
		}
	}
	for name, m := range cfg.Models {
		t, err := m.ProxyTarget()
		if err != nil || t == nil || t.API != config.APIAnthropic || t.CallerAuth {
			continue
		}
		served = append(served, name)
		if ext, ok := cfg.Extensions[m.Extension]; ok {
			if pv, ok := ext.Providers[m.ProviderName]; ok {
				limit(pv.Limits)
				for _, c := range pv.CredentialList() {
					limit(c.Limits)
					if s := c.ScopeKey(m.ProviderName); !seen[s] {
						seen[s] = true
						scopes = append(scopes, s)
					}
				}
			}
		}
	}
	sort.Strings(served)
	return served, budget, scopes
}

// Spend reports the month so far.
func (p *Proxy) Spend(now time.Time) (SpendReport, error) {
	now = now.UTC()
	rep := SpendReport{Object: "spend", AsOf: now.Format(time.RFC3339), Currency: "usd", Providers: []SpendProvider{}}
	cfg := p.config()
	served, budget, _ := claudeBackends(cfg)
	if len(served) == 0 || p.store == nil {
		return rep, nil
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	out := SpendProvider{Provider: spendAnthropic, Period: now.Format("2006-01"), CostEstimated: true, Source: spendFromEstimate}
	from := month
	usd, at, ok, err := p.store.SpendReconcile(spendAnthropic)
	if err != nil {
		return rep, err
	}
	// Last month's figure says nothing about this one.
	if ok && at >= month {
		from = at
		out.BilledUSD = &usd
		out.EstimatedUSD = usd
		out.Source = spendFromReconciled
		stale := (now.UnixMilli() - at) / 1000
		out.StaleSeconds = &stale
	}
	recorded, err := p.store.RecordedCostByHour(served, from, now.UnixMilli()+1)
	if err != nil {
		return rep, err
	}
	for _, hours := range recorded {
		for _, v := range hours {
			out.EstimatedUSD += v
		}
	}
	if budget > 0 {
		left := max(budget-out.EstimatedUSD, 0)
		out.RemainingUSD = &left
	}
	rep.Providers = append(rep.Providers, out)
	return rep, nil
}

// ReconcileSpend stores the real figure for the month. It also raises the budget
// counters to it, so a hard limit acts on the real spend. It can only raise them:
// see RaiseSpendFloor.
func (p *Proxy) ReconcileSpend(usd float64, now time.Time) error {
	if p.store == nil {
		return errors.New("no store")
	}
	if err := p.store.SetSpendReconcile(spendAnthropic, usd, now.UnixMilli()); err != nil {
		return err
	}
	_, _, scopes := claudeBackends(p.config())
	for _, s := range scopes {
		p.RaiseSpendFloor(s, "month", usd)
	}
	return nil
}

// CreateSpendToken mints a cost token and returns it, once, with its id. A zero
// ttl never expires.
func (p *Proxy) CreateSpendToken(label string, ttl time.Duration, now time.Time) (string, int64, error) {
	if p.store == nil {
		return "", 0, errors.New("no store")
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	token := SpendTokenPrefix + hex.EncodeToString(raw)
	var expires int64
	if ttl > 0 {
		expires = now.Add(ttl).UnixMilli()
	}
	id, err := p.store.CreateSpendToken(spendTokenHash(token), label, now.UnixMilli(), expires)
	return token, id, err
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
