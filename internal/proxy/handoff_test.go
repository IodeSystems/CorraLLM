package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// handoffRig is refusalRig with the pieces a handoff test drives directly.
func handoffRig(t *testing.T, models map[string]config.Model) (http.Handler, *Proxy, *sched.Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	cfg := &config.Config{
		Models: models,
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1,
			OnSaturated: map[string]config.Stage{"default": {Queue: true}}}},
		Keys: map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	sc := sched.New()
	px := New(cfg, mgr, sc, st)
	r := chi.NewRouter()
	px.Mount(r)
	return r, px, sc, st
}

// successor records what a forwarded request looked like when it arrived.
type successor struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []*http.Request
	bod []string
}

func newSuccessor(t *testing.T) *successor {
	s := &successor{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, r)
		s.bod = append(s.bod, string(b))
		s.mu.Unlock()
		w.Header().Set(PidHeader, "777")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from successor"}}]}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// After the handover, a request arriving here is served by the successor —
// with the body and caller address it arrived with — and leaves no row here:
// the successor writes the one row.
func TestHandoff_ArrivingRequestIsForwarded(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the draining process served a request itself")
	}))
	t.Cleanup(up.Close)
	h, px, sc, st := handoffRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")})
	succ := newSuccessor(t)
	sc.Drain()
	if err := px.SetSuccessor(succ.srv.URL); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Corrallm-Key", "k")
	req.RemoteAddr = "10.1.2.3:4444"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from successor") {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if pids := rec.Header().Values(PidHeader); len(pids) != 1 || pids[0] != "777" {
		t.Errorf("pid header %v, want only the successor's", pids)
	}
	if len(succ.got) != 1 || succ.bod[0] != body {
		t.Fatalf("successor got %d requests, body %q", len(succ.got), succ.bod)
	}
	g := succ.got[0]
	if g.Header.Get("X-Corrallm-Key") != "k" || g.Header.Get(HandoffHeader) == "" || g.Header.Get("X-Real-IP") != "10.1.2.3" {
		t.Errorf("forwarded headers: key %q handoff %q real-ip %q", g.Header.Get("X-Corrallm-Key"),
			g.Header.Get(HandoffHeader), g.Header.Get("X-Real-IP"))
	}
	if rows, markers := counts(t, st); rows != 0 || markers != 0 {
		t.Errorf("draining process wrote %d rows, %d markers; the successor owns the row", rows, markers)
	}
}

// A request queued for a 1-slot backend when the handover happens goes to the
// successor; the one already being served finishes here with its own row.
func TestHandoff_QueuedRequestIsForwarded(t *testing.T) {
	treatAsRemote(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from old"}}]}`))
	}))
	t.Cleanup(up.Close)
	m := modelTo(t, up.URL, "chat")
	m.MaxConcurrent = 1
	h, px, sc, st := handoffRig(t, map[string]config.Model{"m": m})
	succ := newSuccessor(t)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post(h, "m") }()
	<-started
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(h, "m") }()
	// Wait until the second is queued behind the first.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := sc.Snapshot(); len(snap.Backends) == 1 && snap.Backends[0].Waiting == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := px.SetSuccessor(succ.srv.URL); err != nil {
		t.Fatal(err)
	}
	sc.Drain()
	select {
	case rec := <-second:
		if !strings.Contains(rec.Body.String(), "from successor") {
			t.Fatalf("queued request: %d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request was not handed over")
	}
	close(release)
	if rec := <-first; !strings.Contains(rec.Body.String(), "from old") {
		t.Fatalf("in-flight request: %d %q", rec.Code, rec.Body.String())
	}
	if rows, markers := counts(t, st); rows != 1 || markers != 0 {
		t.Errorf("%d rows, %d markers; want the in-flight request's one row", rows, markers)
	}
}

// A request already forwarded once is not forwarded again (no loop).
func TestHandoff_NoDoubleForward(t *testing.T) {
	treatAsRemote(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"served here"}}]}`))
	}))
	t.Cleanup(up.Close)
	h, px, _, _ := handoffRig(t, map[string]config.Model{"m": modelTo(t, up.URL, "chat")})
	succ := newSuccessor(t)
	_ = px.SetSuccessor(succ.srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Corrallm-Key", "k")
	req.Header.Set(HandoffHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if len(succ.got) != 0 || !strings.Contains(rec.Body.String(), "served here") {
		t.Errorf("forwarded a forwarded request: %d %q", len(succ.got), rec.Body.String())
	}
}
