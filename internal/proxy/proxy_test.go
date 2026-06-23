package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// mkConfig builds a one-model config whose backend pure-proxies (no cmd) to the
// given upstream base URL.
func mkConfig(t *testing.T, served, upstream string) *config.Config {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Models: map[string]config.Model{
			served: {Backends: []config.Backend{{Proxy: pn, Type: "local", Quality: 100}}},
		},
	}
}

func TestInferencePassthroughAndActivityLog(t *testing.T) {
	// Mock OpenAI upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"mock"`) {
				t.Errorf("upstream did not receive body: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hi"}}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := mkConfig(t, "mock", upstream.URL)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	// Fire a chat completion through corrallm.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Errorf("unexpected proxied body: %s", rec.Body.String())
	}

	// Activity was logged.
	acts, err := st.RecentActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	if acts[0].Served != "mock" || acts[0].Status != http.StatusOK || acts[0].Path != "/v1/chat/completions" {
		t.Errorf("activity = %+v", acts[0])
	}
}

// TestBackpressure429 holds the single slot with one in-flight request and
// asserts a concurrent second request is rejected with 429 + backoff headers.
func TestBackpressure429(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-release // hold the slot
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	defer close(release)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	// First request occupies the only slot.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock"}`))
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started

	// Second request: backend at capacity, default group → reject → 429.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock"}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-RateLimit-Capacity") != "1" {
		t.Errorf("missing backoff headers: %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"rejected"`) {
		t.Errorf("body lacks reason: %s", rec.Body.String())
	}
}

func backendTo(t *testing.T, urlStr, typ string) config.Backend {
	t.Helper()
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Backend{Proxy: pn, Type: typ}
}

func TestOrderBackends(t *testing.T) {
	bs := []config.Backend{{Type: "local"}, {Type: "local"}, {Type: "cloud"}}
	cases := []struct {
		rr   uint64
		want []int
	}{
		{0, []int{0, 1, 2}},
		{1, []int{1, 0, 2}}, // rr rotates within the local type; cloud stays last
		{2, []int{0, 1, 2}},
	}
	for _, c := range cases {
		got := orderBackends(bs, c.rr)
		if len(got) != len(c.want) {
			t.Fatalf("rr=%d len %d", c.rr, len(got))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("rr=%d: got %v want %v", c.rr, got, c.want)
				break
			}
		}
	}
}

// TestFallThroughSpill: the first (local) backend is saturated with a spill
// stage, so a concurrent request advances to the second (cloud) backend.
func TestFallThroughSpill(t *testing.T) {
	rel := make(chan struct{})
	started := make(chan struct{})
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		close(started)
		<-rel // hold the local slot
		_, _ = w.Write([]byte(`{"served_by":"up1"}`))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"up2"}`))
	}))
	defer up2.Close()
	// Registered last → runs first (LIFO): unblock the held request before the
	// servers Close (which waits for in-flight connections).
	defer close(rel)

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": {Backends: []config.Backend{backendTo(t, up1.URL, "local"), backendTo(t, up2.URL, "cloud")}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"g": {Weight: 1, OnSaturated: map[string]config.Stage{"local": {Spill: true}}},
		},
		Keys: map[string]string{"k": "g"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", "k")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	go send() // occupies the local slot, blocks in up1
	<-started

	rec := send() // local saturated → spill → served by cloud up2
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"up2"`) {
		t.Errorf("expected spill to up2, got: %s", rec.Body.String())
	}
}

// TestExhaustedAllSpill: every backend spills → terminal 429 with reason
// "exhausted".
func TestExhaustedAllSpill(t *testing.T) {
	rel := make(chan struct{})
	started := make(chan struct{}, 1)
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-rel
	}))
	defer block.Close()
	defer close(rel) // runs before block.Close() (LIFO) to release the held request

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": {Backends: []config.Backend{backendTo(t, block.URL, "local")}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"g": {Weight: 1, OnSaturated: map[string]config.Stage{"local": {Spill: true}}},
		},
		Keys: map[string]string{"k": "g"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", "k")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	go send()
	<-started

	rec := send() // only backend saturated + spill, nothing else → exhausted 429
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"exhausted"`) {
		t.Errorf("expected reason exhausted, got: %s", rec.Body.String())
	}
}

// TestPreemptionServesHigherGroup: a low, interruptible group holds the only
// slot mid-request; a higher group with a preempt stage cancels it and is served
// once the slot frees. The preempted upstream request observes ctx cancellation.
func TestPreemptionServesHigherGroup(t *testing.T) {
	started := make(chan struct{})
	rel := make(chan struct{}) // unblocks the held upstream handler for clean shutdown
	var once sync.Once
	first := true
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			once.Do(func() { close(started) })
			// Hold the slot until preempted (proxy aborts on ctx cancel, freeing
			// the slot) or the test ends. The slot frees independent of this
			// handler returning, so `rel` is only for tidy server teardown.
			select {
			case <-r.Context().Done():
			case <-rel:
			}
			return
		}
		_, _ = w.Write([]byte(`{"served_by":"hi"}`))
	}))
	defer upstream.Close()
	defer close(rel) // runs before upstream.Close() (LIFO) so no connection lingers

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := &config.Config{
		Models: map[string]config.Model{
			"m": {Backends: []config.Backend{backendTo(t, upstream.URL, "local")}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"lo": {Weight: 1, Interruptible: true,
				OnSaturated: map[string]config.Stage{"default": {Reject: true}}},
			"hi": {Weight: 10,
				OnSaturated: map[string]config.Stage{"local": {Preempt: true}}},
		},
		Keys: map[string]string{"lokey": "lo", "hikey": "hi"},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	send := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("X-Corrallm-Key", key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	go send("lokey") // occupies the only slot, blocks in upstream
	<-started

	rec := send("hikey") // preempts the low request → served
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for preempting request, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"served_by":"hi"`) {
		t.Errorf("expected hi to be served after preemption, got: %s", rec.Body.String())
	}
}

func TestUnknownModel404(t *testing.T) {
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(&config.Config{Models: map[string]config.Model{}}, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ghost"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown model, got %d", rec.Code)
	}
}

func TestModelsCatalog(t *testing.T) {
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	r := chi.NewRouter()
	New(mkConfig(t, "mock", "http://127.0.0.1:1"), proc.NewManager(&config.Config{}), sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mock"`) {
		t.Fatalf("models catalog: %d %s", rec.Code, rec.Body.String())
	}
}
