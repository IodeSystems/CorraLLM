package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// gateway is a fake upstream gateway: a catalogue, and a chat endpoint that
// records the model ids it was asked for.
type gateway struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
	hold  chan struct{} // when set, chat waits on it
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[
				{"id":"vendor/priced","pricing":{"prompt":"0.000001","completion":"0.000004"}},
				{"id":"vendor/unpriced"}
			]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.asked = append(g.asked, body.Model)
		hold := g.hold
		g.mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`)
	}))
	t.Cleanup(g.Close)
	return g
}

func passthroughRig(t *testing.T, g *gateway, provider string) (*Proxy, http.Handler) {
	t.Helper()
	u, _ := url.Parse(g.URL)
	y := fmt.Sprintf(`
extensions:
  corp:
    providers:
      gw:
        proxy: %s
%s        passthrough:
          template: {maxConcurrent: 1}
`, u.Port(), provider)
	cfg, err := config.LoadBytesForTest([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PriorityGroups = map[string]config.PriorityGroup{"default": {Weight: 1}}
	cfg.Keys = map[string]config.KeyPolicy{"k": {Group: "default"}}
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	p := New(cfg, mgr, sched.NewWithConfig(cfg), st)
	r := chi.NewRouter()
	p.Mount(r)
	if !p.HasRosterRefresh() {
		t.Fatal("a passthrough provider must start the refresh loop")
	}
	p.RefreshRoster(context.Background())
	return p, r
}

func chat(h http.Handler, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPassthroughServesACatalogueModelUnderItsOwnId(t *testing.T) {
	g := newGateway(t)
	_, h := passthroughRig(t, g, "")
	if rec := chat(h, "gw-vendor-priced"); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(g.asked) != 1 || g.asked[0] != "vendor/priced" {
		t.Errorf("upstream asked for %v, want the catalogue id", g.asked)
	}
	if rec := chat(h, "gw-vendor-not-listed"); rec.Code == 200 {
		t.Errorf("an id outside the catalogue was served: %d", rec.Code)
	}
	if len(g.asked) != 1 {
		t.Errorf("an id outside the catalogue reached the upstream: %v", g.asked)
	}
}

func TestPassthroughChargesTheCataloguePrice(t *testing.T) {
	g := newGateway(t)
	p, h := passthroughRig(t, g, "")
	if rec := chat(h, "gw-vendor-priced"); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rows, err := p.store.RecentActivity(store.Window{}, 10, "", "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	// 1M prompt at $1/M + 1M completion at $4/M
	if rows[0].CostUSD < 4.99 || rows[0].CostUSD > 5.01 {
		t.Errorf("cost = %v, want $5 from the catalogue's prices", rows[0].CostUSD)
	}
}

func TestPassthroughLeavesOutUnpricedRowsUnderAUSDLimit(t *testing.T) {
	g := newGateway(t)
	p, _ := passthroughRig(t, g, "        limits: [{usd: 50, per: calendar-month}]\n")
	got := p.config().Discovered()
	if _, ok := got["gw-vendor-priced"]; !ok {
		t.Error("the priced row is missing")
	}
	if _, ok := got["gw-vendor-unpriced"]; ok {
		t.Error("an unpriced row was served on a provider with a USD limit")
	}
}

func TestProviderCapHoldsAcrossItsModels(t *testing.T) {
	g := newGateway(t)
	g.hold = make(chan struct{})
	p, h := passthroughRig(t, g, "        maxConcurrent: 1\n")
	first := make(chan int)
	go func() { first <- chat(h, "gw-vendor-priced").Code }()
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.asked) == 1 })

	// another model, with a free slot of its own: the provider is full
	rec := chat(h, "gw-vendor-unpriced")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d (Retry-After %q), want 429 from the provider cap", rec.Code, rec.Header().Get("Retry-After"))
	}
	promises, err := p.store.RetryPromises(store.Window{}, 10, "")
	if err != nil || len(promises) != 1 || promises[0].RefusedBy != config.ProviderGateKey("corp", "gw") {
		t.Errorf("refused by %+v (err %v), want the provider gate", promises, err)
	}
	// the gate is not a backend in the load views
	for _, b := range p.sched.Snapshot().Backends {
		if strings.HasPrefix(b.Backend, config.ProviderGatePrefix) {
			t.Errorf("gate %q shows as a backend", b.Backend)
		}
	}

	close(g.hold)
	if code := <-first; code != 200 {
		t.Fatalf("first request: %d", code)
	}
	if rec := chat(h, "gw-vendor-unpriced"); rec.Code != 200 {
		t.Errorf("after the first finished: %d, want the gate slot back", rec.Code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
