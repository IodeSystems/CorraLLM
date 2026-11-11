package proc

import (
	"errors"
	"log/slog"
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

// EventRecorder is given each lifecycle event, in order, on one goroutine of
// its own. It may block (a database write): the manager never waits for it.
type EventRecorder interface {
	RecordBackendEvent(BackendEvent)
}

// eventBuffer is how many events may wait for a slow recorder before new
// ones are dropped. Events are a handful per hour; a full buffer means the
// recorder is stuck.
const eventBuffer = 1024

// SetEventRecorder attaches one, and the goroutine that feeds it. nil
// disables recording. Call it once, at setup.
//
// ASYNCHRONOUS ON PURPOSE. evictLocked records its stop with m.mu held, and
// the store is one connection with a 10 s busy timeout: a write waiting
// behind a prune or a long read would hold the manager lock — and every
// EnsureReady behind it — for as long. So the event goes into a buffer and
// the caller moves on; a full buffer drops the event and counts it.
func (m *Manager) SetEventRecorder(r EventRecorder) {
	if r == nil {
		m.eventRecorder, m.eventCh = nil, nil
		return
	}
	ch := make(chan BackendEvent, eventBuffer)
	m.eventRecorder, m.eventCh = r, ch
	go func() {
		for ev := range ch {
			r.RecordBackendEvent(ev)
		}
	}()
}

// EventsDropped is how many events a full buffer has dropped.
func (m *Manager) EventsDropped() int64 { return m.eventsDropped.Load() }

func (m *Manager) recordEvent(ev BackendEvent) {
	if m.eventCh == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	select {
	case m.eventCh <- ev:
	default:
		if n := m.eventsDropped.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("backend event dropped: the recorder is not keeping up", "model", ev.Model, "kind", ev.Kind, "dropped", n)
		}
	}
}

// event records one event for handle h; nothing without a recorder or a
// handle. err marks it failed.
func (m *Manager) event(model, server string, h host.Handle, kind, reason string, err error) {
	if m.eventCh == nil || h == nil {
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
	if ordered || m.eventCh == nil {
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

// RunningInstance is a ready backend process, for pollers that read it.
type RunningInstance struct {
	Model    string
	Instance string
	BaseURL  string
}

// RunningInstances lists the ready backends this manager runs a process
// for (a handle and a target; pure proxies are not ours to read).
func (m *Manager) RunningInstances() []RunningInstance {
	m.mu.Lock()
	procs := make([]*Process, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()
	var out []RunningInstance
	for _, p := range procs {
		p.mu.Lock()
		h, st, tg := p.handle, p.state, p.Target
		p.mu.Unlock()
		if h == nil || st != StateReady || tg == nil || tg.URL == nil {
			continue
		}
		out = append(out, RunningInstance{Model: p.ModelName, Instance: h.ID(), BaseURL: tg.URL.String()})
	}
	return out
}
