package proxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/metrics"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// spendRig is a proxy with one paid Claude model.
func spendRig(t *testing.T, target string, cfg func(*config.Config)) (*Proxy, *store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	c := &config.Config{
		Models:         map[string]config.Model{"claude": claudeModel(t, target, "")},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	if cfg != nil {
		cfg(c)
	}
	p := New(c, mgr, sched.New(), st)
	r := chi.NewRouter()
	p.Mount(r)
	return p, st, r
}

func getSpend(h http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/spend", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func spendRow(t *testing.T, st *store.Store, served string, usd float64, at time.Time) {
	t.Helper()
	if err := st.InsertActivity(store.Activity{TS: at.UnixMilli(), Served: served, Path: "/v1/chat/completions", Status: 200, CostUSD: usd}); err != nil {
		t.Fatal(err)
	}
}

func TestSpendIsTheLedgerAndSaysItIsAnEstimate(t *testing.T) {
	p, st, h := spendRig(t, "http://127.0.0.1:1", nil)
	now := time.Now()
	spendRow(t, st, "claude", 1.5, now.Add(-time.Minute))
	spendRow(t, st, "claude", 2.5, now.Add(-time.Second))
	spendRow(t, st, "local", 9, now.Add(-time.Second))
	token, _, err := p.CreateSpendToken("dash", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	rec := getSpend(h, token)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var rep SpendReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Providers) != 1 {
		t.Fatalf("report = %s", rec.Body)
	}
	got := rep.Providers[0]
	if got.Provider != "anthropic" || got.EstimatedUSD != 4 || !got.CostEstimated || got.Source != "estimate" {
		t.Errorf("provider = %+v", got)
	}
	// A figure corrallm does not have is absent: a missing budget is not $0 left.
	for _, field := range []string{"billed_usd", "remaining_usd", "stale_seconds"} {
		if strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s should be absent: %s", field, rec.Body)
		}
	}
}

func TestLastMonthDoesNotCount(t *testing.T) {
	p, st, _ := spendRig(t, "http://127.0.0.1:1", nil)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	spendRow(t, st, "claude", 7, time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	spendRow(t, st, "claude", 1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err := st.SetSpendReconcile("anthropic", 100, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Providers[0]
	if got.EstimatedUSD != 1 || got.BilledUSD != nil || got.Period != "2026-11" {
		t.Errorf("provider = %+v, last month's spend and figure must not count", got)
	}
}

func TestReconcileAnchorsTheEstimate(t *testing.T) {
	p, st, _ := spendRig(t, "http://127.0.0.1:1", nil)
	now := time.Now()
	spendRow(t, st, "claude", 3, now.Add(-time.Hour))
	if err := p.ReconcileSpend(10, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	spendRow(t, st, "claude", 2, now.Add(-time.Second))
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Providers[0]
	if got.BilledUSD == nil || *got.BilledUSD != 10 || got.EstimatedUSD != 12 {
		t.Fatalf("provider = %+v: want $10 real plus the $2 after it", got)
	}
	if !got.CostEstimated || got.Source != "reconciled" || got.StaleSeconds == nil {
		t.Errorf("provider = %+v", got)
	}
}

func TestRemainingIsAgainstTheCredentialsMonthlyLimit(t *testing.T) {
	budget := config.LimitSet{{USD: 100, Per: "calendar-month", Hard: true}, {USD: 5, Per: "calendar-day"}}
	cases := map[string]float64{"under": 40, "over": 150}
	for name, spent := range cases {
		t.Run(name, func(t *testing.T) {
			p, st, _ := spendRig(t, "http://127.0.0.1:1", func(c *config.Config) {
				m := c.Models["claude"]
				m.Extension, m.ProviderName = "x", "anthropic"
				c.Models["claude"] = m
				c.Extensions = map[string]config.Extension{"x": {Providers: map[string]config.Provider{"anthropic": {Limits: budget}}}}
			})
			now := time.Now()
			spendRow(t, st, "claude", spent, now.Add(-time.Second))
			rep, err := p.Spend(now)
			if err != nil {
				t.Fatal(err)
			}
			left := rep.Providers[0].RemainingUSD
			if left == nil || math.Abs(*left-max(100-spent, 0)) > 1e-9 {
				t.Errorf("remaining = %v with $%g spent of $100", left, spent)
			}
		})
	}
}

func TestACallerOwnSignInIsNotCorrallmSpend(t *testing.T) {
	p, st, _ := spendRig(t, "http://127.0.0.1:1", func(c *config.Config) {
		c.Models["claude"] = claudeModel(t, "http://127.0.0.1:1", "callerAuth: true\n")
	})
	now := time.Now()
	spendRow(t, st, "claude", 5, now.Add(-time.Second))
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Providers) != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestACostTokenOpensSpendAndNothingElse(t *testing.T) {
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, claudeReplyJSON)
	}))
	t.Cleanup(up.Close)
	treatAsRemote(t)
	p, _, h := spendRig(t, up.URL, nil) // the default admits a stranger
	now := time.Now()
	token, _, err := p.CreateSpendToken("", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if rec := getSpend(h, token); rec.Code != 200 {
		t.Fatalf("spend: %d %s", rec.Code, rec.Body)
	}
	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	for _, c := range []struct{ method, path, header string }{
		{http.MethodPost, "/v1/chat/completions", "Authorization"},
		{http.MethodPost, "/v1/chat/completions", "X-Corrallm-Key"},
		{http.MethodPost, "/v1/messages", "X-Api-Key"},
		{http.MethodGet, "/v1/models", "Authorization"},
		{http.MethodGet, "/v1/keys", "Authorization"},
		{http.MethodPost, "/v1/keys", "Authorization"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(body))
		v := token
		if c.header == "Authorization" {
			v = "Bearer " + token
		}
		req.Header.Set(c.header, v)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s via %s: status %d: %s", c.method, c.path, c.header, rec.Code, rec.Body)
		}
	}
	if hit {
		t.Error("a cost token reached the backend")
	}
}

