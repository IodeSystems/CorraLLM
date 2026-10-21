package proc

import (
	"context"
	"errors"
	"log/slog"
	"sort"
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
//
// The same holds for an AGENT spawn onto a server the old process ran
// locally: a server moved under an agent beside the primary (local → agent in
// the new config) puts the successor's spawn on the very GPU the old
// process's children still occupy. The old process says which servers its
// local children run on (LocalBackendsOn); until it has, every agent
// spawn is held, because any of them could be that server.

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
	n, _ := m.LocalBackendsOn()
	return n
}

// LocalBackendsOn is LocalBackends plus the servers, sorted, those backends
// run on — read together, so the two agree. The list is never nil: empty is
// the answer "none", which a successor must be able to tell from a
// predecessor too old to send one.
func (m *Manager) LocalBackendsOn() (int, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.stoppingLocal)
	seen := map[string]bool{}
	for _, server := range m.stoppingLocal {
		seen[server] = true
	}
	for _, p := range m.procs {
		p.mu.Lock()
		h, st := p.handle, p.state
		p.mu.Unlock()
		switch {
		case h != nil && !onAgent(h):
			n++
			seen[p.server] = true
		case h == nil && st == StateLoading && !m.agentBacked(p.server) && p.logs != nil:
			n++ // spawning: no handle yet, but about to hold memory
			seen[p.server] = true
		}
	}
	servers := make([]string, 0, len(seen))
	for s := range seen {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	return n, servers
}

// HoldLocalSpawns makes every local spawn — and every agent spawn, until
// SetHoldServers narrows that — wait until ReleaseLocalSpawns. For a successor
// whose predecessor still runs local backends.
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
	m.holdServers = nil
}

// SetHoldServers records which servers the predecessor's local children run
// on (handoff.Status.LocalServers). While the hold stands, an agent spawn onto
// one of them waits with the local ones; an agent spawn onto any other server
// proceeds. nil — a predecessor that does not report it — keeps every agent
// spawn held, as before the first report.
func (m *Manager) SetHoldServers(servers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if servers == nil {
		m.holdServers = nil
		return
	}
	m.holdServers = make(map[string]bool, len(servers))
	for _, s := range servers {
		m.holdServers[s] = true
	}
}

// awaitLocalHold blocks a spawn of key onto server while the hold stands: a
// local spawn always, an agent spawn when server is one the predecessor runs
// local children on, or when it has not said which (see SetHoldServers). A
// backend already resident under key needs no spawn and is not held.
//
// An agent spawn waits for the WHOLE hold, not for that server's children
// alone: the predecessor reports a count, and a per-server release would need
// a per-server count for a gain only a multi-box primary moving one box under
// an agent would see.
func (m *Manager) awaitLocalHold(ctx context.Context, key, server string) error {
	m.mu.Lock()
	h, resident := m.localHold, m.procs[key] != nil
	held := !m.agentBacked(server) || m.holdServers == nil || m.holdServers[server]
	m.mu.Unlock()
	if h == nil || resident || !held {
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
