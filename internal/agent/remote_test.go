package agent

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// syncBuf is the manager's log sink: written by the poller, read by the test.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A real agent, a real client, over real HTTP. This is the round trip the whole
// multi-host design rests on, and it needs no second machine — a second agent
// on another port is the same code path the Mac will take.
func TestRemoteHost_RoundTrip(t *testing.T) {
	a := New("test", "tok")
	srv := httptest.NewServer(a.Routes())
	defer func() { a.Shutdown(); srv.Close() }()

	r := NewRemoteHost("mac1", []string{"http://127.0.0.1:1", srv.URL}, "tok")
	if r.Name() != "mac1" {
		t.Fatalf("Name = %q", r.Name())
	}

	out := &syncBuf{}
	h, err := r.Start(host.Spec{Name: "m", Cmd: "echo n_slots-marker; sleep 30", Out: out})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The banner must reach the primary's sink. This is what a model's tuning
	// profile is parsed out of; losing it costs the model its profile.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "n_slots-marker") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "n_slots-marker") {
		t.Fatalf("backend output never reached the sink; got %q", out.String())
	}

	for time.Now().Before(deadline) && !h.Alive() {
		time.Sleep(50 * time.Millisecond)
	}
	if !h.Alive() {
		t.Error("handle should report alive")
	}

	if err := h.Signal(host.SigTerm); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done never closed after the backend was signalled")
	}
	if h.Alive() {
		t.Error("handle still reports alive after exit")
	}
}

// The first endpoint is dead. An agent has several addresses at once and which
// one works depends on where the daemon sits, so a dead one must be skipped
// rather than failing the spawn.
func TestRemoteHost_FallsThroughToAReachableEndpoint(t *testing.T) {
	a := New("test", "")
	srv := httptest.NewServer(a.Routes())
	defer func() { a.Shutdown(); srv.Close() }()

	r := NewRemoteHost("mac1", []string{
		"http://127.0.0.1:1",    // nothing listening
		"http://192.0.2.1:6503", // TEST-NET-1, unroutable
		srv.URL,                 // the live one
	}, "")
	h, err := r.Start(host.Spec{Name: "m", Cmd: "sleep 5"})
	if err != nil {
		t.Fatalf("Start should have fallen through to the reachable endpoint: %v", err)
	}
	if !strings.HasPrefix(h.ID(), srv.URL) {
		t.Errorf("handle bound to %s, want the reachable endpoint %s", h.ID(), srv.URL)
	}
	_ = h.Signal(host.SigKill)
}

// Every endpoint dead must be a clear error naming what was tried — not a
// silent local spawn, and not a nil handle someone dereferences.
func TestRemoteHost_AllEndpointsDown(t *testing.T) {
	r := NewRemoteHost("mac1", []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, "")
	h, err := r.Start(host.Spec{Name: "m", Cmd: "sleep 1"})
	if err == nil {
		t.Fatal("want an error when no endpoint answers")
	}
	if h != nil {
		t.Error("want a nil handle on failure")
	}
	if !strings.Contains(err.Error(), "mac1") {
		t.Errorf("err = %v, want it to name the server", err)
	}
}

// A wrong token must fail loudly at spawn rather than looking like an outage.
func TestRemoteHost_WrongTokenFails(t *testing.T) {
	a := New("test", "right")
	srv := httptest.NewServer(a.Routes())
	defer func() { a.Shutdown(); srv.Close() }()

	r := NewRemoteHost("mac1", []string{srv.URL}, "wrong")
	if _, err := r.Start(host.Spec{Name: "m", Cmd: "sleep 1"}); err == nil {
		t.Fatal("want an error with a wrong token")
	}
}

// A machine that cannot attribute memory per process must surface an ERROR, so
// the primary can tell "cannot know" from "zero". Conflating them would let
// ramUsage be silently overridden by a bogus measurement of 0.
func TestRemoteHandle_UnmeasurableMemoryIsAnError(t *testing.T) {
	h := &remoteHandle{memMiB: -1, done: make(chan struct{})}
	if _, err := h.MemoryMiB(); err == nil {
		t.Error("want an error when the agent reports memory as unattributable")
	}
	h2 := &remoteHandle{memMiB: 4096, done: make(chan struct{})}
	if v, err := h2.MemoryMiB(); err != nil || v != 4096 {
		t.Errorf("MemoryMiB = %d,%v; want 4096,nil", v, err)
	}
}
