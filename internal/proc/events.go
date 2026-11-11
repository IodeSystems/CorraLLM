package proc

import (
	"errors"
	"time"

	"github.com/iodesystems/corrallm/internal/host"
)

// Backend lifecycle events (P75): every start, adoption and stop, with why.
//
// model_load (loadrecord.go) has one writer, the cold spawn, so it has no row
// for an adopted backend, a crash, or a stop and its reason. The prompt-cache
// classifier needs all three: a restart empties the cache, and a miss after
// one is a reload, not an eviction. Instance is the handle id, which an
// adoption keeps (pgid locally, endpoint#id on an agent): an adopted backend
// is the same process, its cache intact, and must not read as a reload.

// Event kinds.
const (
	EventStart = "start"
	EventAdopt = "adopt"
	EventStop  = "stop"
)

// BackendEvent is one lifecycle event.
type BackendEvent struct {
	TS       int64 // unix ms; set by recordEvent when zero
	Model    string
	Server   string
	Instance string
	Kind     string
	// Reason: cold-spawn, adopt, evicted-for:<model>, idle-unload, unload-api,
	// unload-all, pause:<why>, refresh, handoff, vanished, crash:<exit err>.
	Reason string
	OK     bool
	Err    string
}

// EventRecorder is given each lifecycle event. Implementations must not
// block, for the reason LoadRecorder must not.
type EventRecorder interface {
	RecordBackendEvent(BackendEvent)
}

// SetEventRecorder attaches one. nil disables recording.
func (m *Manager) SetEventRecorder(r EventRecorder) { m.eventRecorder = r }

func (m *Manager) recordEvent(ev BackendEvent) {
	if m.eventRecorder == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	m.eventRecorder.RecordBackendEvent(ev)
}

// event records one event for handle h; nothing without a recorder or a
// handle. err marks it failed.
func (m *Manager) event(model, server string, h host.Handle, kind, reason string, err error) {
	if m.eventRecorder == nil || h == nil {
		return
	}
	ev := BackendEvent{Model: model, Server: server, Instance: h.ID(), Kind: kind, Reason: reason, OK: err == nil}
	if err != nil {
		ev.Err = err.Error()
	}
	m.recordEvent(ev)
}

// Instance names the backend process serving p: its handle id, "" while
// nothing runs (a pure proxy, or a spawn still in flight).
func (p *Process) Instance() string {
	if h := p.handleOf(); h != nil {
		return h.ID()
	}
	return ""
}

func (p *Process) handleOf() host.Handle {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handle
}

func (p *Process) setStopReason(r string) {
	p.mu.Lock()
	p.stopReason = r
	p.mu.Unlock()
}

// recordExit runs when a backend's process group is gone. A stop corrallm
// asked for was recorded by evictLocked; anything else is a crash.
func (m *Manager) recordExit(p *Process, h host.Handle) {
	p.mu.Lock()
	ordered := p.stopped
	p.stopped = true // once
	p.mu.Unlock()
	if ordered || m.eventRecorder == nil {
		return
	}
	reason, err := "crash", h.Err()
	if err != nil {
		reason = "crash:" + err.Error()
	} else {
		err = errors.New("exited on its own")
	}
	m.event(p.ModelName, p.server, h, EventStop, reason, err)
}
