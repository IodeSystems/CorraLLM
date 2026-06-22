package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

	mgr := proc.NewManager()
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
	mgr := proc.NewManager()
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

func TestUnknownModel404(t *testing.T) {
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager()
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
	New(mkConfig(t, "mock", "http://127.0.0.1:1"), proc.NewManager(), sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mock"`) {
		t.Fatalf("models catalog: %d %s", rec.Code, rec.Body.String())
	}
}
