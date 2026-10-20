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

// treatAsRemote makes loopback fakes count as remote APIs for one test.
func treatAsRemote(t *testing.T) {
	prev := isRemote
	isRemote = func(config.Model) bool { return true }
	t.Cleanup(func() { isRemote = prev })
}

func refusalRig(t *testing.T, models map[string]config.Model, lanes map[string]config.Lane) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	cfg := &config.Config{
		Models:         models,
		Lanes:          lanes,
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

func post(h http.Handler, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The only candidate's 429 reaches the caller, and carries a Retry-After even
// though the upstream sent none.
func TestRemote429WithoutRetryAfterGetsOne(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	t.Cleanup(up.Close)
	h, st := refusalRig(t, map[string]config.Model{"fw": modelTo(t, up.URL, "chat")}, nil)

	rec := post(h, "fw")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("Retry-After = %q, want 60 (the 1-minute fallback pause)", ra)
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(rows) != 1 || rows[0].Error != "upstream 429" || rows[0].RetryAfterMS <= 0 {
		t.Fatalf("rows = %+v, want one row marked upstream 429 with a retry-after", rows)
	}
}

// A 503 from the first candidate moves the request to the next one, and the
// refusal gets its own row.
func TestRemote503TriesTheNextCandidate(t *testing.T) {
	treatAsRemote(t)
	shed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(shed.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ok.Close)
	a, b := modelTo(t, shed.URL, "chat"), modelTo(t, ok.URL, "chat")
	a.Quality, b.Quality = 1, 1 // same tier: both in the walk, a first (round-robin starts at 0)
	h, st := refusalRig(t, map[string]config.Model{"a": a, "b": b},
		map[string]config.Lane{"chat": {Members: []config.LaneMember{{Model: "a"}, {Model: "b"}}}})

	rec := post(h, "chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 from the second candidate", rec.Code, rec.Body.String())
	}
	rows, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	var refused, served bool
	for _, r := range rows {
		if r.Served == "a" && r.Status == 503 && strings.Contains(r.Error, "tried the next model") {
			refused = true
		}
		if r.Served == "b" && r.Status == 200 {
			served = true
		}
	}
	if !refused || !served {
		t.Fatalf("rows = %+v, want a 503 row for a and a 200 row for b", rows)
	}
	// a stays paused: the next request goes straight to b.
	_ = post(h, "chat")
	rows, _ = st.RecentActivity(store.Window{}, 10, "", "", "")
	n503 := 0
	for _, r := range rows {
		if r.Status == 503 {
			n503++
		}
	}
	if n503 != 1 {
		t.Errorf("a was asked again during its 2s pause (%d refusals)", n503)
	}
}