func TestOnlyALiveCostTokenReadsSpend(t *testing.T) {
	p, st, h := spendRig(t, "http://127.0.0.1:1", nil)
	now := time.Now()
	expired, _, err := p.CreateSpendToken("", time.Minute, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	revoked, id, err := p.CreateSpendToken("", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RevokeSpendToken(id, now.UnixMilli()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	for name, token := range map[string]string{"none": "", "expired": expired, "revoked": revoked,
		"made up": "spend-00", "a key": "k"} {
		if rec := getSpend(h, token); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
}

func TestATokenIsStoredOnlyAsItsHash(t *testing.T) {
	p, st, _ := spendRig(t, "http://127.0.0.1:1", nil)
	token, _, err := p.CreateSpendToken("x", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UseSpendToken(token, time.Now().UnixMilli()); ok || err != nil {
		t.Errorf("the token itself was accepted as the stored value: %v %v", ok, err)
	}
	if ok, err := st.UseSpendToken(spendTokenHash(token), time.Now().UnixMilli()); !ok || err != nil {
		t.Errorf("its hash was not: %v %v", ok, err)
	}
}

// timedRig is a paid provider with a $10 hard monthly limit that renews on the 15th.
func timedRig(t *testing.T) (*Proxy, *store.Store, config.Candidate) {
	t.Helper()
	return limitRig(t, config.LimitSet{{USD: 10, Per: "calendar-month", Hard: true, Renews: 15}})
}

func limitRig(t *testing.T, limits config.LimitSet) (*Proxy, *store.Store, config.Candidate) {
	t.Helper()
	p, st, _ := spendRig(t, "http://127.0.0.1:1", func(c *config.Config) {
		m := c.Models["claude"]
		m.Extension, m.ProviderName = "x", "anthropic"
		c.Models["claude"] = m
		c.Extensions = map[string]config.Extension{"x": {Providers: map[string]config.Provider{"anthropic": {
			Limits:      limits,
			Credentials: []config.Credential{{Name: "default"}},
		}}}}
	})
	cands, ok := p.config().ResolveServed("claude")
	if !ok || len(cands) != 1 || cands[0].Credential == nil {
		t.Fatalf("candidates = %+v", cands)
	}
	return p, st, cands[0]
}

func TestTimedBudgetPaysPastTheProviderLimit(t *testing.T) {
	p, _, cand := timedRig(t)
	scope := config.ProviderScopeKey("anthropic")
	p.quota.Charge(scope, config.DimUSD, 10)
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("a full hard limit with no timed budget must refuse")
	}
	now := time.Now()
	if err := p.AddBudget("anthropic", 3, "1h", "", "", now); err != nil {
		t.Fatal(err)
	}
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 1 {
		t.Fatal("a timed budget must admit past the full limit")
	}
	p.grants.spend(p.store, scope, cand.Name, 3)
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("a spent timed budget must refuse again")
	}
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	if b := rep.Providers[0].Budgets; len(b) != 1 || b[0].SpentUSD != 3 || b[0].LeftUSD != 0 {
		t.Fatalf("budgets = %+v", b)
	}
	if err := p.EditBudget(b0(rep), BudgetEdit{Left: ptr(2.5)}, now); err != nil {
		t.Fatal(err)
	}
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 1 {
		t.Fatal("a budget corrected to $2.50 left must admit")
	}
	spendRow(t, p.store, "claude", 13, now.Add(-time.Second))
	rep, _ = p.Spend(now)
	if got := rep.Providers[0]; got.RemainingUSD == nil || *got.RemainingUSD != 2.5 {
		t.Fatalf("remaining = %+v", got.RemainingUSD)
	}
}

func b0(rep SpendReport) int64 { return rep.Providers[0].Budgets[0].ID }

func TestExpiredTimedBudgetDoesNotPay(t *testing.T) {
	p, _, cand := timedRig(t)
	p.quota.Charge(config.ProviderScopeKey("anthropic"), config.DimUSD, 10)
	now := time.Now()
	if err := p.AddBudget("anthropic", 3, "", now.Add(-time.Minute).Format(time.RFC3339), "", now); err != nil {
		t.Fatal(err)
	}
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("an expired timed budget must not admit")
	}
}

func TestTimedBudgetNeedsAHardLimitAndAnExpiry(t *testing.T) {
	p, _, _ := timedRig(t)
	now := time.Now()
	if err := p.AddBudget("anthropic", 3, "", "", "", now); err == nil {
		t.Error("no expiry: want an error")
	}
	if err := p.AddBudget("nobody", 3, "1h", "", "", now); err == nil {
		t.Error("no such provider: want an error")
	}
}

func TestReconcileLowersTheCounter(t *testing.T) {
	p, _, cand := timedRig(t)
	scope := config.ProviderScopeKey("anthropic")
	p.quota.Charge(scope, config.DimUSD, 10)
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("want refused while the estimate says $10")
	}
	if err := p.ReconcileSpend(4, time.Now()); err != nil {
		t.Fatal(err)
	}
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 1 {
		t.Fatal("a real figure of $4 must reopen the limit")
	}
	// The merge takes the higher level, so the store must have come down as well.
	p.quota.Resync()
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 1 {
		t.Fatal("a resync undid the correction")
	}
}

func TestAReloadAppliesWhatTheCLIStored(t *testing.T) {
	p, st, cand := timedRig(t)
	p.quota.Charge(config.ProviderScopeKey("anthropic"), config.DimUSD, 10)
	if err := st.SetSpendReconcile("anthropic", 1, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := AddProviderBudget(p.config(), st, "anthropic", 1, "1h", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	p.SetConfig(p.config())
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 1 {
		t.Fatal("reload: want the stored figure applied")
	}
	if !p.grants.usable(config.ProviderScopeKey("anthropic"), cand.Name) {
		t.Fatal("reload: want the stored timed budget in memory")
	}
}

func TestRenewingLimitReportsItsOwnPeriod(t *testing.T) {
	p, st, _ := timedRig(t)
	spendRow(t, st, "claude", 7, time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC))
	spendRow(t, st, "claude", 2, time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC))
	rep, err := p.Spend(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Providers[0]
	if got.Period != "2026-10-15" || got.EstimatedUSD != 2 || got.ResetsAt != "2026-11-15T00:00:00Z" {
		t.Fatalf("provider = %+v", got)
	}
}

func TestRefusalSaysTheBudgetAndItsDates(t *testing.T) {
	p, _, cand := timedRig(t)
	now := time.Now().UTC()
	scope := config.ProviderScopeKey("anthropic")
	if err := p.AddBudget("anthropic", 3, "", now.Add(48*time.Hour).Format(time.RFC3339), "", now); err != nil {
		t.Fatal(err)
	}
	p.quota.Charge(scope, config.DimUSD, 10)
	p.grants.spend(p.store, scope, cand.Name, 3)
	notes := p.budgetNotes([]string{scope})
	if len(notes) != 1 {
		t.Fatalf("notes = %+v", notes)
	}
	start := config.PeriodStart("month", 15, now)
	want := "the anthropic provider's limit (every key): budget of $10.00 created on " + start.Format("2006-01-02") +
		", expires on " + start.AddDate(0, 1, 0).Format("2006-01-02") + " 00:00 UTC; $10.00 spent; timed budget of $3.00 created on " +
		now.Format("2006-01-02") + ", expires on " + now.Add(48*time.Hour).Format("2006-01-02 15:04 UTC") + "; $3.00 spent"
	if notes[0].Summary != want {
		t.Errorf("summary:\n got %s\nwant %s", notes[0].Summary, want)
	}
}

func TestPacedDayRefusesAndTimedBudgetDoesNotPay(t *testing.T) {
	p, _, cand := limitRig(t, config.LimitSet{
		{USD: 10, Per: "calendar-month", Hard: true, Renews: 15},
		{Pace: true, Per: "calendar-day", Hard: true},
	})
	scope := config.ProviderScopeKey("anthropic")
	now := time.Now()
	allow := config.PacedAllowance(10, 0, 0, 15, now)
	p.quota.Charge(scope, config.DimUSD, allow+0.01)
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("today's share spent must refuse")
	}
	if err := p.AddBudget("anthropic", 3, "1h", "", "", now); err != nil {
		t.Fatal(err)
	}
	if kept, _ := p.filterByQuota([]config.Candidate{cand}); len(kept) != 0 {
		t.Fatal("a timed budget pays past the month, not past today's share")
	}
	notes := p.budgetNotes([]string{scope})
	if len(notes) != 1 || !strings.Contains(notes[0].Summary, "today's share of the month") {
		t.Fatalf("notes = %+v", notes)
	}
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	if pr := rep.Providers[0]; pr.TodayUSD == nil {
		t.Fatalf("report has no paced day: %+v", pr)
	}
}

func scrapeMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestSpendGaugesMirrorTheReport(t *testing.T) {
	p, st, _ := timedRig(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	rat := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := st.SetSpendReconcile("anthropic", 3, rat.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	spendRow(t, st, "claude", 2, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	metrics.SetSpend(SpendMetrics(rep))
	body := scrapeMetrics(t)
	// the exposition writes floats as strconv 'g'
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	unix := func(t time.Time) string { return g(float64(t.Unix())) }
	for _, want := range []string{
		`corrallm_spend_estimated_usd{provider="anthropic"} 5`,
		`corrallm_spend_limit_usd{provider="anthropic"} 10`,
		`corrallm_spend_remaining_usd{provider="anthropic"} 5`,
		`corrallm_spend_resets_at_seconds{provider="anthropic"} ` + unix(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)),
		`corrallm_spend_period_start_seconds{provider="anthropic"} ` + unix(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)),
		`corrallm_spend_reconciled_at_seconds{provider="anthropic"} ` + unix(rat),
		`corrallm_spend_reconciled{provider="anthropic"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestSpendGaugesLeaveOutWhatIsNotKnown(t *testing.T) {
	p, st, _ := spendRig(t, "http://127.0.0.1:1", nil)
	now := time.Now()
	spendRow(t, st, "claude", 1, now.Add(-time.Second))
	rep, err := p.Spend(now)
	if err != nil {
		t.Fatal(err)
	}
	metrics.SetSpend(SpendMetrics(rep))
	body := scrapeMetrics(t)
	if !strings.Contains(body, `corrallm_spend_reconciled{provider="anthropic"} 0`) || !strings.Contains(body, `corrallm_spend_estimated_usd{provider="anthropic"} 1`) {
		t.Errorf("want estimate-only series:\n%s", body)
	}
	// no limit is not $0 left, and no real figure has no time
	for _, absent := range []string{"corrallm_spend_limit_usd{", "corrallm_spend_remaining_usd{", "corrallm_spend_resets_at_seconds{", "corrallm_spend_reconciled_at_seconds{"} {
		if strings.Contains(body, absent) {
			t.Errorf("%s should be absent:\n%s", absent, body)
		}
	}
}
