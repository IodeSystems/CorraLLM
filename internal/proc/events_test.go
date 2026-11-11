package proc

import (
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
)

type capturedEvents struct{ events []BackendEvent }

func (c *capturedEvents) RecordBackendEvent(ev BackendEvent) { c.events = append(c.events, ev) }

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
	if len(c.events) != 1 {
		t.Fatalf("events = %+v, want one", c.events)
	}
	e := c.events[0]
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
	if len(c.events) != 1 {
		t.Fatalf("events = %+v, want only the stop", c.events)
	}
	if e := c.events[0]; e.Kind != EventStop || e.Reason != "evicted-for:qwen" || !e.OK {
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
	if len(c.events) != 0 {
		t.Fatalf("recorded before the drain finished: %+v", c.events)
	}
	m.releaser(p)()
	if len(c.events) != 1 || c.events[0].Reason != "refresh" {
		t.Fatalf("events = %+v, want one stop with reason refresh", c.events)
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
