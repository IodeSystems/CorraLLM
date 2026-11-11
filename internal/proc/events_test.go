package proc

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
)

type capturedEvents struct {
	mu     sync.Mutex
	events []BackendEvent
}

func (c *capturedEvents) RecordBackendEvent(ev BackendEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

// settle waits for at least n events (they arrive on the recorder's own
// goroutine), then a moment more for any extra, and returns them.
func (c *capturedEvents) settle(t *testing.T, n int) []BackendEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.events)
		c.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]BackendEvent(nil), c.events...)
}

// eventHandle is a backend whose group has exited with err.
type eventHandle struct {
	host.Handle
	id   string
	err  error
	done chan struct{}
}

func (h eventHandle) ID() string              { return h.id }
func (h eventHandle) Err() error              { return h.err }
func (h eventHandle) Done() <-chan struct{}   { return h.done }
func (h eventHandle) Signal(host.Sig) error   { return nil }
func (h eventHandle) Alive() bool             { return false }
func (h eventHandle) MemoryMiB() (int, error) { return 0, errors.New("n/a") }

func eventMgr() (*Manager, *capturedEvents) {
	m := NewManager(&config.Config{})
	c := &capturedEvents{}
	m.SetEventRecorder(c)
	return m, c
}

// A process that exits with no stop asked for crashed, and says how.
func TestBackendEventCrash(t *testing.T) {
	m, c := eventMgr()
	h := eventHandle{id: "pgid:7", err: errors.New("exit status 139"), done: make(chan struct{})}
	p := &Process{Name: "swift", ModelName: "swift", key: "swift", server: "box1", handle: h}
	m.recordExit(p, h)
	evs := c.settle(t, 1)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	e := evs[0]
	if e.Kind != EventStop || e.Reason != "crash:exit status 139" || e.OK || e.Instance != "pgid:7" || e.TS == 0 {
		t.Errorf("event = %+v", e)
	}
}

// An ordered stop records its reason once; the exit that follows is not a
// crash.
func TestBackendEventEvictionReason(t *testing.T) {
	m, c := eventMgr()
	h := eventHandle{id: "pgid:8", done: make(chan struct{})}
	close(h.done)
	p := &Process{Name: "swift", ModelName: "swift", key: "swift", server: "box1", handle: h}
	m.mu.Lock()
	m.procs["swift"] = p
	p.setStopReason("evicted-for:qwen")
	m.evictLocked(p)
	m.mu.Unlock()
	m.recordExit(p, h)
	evs := c.settle(t, 1)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want only the stop", evs)
	}
	if e := evs[0]; e.Kind != EventStop || e.Reason != "evicted-for:qwen" || !e.OK {
		t.Errorf("event = %+v", e)
	}
}

// unloadKey carries its reason through a drain: the stop is recorded when
// the last request releases, with the reason the unload gave.
func TestBackendEventDrainKeepsReason(t *testing.T) {
	m, c := eventMgr()
	h := eventHandle{id: "pgid:9", done: make(chan struct{})}
	close(h.done)
	p := &Process{Name: "swift", ModelName: "swift", key: "swift", server: "box1", handle: h,
		state: StateReady, refs: 1, ready: make(chan struct{})}
	m.mu.Lock()
	m.procs["swift"] = p
	m.mu.Unlock()
	if _, err := m.unloadKey("swift", "swift", true, "refresh"); err != nil {
		t.Fatal(err)
	}
	if evs := c.settle(t, 0); len(evs) != 0 {
		t.Fatalf("recorded before the drain finished: %+v", evs)
	}
	m.releaser(p)()
	if evs := c.settle(t, 1); len(evs) != 1 || evs[0].Reason != "refresh" {
		t.Fatalf("events = %+v, want one stop with reason refresh", evs)
	}
}

// The instance is the handle id and nothing else: an adopted backend gets a
// handle with the agent's id and its own start time, which the primary that
// spawned it never saw. Were the load time part of the instance, every
// adoption after a handoff would read as a reload.
func TestInstanceIsTheHandleID(t *testing.T) {
	spawned := &Process{handle: eventHandle{id: "http://a:1#b1"}, loadedAt: time.UnixMilli(1000)}
	adopted := &Process{handle: eventHandle{id: "http://a:1#b1"}, loadedAt: time.UnixMilli(1003)}
	if spawned.Instance() != adopted.Instance() || spawned.Instance() == "" {
		t.Errorf("spawned %q adopted %q", spawned.Instance(), adopted.Instance())
	}
	if (&Process{}).Instance() != "" {
		t.Error("a process with no handle has no instance")
	}
}

// stuckRecorder blocks every write until released, as a store write waiting
// out its busy timeout does.
type stuckRecorder struct {
	release chan struct{}
	got     chan BackendEvent
}

func (s stuckRecorder) RecordBackendEvent(ev BackendEvent) {
	<-s.release
	s.got <- ev
}

// A recorder that blocks never holds up the caller (evictLocked records
// with m.mu held), and a full buffer drops and counts rather than waits.
func TestEventDoesNotBlockOnARecorder(t *testing.T) {
	m := NewManager(&config.Config{})
	rec := stuckRecorder{release: make(chan struct{}), got: make(chan BackendEvent, eventBuffer+10)}
	m.SetEventRecorder(rec)
	h := eventHandle{id: "pgid:1", done: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		m.mu.Lock() // as evictLocked holds it
		for range eventBuffer + 5 {
			m.event("swift", "box1", h, EventStop, "idle-unload", nil)
		}
		m.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("m.event blocked behind a stuck recorder")
	}
	// One is in the recorder's hands (or about to be), the buffer holds
	// eventBuffer, and the rest were dropped.
	if d := m.EventsDropped(); d < 4 || d > 5 {
		t.Errorf("dropped = %d, want 4 or 5", d)
	}
	close(rec.release)
	want := eventBuffer + 5 - int(m.EventsDropped())
	for range want {
		select {
		case <-rec.got:
		case <-time.After(2 * time.Second):
			t.Fatal("buffered events never reached the recorder")
		}
	}
}
