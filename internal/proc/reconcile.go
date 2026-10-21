package proc

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
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

// adoptHealthBudget bounds the health check that gates adopting a reported
// backend. It runs inside the heartbeat handler, so it must be short; a backend
// that is not healthy within it is retried on the next beat.
const adoptHealthBudget = 3 * time.Second

// ReconcileAgent settles what an agent reports running against what this
// primary believes is resident on that server.
//
// Four cases, and each has exactly one right answer:
//
//   - the agent has a backend whose key matches a live Process → ADOPTED
//     already: nothing to do. This is what makes a brief control-plane outage
//     cost nothing instead of a full cold load.
//   - the agent has a backend nothing here claims, and it is EXACTLY what the
//     current config would spawn for that key (same served model and server,
//     same configured cmd, same port, healthy) → ADOPT it: rebuild the Process
//     as ready and charge its pools. This is what lets a model survive a
//     PRIMARY restart — the old primary leaves agent backends running (see
//     Shutdown) and this one picks them up from the first heartbeat.
//   - the agent has a backend nothing here claims and that does not match → REAP.
//     The model was removed or its cmd changed, it is a duplicate of one we
//     hold, or it is a Process we already wrote off. An untracked process
//     holding tens of GB is the worst state in the system: nothing will ever
//     reap it and the next spawn dies with an allocation failure.
//   - this primary holds a Process the agent does NOT report → the backend is
//     gone (the agent restarted without adopting it, or it exited while we
//     could not hear). Free its pools, or that server's budget stays spoken
//     for by nothing.
//
// Returns counts for logging. Safe to call on every heartbeat: adoption is a
// no-op in the steady state.
func (m *Manager) ReconcileAgent(ctx context.Context, server string, reported []agent.Backend) (adopted, reaped, vanished int) {
	if server == "" {
		return 0, 0, 0
	}
	// Whatever happens below, this server has now reported: spawns held since
	// startup may proceed (and will find anything adopted here).
	defer m.markReported(server)
	now := time.Now()

	reportedKeys := make(map[string]bool, len(reported))
	for _, b := range reported {
		if b.Alive && !b.Exited {
			reportedKeys[b.Key] = true
		}
	}

	// Ours, by key, on this server — with the agent id each one is attached
	// to, when known, so a second backend under the same key is recognised as
	// the duplicate it is rather than counted as ours.
	m.mu.Lock()
	ourIDs := map[string]string{}
	var gone []*Process
	for key, p := range m.procs {
		if p.server != server {
			continue
		}
		p.mu.Lock()
		h := p.handle
		p.mu.Unlock()
		id := ""
		if a, ok := h.(interface{ AgentID() string }); ok {
			id = a.AgentID()
		}
		ourIDs[key] = id
		if !reportedKeys[key] {
			gone = append(gone, p)
		}
	}
	m.mu.Unlock()

	for _, b := range reported {
		if id, ours := ourIDs[b.Key]; ours && (id == "" || id == b.ID) {
			adopted++
			continue
		}
		if !b.Alive || b.Exited {
			continue // already dead; nothing to adopt or reap
		}
		if _, ours := ourIDs[b.Key]; !ours {
			if p := m.adoptReported(server, b); p != nil {
				ourIDs[b.Key] = b.ID
				adopted++
				continue
			}
		}
		// Grace: the primary may be mid-spawn and not yet hold this key.
		if b.Started > 0 && now.Sub(time.UnixMilli(b.Started)) < adoptionGrace {
			continue
		}
		rh, ok := m.hostFor(server).(*agent.RemoteHost)
		if !ok {
			continue
		}
		slog.Warn("reaping backend on agent — nothing here claims it, or it is not what config would run",
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

// adoptReported registers a backend the agent is running and this primary did
// not start, IF it is exactly what the current config would spawn under that
// key. Returns the new Process, or nil when it must not be adopted — which
// sends the caller down the reap path.
//
// Every check here errs toward nil. Wrongly adopting means serving traffic
// from a process running a command nobody configured any more; wrongly
// declining costs one cold load.
func (m *Manager) adoptReported(server string, b agent.Backend) *Process {
	cfg := m.config()
	if cfg == nil {
		return nil
	}
	served, mdl, pl, ok := configuredFor(cfg, server, b.Key)
	if !ok {
		slog.Info("not adopting agent backend: no configured model has this key here",
			"server", server, "id", b.ID, "key", b.Key)
		return nil
	}
	switch {
	case b.ConfigCmd == "":
		// Started by a primary or agent older than adoption: nothing to
		// compare against, so nothing to vouch for it.
		slog.Info("not adopting agent backend: it carries no configured cmd to verify",
			"server", server, "id", b.ID, "key", b.Key)
		return nil
	case b.ConfigCmd != mdl.Cmd:
		slog.Info("not adopting agent backend: its model's cmd has changed in config",
			"server", server, "id", b.ID, "key", b.Key)
		return nil
	case b.Port == 0 || b.Port != backendPort(mdl):
		slog.Info("not adopting agent backend: its port is not the configured one",
			"server", server, "id", b.ID, "key", b.Key, "port", b.Port, "want", backendPort(mdl))
		return nil
	case mdl.Disabled:
		return nil
	}
	if _, paused := m.pauseByKey(b.Key); paused {
		return nil // out of service by order: it should not be running at all
	}
	target, err := cfg.TargetFor(served, mdl)
	if err != nil || target == nil {
		return nil
	}
	// Running is not serving. A backend left half-loaded, or wedged, is not
	// adopted; if it stays that way past the grace it is reaped.
	if err := m.waitHealthyFor(target, nil, adoptHealthBudget); err != nil {
		slog.Info("not adopting agent backend yet: not healthy", "server", server, "id", b.ID, "key", b.Key, "err", err)
		return nil
	}
	rh, ok := m.hostFor(server).(*agent.RemoteHost)
	if !ok {
		return nil
	}

	logs := newLogBuffer(500)
	h, err := rh.Attach(b.ID, logs)
	if err != nil {
		slog.Warn("not adopting agent backend: cannot attach", "server", server, "id", b.ID, "err", err)
		return nil
	}
	now := time.Now()
	p := &Process{
		Name:       served,
		ModelName:  served,
		key:        b.Key,
		placement:  pl.Name,
		Target:     target,
		remote:     mdl.Remote(),
		server:     mdl.Server,
		persistent: mdl.Persistent,
		evictRank:  evictRank(mdl.Sticky),
		ttl:        stickyTTL(mdl.Sticky),
		idleUnload: stickyIdleUnload(mdl.Sticky),
		logs:       logs,
		state:      StateReady,
		ready:      make(chan struct{}),
		handle:     h,
		spawnedCmd: b.Cmd,
		// In-memory state starts fresh: the idle clock, refs and residency
		// anchor all describe THIS primary's view, which begins now.
		readyAt:  now,
		lastUsed: now,
	}
	if n := parallelOf(b.Cmd); n > 0 && n != mdl.Slots() {
		p.tunedSlots = n
	}
	close(p.ready)

	m.mu.Lock()
	if m.procs[b.Key] != nil || m.stopping[b.Key] != nil {
		// Lost a race with a spawn or an unload of the same key. Not ours to
		// adopt after all; detach and let the reap path have it.
		m.mu.Unlock()
		stopWatching(h)
		return nil
	}
	p.usage = m.effectiveUsage(served, mdl)
	m.reserveLocked(mdl.Server, p.usage)
	m.procs[b.Key] = p
	m.mu.Unlock()

	go func() {
		<-h.Done()
		slog.Info("backend exited", "name", served, "err", h.Err())
		m.onProcExit(served, p)
	}()
	go m.sampleVRAMPeak(served, h)
	// Re-measure as a spawn would. Delayed: the handle's first poll has not
	// filled in the memory reading yet, and measuring "unknown" records nothing.
	go func() {
		select {
		case <-h.Done():
			return
		case <-time.After(3 * time.Second):
		}
		m.measure(served, mdl, p, h)
	}()
	go m.probeUI(p)
	slog.Info("adopted a running backend from its agent",
		"server", server, "id", b.ID, "key", b.Key, "model", served, "target", target.URL.String())
	return p
}

// stopWatching ends a remote handle's poller without signalling the backend.
// Only meaningful for handles this package created and then abandoned.
func stopWatching(h host.Handle) {
	if s, ok := h.(interface{ Detach() }); ok {
		s.Detach()
	}
}

// configuredFor finds the served model and placement whose process key on
// server is key, as EnsureReady would compute it. Sorted by served name so an
// extension (one key, several served models) always resolves the same way.
func configuredFor(cfg *config.Config, server, key string) (string, config.Model, config.Placement, bool) {
	names := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, served := range names {
		eff, ok := cfg.Effective(served)
		if !ok {
			continue
		}
		for _, pl := range eff.PlacementList() {
			mdl := eff.ForPlacement(pl)
			if mdl.Server != server || mdl.Cmd == "" {
				continue
			}
			if mdl.PlacementProcKey(served, pl) == key {
				return served, mdl, pl, true
			}
		}
	}
	return "", config.Model{}, config.Placement{}, false
}

// backendPort is the port a spawned model's backend serves on, from its
// configured proxy target (before any agent rewriting). 0 when it has none.
func backendPort(mdl config.Model) int {
	t, err := mdl.ProxyTarget()
	if err != nil || t == nil || t.URL == nil {
		return 0
	}
	n, _ := strconv.Atoi(t.URL.Port())
	return n
}

// parallelOf reads `--parallel N` from a spawned command, 0 when absent.
func parallelOf(cmd string) int {
	m := reParallel.FindString(cmd)
	if m == "" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(m, "--parallel")))
	return n
}

// agentBacked reports whether server's backends run under an agent.
func (m *Manager) agentBacked(server string) bool {
	if server == "" {
		return false
	}
	cfg := m.config()
	if cfg == nil {
		return false
	}
	srv, ok := cfg.Servers[server]
	return ok && srv.Agent != nil
}

// reportChLocked returns server's first-report channel. Caller holds m.mu.
func (m *Manager) reportChLocked(server string) chan struct{} {
	if m.reported == nil {
		m.reported = map[string]chan struct{}{}
	}
	ch, ok := m.reported[server]
	if !ok {
		ch = make(chan struct{})
		m.reported[server] = ch
	}
	return ch
}

// markReported releases spawns held on server's first report. Idempotent.
func (m *Manager) markReported(server string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := m.reportChLocked(server)
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// awaitAgentReport holds a spawn onto an agent-backed server until that
// agent's first heartbeat has been reconciled, or until agentHoldTimeout after
// this primary started.
//
// Right after a restart this primary knows nothing about what its agents are
// running; their backends outlived the previous primary on purpose. Spawning
// before the first report would start a SECOND copy of a model that is
// already loaded — on a GPU sized for one — and the first copy would then be
// reaped as a duplicate. Holding costs at most one heartbeat interval in the
// normal case. A request that arrives in that window waits here, exactly as it
// would wait for a load, instead of failing.
//
// Only while an agent can be expected: with no liveness tracker (single host,
// tests) there is nothing that will ever report, and after the timeout an
// agent that has not reported is presumed down — spawning then fails or
// succeeds on its own merits, as it always did.
func (m *Manager) awaitAgentReport(ctx context.Context, server string) error {
	if m.live == nil || !m.agentBacked(server) {
		return nil
	}
	wait := time.Until(m.born.Add(agentHoldTimeout))
	if wait <= 0 {
		return nil
	}
	m.mu.Lock()
	ch := m.reportChLocked(server)
	m.mu.Unlock()
	select {
	case <-ch:
		return nil
	default:
	}
	slog.Info("holding spawn until the agent reports what it is running",
		"server", server, "atMost", wait.Round(time.Second))
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-t.C:
		slog.Warn("agent has not reported since this primary started; spawning without its report",
			"server", server, "waited", agentHoldTimeout)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
