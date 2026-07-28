package proc

import (
	"context"
	"log/slog"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
)

// adoptionGrace protects a backend the agent has only just started from being
// reaped as an orphan.
//
// Registration is not simultaneous on both sides: the primary creates its
// Process, then the agent starts the group, then the agent's next heartbeat
// reports it. A heartbeat landing inside that window would show a backend the
// primary "does not know about" and kill something it deliberately spawned
// seconds earlier.
const adoptionGrace = 60 * time.Second

// ReconcileAgent settles what an agent reports running against what this
// primary believes is resident on that server.
//
// Three cases, and each has exactly one right answer:
//
//   - the agent has a backend whose key matches a live Process → ADOPT: it is
//     already ours, nothing to do. This is what makes a brief control-plane
//     outage cost nothing instead of a full cold load.
//   - the agent has a backend nothing here claims → REAP. It is an orphan,
//     from a previous primary or a Process we already wrote off. An untracked
//     process holding tens of GB is the worst state in the system: nothing
//     will ever reap it and the next spawn dies with an allocation failure.
//   - this primary holds a Process the agent does NOT report → the backend is
//     gone (the agent restarted, or it exited while we could not hear). Free
//     its pools, or that server's budget stays spoken for by nothing.
//
// Returns counts for logging. Safe to call on every heartbeat: adoption is a
// no-op in the steady state.
func (m *Manager) ReconcileAgent(ctx context.Context, server string, reported []agent.Backend) (adopted, reaped, vanished int) {
	if server == "" {
		return 0, 0, 0
	}
	now := time.Now()

	reportedKeys := make(map[string]bool, len(reported))
	for _, b := range reported {
		reportedKeys[b.Key] = true
	}

	// Ours, by key, on this server.
	m.mu.Lock()
	ourKeys := map[string]bool{}
	var gone []*Process
	for key, p := range m.procs {
		if p.server != server {
			continue
		}
		ourKeys[key] = true
		if !reportedKeys[key] {
			gone = append(gone, p)
		}
	}
	m.mu.Unlock()

	for _, b := range reported {
		if ourKeys[b.Key] {
			adopted++
			continue
		}
		// Grace: the primary may be mid-spawn and not yet hold this key.
		if b.Started > 0 && now.Sub(time.UnixMilli(b.Started)) < adoptionGrace {
			continue
		}
		if !b.Alive {
			continue // already dead; nothing to reap
		}
		rh, ok := m.hostFor(server).(*agent.RemoteHost)
		if !ok {
			continue
		}
		slog.Warn("reaping orphaned backend on agent — nothing here claims it",
			"server", server, "id", b.ID, "key", b.Key, "model", b.Model)
		if err := rh.KillBackend(ctx, b.ID); err != nil {
			slog.Error("failed to reap orphan", "server", server, "id", b.ID, "err", err)
			continue
		}
		reaped++
	}

	for _, p := range gone {
		slog.Warn("backend vanished from its agent; freeing its pools",
			"server", server, "name", p.Name, "key", p.key)
		m.onProcExit(p.Name, p)
		vanished++
	}
	return adopted, reaped, vanished
}
