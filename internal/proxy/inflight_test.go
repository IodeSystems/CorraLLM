package proxy

import (
	"context"
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

// A request must be visible in the live registry WHILE it runs (the activity log
// only sees it once it is over) and gone once it returns.
func TestInflightVisibleDuringRequestAndClearedAfter(t *testing.T) {
	hold := make(chan struct{})
	arrived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		close(arrived)
		<-hold // block the upstream so the request is measurably in flight
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	p := New(mkConfig(t, "mock", upstream.URL), mgr, sched.New(), st)
	r := chi.NewRouter()
	p.Mount(r)

	if got := p.Inflight(); len(got) != 0 {
		t.Fatalf("idle proxy reports %d in flight", len(got))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Corrallm-Key", "k1")
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-arrived
	live := p.Inflight()
	if len(live) != 1 {
		t.Fatalf("in-flight = %d, want 1", len(live))
	}
	e := live[0]
	if e.Served != "mock" || e.Backend != "mock" {
		t.Errorf("served/backend = %q/%q, want mock/mock", e.Served, e.Backend)
	}
	if e.State != inflightStreaming {
		t.Errorf("state = %q, want %q", e.State, inflightStreaming)
	}
	if e.Key != "k1" || e.Path != "/v1/chat/completions" {
		t.Errorf("key/path = %q/%q", e.Key, e.Path)
	}

	close(hold)
	<-done

	// The registry drains on the way out — a stale entry would report permanent
	// phantom load.
	deadline := time.Now().Add(2 * time.Second)
	for len(p.Inflight()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("in-flight not cleared after the request finished: %+v", p.Inflight())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
