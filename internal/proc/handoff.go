package proc

import (
	"context"
	"errors"
	"log/slog"
)

// Blue-green handoff, the residency side.
//
// The old process (draining) keeps what it has loaded for the requests it
// already admitted and touches nothing else: no spawn, no eviction to make
// room, no idle sweep, no agent reconcile. Agent-hosted backends stay up for
// the successor, which adopts them from the agent (slice A). LOCAL backends
// are the old process's children and cannot be adopted: each is stopped as
// soon as it has no request in flight, and whatever is left goes with
// Shutdown when the drain ends. The successor cold-spawns them.
//
// The new process holds every local spawn until the old one holds no local
// backend (HoldLocalSpawns), so the GPU never carries two copies of a model
// — or one model plus the memory of another the old process has not freed.

// ErrDraining refuses a spawn in a process that has handed over. The caller
// forwards the request to the successor.
var ErrDraining = errors.New("proc: draining; this process has handed over to its successor")

// onAgent reports whether h is a backend an agent supervises.
func onAgent(h interface{}) bool {
	_, ok := h.(interface{ AgentID() string })
	return ok
}

// Drain enters handoff mode and stops every local backend that is idle now.
// The rest stop as their last request releases them.
func (m *Manager) Drain() {
	m.draining.Store(true)
	m.releaseIdleLocal()
}

// Draining reports whether Drain has been called.
func (m *Manager) Draining() bool { return m.draining.Load() }

// releaseIdleLocal evicts every local backend with nothing in flight, so the
// successor can load onto its GPU without waiting for the whole drain.
func (m *Manager) releaseIdleLocal() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		p.mu.Lock()
		h, idle, ready := p.handle, p.refs == 0, p.state == StateReady
		p.mu.Unlock()
		if h == nil || onAgent(h) || !idle || !ready {
			continue
		}
		slog.Info("handoff: stopping idle local backend so the successor can load it", "name", p.Name)
		m.evictLocked(p)
	}
}

// LocalBackends counts this process's local backends that may still hold
// memory: loaded, loading, or stopping but not yet confirmed gone. What a
// draining process reports to its successor.
func (m *Manager) LocalBackends() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.stoppingLocal)
	for _, p := range m.procs {
		p.mu.Lock()
		h, st := p.handle, p.state
		p.mu.Unlock()
		switch {
		case h != nil && !onAgent(h):
			n++
		case h == nil && st == StateLoading && !m.agentBacked(p.server) && p.logs != nil:
			n++ // spawning: no handle yet, but about to hold memory
		}
	}
	return n
}

// HoldLocalSpawns makes every local spawn wait until ReleaseLocalSpawns. For a
// successor whose predecessor still runs local backends.
func (m *Manager) HoldLocalSpawns() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.localHold == nil {
		m.localHold = make(chan struct{})
	}
}

// ReleaseLocalSpawns ends HoldLocalSpawns. Idempotent.
func (m *Manager) ReleaseLocalSpawns() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.localHold != nil {
		close(m.localHold)
		m.localHold = nil
	}
}

// awaitLocalHold blocks a local spawn of key while the hold stands. A backend
// already resident under key needs no spawn and is not held.
func (m *Manager) awaitLocalHold(ctx context.Context, key string) error {
	m.mu.Lock()
	h, resident := m.localHold, m.procs[key] != nil
	m.mu.Unlock()
	if h == nil || resident {
		return nil
	}
	slog.Info("holding local spawn until the previous process has released the GPU", "key", key)
	select {
	case <-h:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
