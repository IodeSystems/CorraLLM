package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/alert"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/store"
)

const testKey = "fw-test-secret-key"

// fakeFireworks serves /v1/accounts and a two-page usageCosts:query.
type fakeFireworks struct {
	mu         sync.Mutex
	incomplete bool
	pageTokens []string // pageToken of each usageCosts call, in order
	badKey     int
	body       map[string]any
}

func (f *fakeFireworks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		f.mu.Lock()
		f.badKey++
		f.mu.Unlock()
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts":
		_, _ = io.WriteString(w, `{"accounts":[{"name":"accounts/acme"}]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/accounts/acme/usageCosts:query":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.body = body
		tok, _ := body["pageToken"].(string)
		f.pageTokens = append(f.pageTokens, tok)
		incomplete := f.incomplete
		f.mu.Unlock()
		comp := "COMPLETE"
		if incomplete {
			comp = "INCOMPLETE"
		}
		if tok == "" {
			// units as a string, nanos as an int: $0.40 + $0.0125 on one hour.
			_, _ = io.WriteString(w, `{"attributionCompleteness":"`+comp+`","nextPageToken":"p2","rows":[
			 {"dimensions":{"model":"accounts/fireworks/models/glm-5p3-flash","startTime":"2026-10-20T10:00:00Z"},
			  "subtotal":{"currencyCode":"USD","units":"0","nanos":400000000}},
			 {"dimensions":{"model":"accounts/fireworks/models/glm-5p3-flash","startTime":"2026-10-20T11:00:00Z"},
			  "subtotal":{"currencyCode":"USD","units":"1","nanos":12500000}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"attributionCompleteness":"`+comp+`","rows":[
		 {"dimensions":{"model":"accounts/fireworks/models/deepseek-v4p1-flash","startTime":"2026-10-05T09:00:00Z"},
		  "subtotal":{"currencyCode":"USD","units":"2","nanos":0}},
		 {"dimensions":{"model":"accounts/fireworks/models/other","startTime":"2026-10-05T09:00:00Z"},
		  "subtotal":{"currencyCode":"EUR","units":"9","nanos":0}}]}`)
	default:
		http.NotFound(w, r)
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("FW_TEST_KEY", testKey)
	cfg, err := config.LoadBytesForTest([]byte(`
extensions:
  fw:
    providers:
      fireworks:
        proxy: {host: 127.0.0.1, port: 443}
        credentials:
          - name: default
            headers: {authorization: "Bearer ${FW_TEST_KEY}"}
            limits: [{usd: 5, per: calendar-day, hard: true}, {usd: 50, per: calendar-month, hard: true}]
        provides:
          glm: {type: chat, upstream: accounts/fireworks/models/glm-5p3-flash}
          ds: {type: chat, upstream: accounts/fireworks/models/deepseek-v4p1-flash}
  other:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443}
        provides:
          llama: {type: chat, upstream: llama}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type fixture struct {
	r      *Reconciler
	st     *store.Store
	ledger *quota.Ledger
	fake   *fakeFireworks
	ntfy   *ntfyCapture
	cfg    *config.Config
}

type ntfyCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (n *ntfyCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	n.mu.Lock()
	n.msgs = append(n.msgs, r.Header.Get("Title")+"\n"+string(b))
	n.mu.Unlock()
}

func (n *ntfyCapture) count() int {
	// the notifier posts in a goroutine
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.msgs)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := &fakeFireworks{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	nt := &ntfyCapture{}
	nsrv := httptest.NewServer(nt)
	t.Cleanup(nsrv.Close)

	cfg := testConfig(t)
	ledger := quota.New()
	ledger.SetLimits("cred:fireworks/default", []config.Limit{
		{USD: 5, Per: "calendar-day", Hard: true}, {USD: 50, Per: "calendar-month", Hard: true}})

	fw := NewFireworks()
	fw.Host, fw.BaseURL = "127.0.0.1", srv.URL
	r := New(st, func() *config.Config { return cfg }, ledgerAdapter{ledger}, alert.New(nsrv.URL))
	r.Providers = []Provider{fw}
	r.Now = func() time.Time { return time.Date(2026, 10, 20, 13, 30, 0, 0, time.UTC) }
	return &fixture{r: r, st: st, ledger: ledger, fake: fake, ntfy: nt, cfg: cfg}
}

type ledgerAdapter struct{ l *quota.Ledger }

func (a ledgerAdapter) RaiseSpendFloor(scope, unit string, usd float64) float64 {
	return a.l.RaiseCalendarFloor(scope, config.DimUSD, unit, usd)
}

func usedOf(l *quota.Ledger, label string) float64 {
	for _, e := range l.Snapshot() {
		for _, w := range e.Windows {
			if w.Label == label {
				return w.Used
			}
		}
	}
	return -1
}

func (f *fixture) record(t *testing.T, served string, at time.Time, usd float64) {
	t.Helper()
	if _, err := f.st.DB().Exec(`INSERT INTO activity (ts, served, path, status, cost_usd) VALUES (?,?,?,?,?)`,
		at.UnixMilli(), served, "/v1/chat/completions", 200, usd); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePaginatesParsesMoneyMapsModelsAndRaisesTheLedger(t *testing.T) {
	f := newFixture(t)
	f.fake.incomplete = true
	// corrallm metered only part of the bill: $0.22 of $0.4125 on glm's hour,
	// nothing for deepseek's early-month hour.
	f.record(t, "fireworks-glm", time.Date(2026, 10, 20, 10, 20, 0, 0, time.UTC), 0.22)
	f.record(t, "fireworks-glm", time.Date(2026, 10, 20, 11, 20, 0, 0, time.UTC), 0.01)
	f.record(t, "fw-glm", time.Date(2026, 10, 20, 11, 20, 0, 0, time.UTC), 0.05)

	f.r.RunOnce(context.Background())

	if got := f.fake.pageTokens; len(got) != 2 || got[0] != "" || got[1] != "p2" {
		t.Fatalf("page tokens = %q, want [\"\" \"p2\"]", got)
	}
	if got := f.fake.body["scope"]; got != "SELF" {
		t.Errorf("scope = %v", got)
	}
	if from, _ := f.fake.body["startTime"].(string); from != "2026-10-01T00:00:00Z" {
		t.Errorf("startTime = %q, want the month start", from)
	}

	rows, err := f.st.BillingRows(0)
	if err != nil {
		t.Fatal(err)
	}
	var billed, recorded float64
	byKey := map[string]store.BillingRow{}
	for _, r := range rows {
		billed += r.BilledUSD
		recorded += r.RecordedUSD
		byKey[NormModel(r.Upstream)+"@"+time.UnixMilli(r.HourMS).UTC().Format("01-02T15")] = r
		if !r.Incomplete {
			t.Errorf("row %+v: want incomplete (the provider said INCOMPLETE)", r)
		}
	}
	// 0.40 + 1.0125 + 2.00; the EUR row is skipped.
	if billed < 3.4124 || billed > 3.4126 {
		t.Errorf("billed total = %v, want 3.4125 (units+nanos, EUR dropped)", billed)
	}
	g := byKey["glm-5p3-flash@10-20T11"]
	if g.BilledUSD < 1.0124 || g.BilledUSD > 1.0126 {
		t.Errorf("glm hour 11 billed = %v, want 1.0125", g.BilledUSD)
	}
	// Only the served names mapped to that upstream count; "fw-glm" is
	// not a configured model and must not be attributed.
	if g.RecordedUSD != 0.01 || g.Served != "fireworks-glm" {
		t.Errorf("glm hour 11 recorded=%v served=%q, want 0.01 / fireworks-glm", g.RecordedUSD, g.Served)
	}
	if recorded < 0.2299 || recorded > 0.2301 {
		t.Errorf("recorded total = %v, want 0.23", recorded)
	}

	// The ledger window floors rose to the bill: month = everything, day = the
	// two 10-20 hours.
	if got := usedOf(f.ledger, "usd/calendar-month"); got < 3.4124 || got > 3.4126 {
		t.Errorf("month window = %v, want 3.4125", got)
	}
	if got := usedOf(f.ledger, "usd/calendar-day"); got < 1.4124 || got > 1.4126 {
		t.Errorf("day window = %v, want 1.4125", got)
	}

	fetches, _ := f.st.BillingFetches()
	if len(fetches) != 1 || fetches[0].Error != "" || fetches[0].OKMS == 0 || !fetches[0].Incomplete {
		t.Errorf("fetch status = %+v", fetches)
	}
}

func TestReconcileNeverLowersTheLedgerAndIgnoresUnrelatedProviders(t *testing.T) {
	f := newFixture(t)
	// Recorded spend already above the bill (a hour not settled yet).
	f.ledger.Charge("cred:fireworks/default", config.DimUSD, 10)
	f.r.Now = func() time.Time { return time.Date(2026, 10, 20, 13, 30, 0, 0, time.UTC) }
	f.r.RunOnce(context.Background())
	if got := usedOf(f.ledger, "usd/calendar-month"); got != 10 {
		t.Errorf("month window = %v, want 10 (a lower bill must not lower it)", got)
	}
	if f.fake.badKey != 0 {
		t.Errorf("%d requests carried the wrong key", f.fake.badKey)
	}
	// groq is not recognised: exactly one fetch status row, for fireworks.
	fetches, _ := f.st.BillingFetches()
	if len(fetches) != 1 || fetches[0].Provider != "fireworks" {
		t.Errorf("fetches = %+v", fetches)
	}
}

func TestReconcileFailureKeepsRowsAndRecordsTheError(t *testing.T) {
	f := newFixture(t)
	f.r.RunOnce(context.Background())
	// Break the key by changing the secret the credential resolves.
	t.Setenv("FW_TEST_KEY", "wrong")
	f.r.RunOnce(context.Background())
	// The account cache is keyed by the credential, so the new key re-lists and 401s.
	fetches, _ := f.st.BillingFetches()
	if len(fetches) != 1 || !strings.Contains(fetches[0].Error, "HTTP 401") {
		t.Fatalf("fetches = %+v, want a 401 error", fetches)
	}
	if strings.Contains(fetches[0].Error, "wrong") || strings.Contains(fetches[0].Error, testKey) {
		t.Errorf("error leaks the key: %q", fetches[0].Error)
	}
	if fetches[0].OKMS == 0 {
		t.Error("the earlier success time was lost")
	}
	if rows, _ := f.st.BillingRows(0); len(rows) == 0 {
		t.Error("rows were dropped by a failed fetch")
	}
}

func TestDriftThreshold(t *testing.T) {
	for _, c := range []struct {
		billed, recorded float64
		want             bool
	}{
		{0.72, 0.22, true},
		{0.26, 0.22, false},  // $0.04 under the $0.05 floor
		{0.28, 0.22, true},   // $0.06
		{100.0, 96.0, false}, // $4 is under 5% of 96 = $4.80
		{102.0, 96.0, true},  // $6
		{0.10, 0.50, false},  // the bill is lower: not our alert
	} {
		if _, over := Drift(c.billed, c.recorded); over != c.want {
			t.Errorf("Drift(%v,%v) = %v, want %v", c.billed, c.recorded, over, c.want)
		}
	}
}

func TestDriftAlertsOncePerEpisode(t *testing.T) {
	f := newFixture(t)
	f.r.RunOnce(context.Background()) // billed $3.41 vs recorded $0: month and day drift
	if n := f.ntfy.count(); n != 1 {
		t.Fatalf("after first run: %d messages, want 1 (month and day share one message)", n)
	}
	f.ntfy.mu.Lock()
	msg := f.ntfy.msgs[0]
	f.ntfy.mu.Unlock()
	for _, want := range []string{"month 2026-10: billed $3.41, recorded $0.00", "day 2026-10-20: billed $1.41"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}

	// Hourly reruns while the drift persists stay quiet, even past the hour key.
	f.r.Now = func() time.Time { return time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC) }
	f.r.RunOnce(context.Background())
	if n := f.ntfy.count(); n != 1 {
		t.Fatalf("persisting drift: %d messages, want still 1", n)
	}

	// Metering catches up: the episode ends, and a new drift is news again.
	f.record(t, "fireworks-glm", time.Date(2026, 10, 20, 10, 5, 0, 0, time.UTC), 3.5)
	f.r.RunOnce(context.Background())
	if len(f.r.drifting) != 0 {
		t.Fatalf("episodes after catching up = %v, want none", f.r.drifting)
	}
}

func TestMoney(t *testing.T) {
	for _, c := range []struct {
		m    Money
		want float64
	}{
		{Money{Units: "0", Nanos: 400000000}, 0.4},
		{Money{Units: "12", Nanos: 340000000}, 12.34},
		{Money{Units: "", Nanos: 1}, 1e-9},
		{Money{Units: "-1", Nanos: -500000000}, -1.5},
	} {
		got, err := c.m.USD()
		if err != nil || got < c.want-1e-12 || got > c.want+1e-12 {
			t.Errorf("%+v = %v (%v), want %v", c.m, got, err, c.want)
		}
	}
	if _, err := (Money{Units: "x"}).USD(); err == nil {
		t.Error("bad units: want an error")
	}
}

func TestNormModel(t *testing.T) {
	if NormModel("accounts/fireworks/models/GLM-5p3-Flash") != "glm-5p3-flash" || NormModel("llama") != "llama" {
		t.Error("NormModel")
	}
}
