package proxy

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
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
