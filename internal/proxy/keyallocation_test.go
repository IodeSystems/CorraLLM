package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// A caller's hard allocation (P56) is charged its metered cost and, once
// full, refuses that caller with 429 allocation_exhausted — while another
// caller on the same model is served.
func TestKeyAllocationRefusesOnlyThatCaller(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := meteredConfig(t, "mock", upstream.URL) // $0.00105 a request
	cfg.Keys = map[string]config.KeyPolicy{
		"capped": {Budgets: config.KeyBudgets{"*": {{USD: 0.001, Per: "calendar-day", Hard: true}}}},
		"free":   {},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mock"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("capped"); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	rec := call("capped")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "allocation_exhausted") {
		t.Fatalf("over allocation: %d %s, want 429 allocation_exhausted", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the refusal")
	}
	if rec := call("free"); rec.Code != http.StatusOK {
		t.Errorf("another caller: %d %s, want served", rec.Code, rec.Body.String())
	}
}
