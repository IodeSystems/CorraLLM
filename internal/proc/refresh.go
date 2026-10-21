package proc

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Non-ECC refresh.
//
// Memory without ECC can flip a bit, and a flip in loaded weights stays until
// the model reloads. So a spawned backend whose memory includes a pool without
// ECC (ecc.go) and whose weights were loaded longer ago than
// residency.nonEccMaxAge (default 48h) is reloaded. The reload reads the
// weights from disk: every spawn drops the model files from the page cache
// first (host.EvictPageCache), so a flip in a cached page is not copied back.
//
// The reload is graceful. The backend drains — in-flight requests finish — and
// a request that arrives meanwhile WAITS for the replacement (EnsureReady)
// instead of being refused. Then the model is loaded again through the same
// path a benchmark uses to put a model back (reloadAfter).
//
// A refresh is deferred, never skipped: while a bench runs against the model,
// a handoff is in progress, or the backend is mid-load or already draining,
// the next tick tries again.

// refreshInterval is how often the refresh looks. The rule is measured in
// days; looking every few seconds would buy nothing.
const refreshInterval = 10 * time.Minute

// refreshDrainWait bounds how long a refresh waits for its drain before it
// stops waiting to reload. The drain itself goes on; a request that arrives
// later still loads the model. A long generation on box1 has taken 9 minutes.
var refreshDrainWait = 30 * time.Minute

// refreshPoll is how often a refresh, and a request waiting on one, checks
// whether the old process has gone. A var for tests.
var refreshPoll = 250 * time.Millisecond

// refreshRequester is what the model-load record shows as the cause of a
// refresh's reload.
const refreshRequester = "non-ECC refresh"

// SetRefreshHold installs a check that names why a model must not be
// refreshed right now ("" = go ahead) — a bench run measuring it, which a
// reload would corrupt. Set before StartNonECCRefresh.
func (m *Manager) SetRefreshHold(f func(model string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshHold = f
}

// StartNonECCRefresh runs the refresh every refreshInterval until ctx is done.
// The first look is one interval after start, not at boot: right after a
// start the agents' first reports, and the adoptions they bring, are still
// arriving.
func (m *Manager) StartNonECCRefresh(ctx context.Context) {
	go func() {
		t := time.NewTicker(refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.refreshNonECC(time.Now())
			}
		}
	}()
}

// refreshNonECC starts a refresh of every backend that is due and not held.
// Returns the process keys refreshed and deferred, for tests.
func (m *Manager) refreshNonECC(now time.Time) (refreshed, deferred []string) {
	cfg := m.config()
	if cfg == nil || m.draining.Load() {
		return nil, nil // handed over: the successor refreshes
	}
	maxAge, _ := cfg.Residency.NonECCMaxAgeDuration() // validated at load
	if maxAge <= 0 {
		return nil, nil
	}

	m.mu.Lock()
	handoff := m.localHold != nil
	hold := m.refreshHold
	procs := make([]*Process, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()
	sort.Slice(procs, func(i, j int) bool { return procs[i].key < procs[j].key })

	for _, p := range procs {
		why := m.refreshDue(p, now, maxAge)
		if why == "" {
			continue
		}
		wait := ""
		switch {
		case handoff:
			wait = "a handoff is in progress"
		case hold != nil:
			wait = hold(p.ModelName)
		}
		if wait == "" {
			wait = p.refreshBusy()
		}
		if wait != "" {
			slog.Info("non-ECC refresh deferred", "model", p.ModelName, "key", p.key, "reason", why, "waitingFor", wait)
			deferred = append(deferred, p.key)
			continue
		}
		if m.startRefresh(p, why) {
			refreshed = append(refreshed, p.key)
		}
	}
	return refreshed, deferred
}

// refreshDue says why p is due for a refresh, or "" when it is not: a spawned
// backend (not a trial, not a proxy) whose weights are older than maxAge and
// that holds memory on at least one pool without ECC.
func (m *Manager) refreshDue(p *Process, now time.Time, maxAge time.Duration) string {
	if p.server == "" || p.remote || strings.HasPrefix(p.key, TrialKeyPrefix) {
		return ""
	}
	p.mu.Lock()
	h, loadedAt := p.handle, p.loadedAt
	pools := make([]string, 0, len(p.usage))
	for pool, b := range p.usage {
		if b > 0 {
			pools = append(pools, pool)
		}
	}
	p.mu.Unlock()
	if h == nil || loadedAt.IsZero() {
		return ""
	}
	age := now.Sub(loadedAt)
	if age < maxAge {
		return ""
	}
	if len(pools) == 0 {
		// Nothing reserved (the size is not known yet): the device memory
		// is still where its weights are.
		if cfg := m.config(); cfg != nil {
			pools = cfg.DevicePoolsFor(p.server)
		}
	}
	sort.Strings(pools)
	for _, pool := range pools {
		if ecc, src := m.PoolECC(p.server, pool); !ecc {
			return fmt.Sprintf("non-ECC refresh: loaded %s ago (pool %s on %s has no ECC, %s)",
				age.Round(time.Hour), pool, p.server, src)
		}
	}
	return ""
}

// refreshBusy names why p cannot be refreshed at this moment, or "".
func (p *Process) refreshBusy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.draining:
		return "it is already draining"
	case p.state != StateReady:
		return "it is " + string(p.state)
	}
	return ""
}

// startRefresh drains p and reloads its model in the background. Reports
// whether the refresh started.
func (m *Manager) startRefresh(p *Process, why string) bool {
	m.mu.Lock()
	ours := m.procs[p.key] == p
	m.mu.Unlock()
	if !ours {
		return false // replaced since the look
	}
	p.mu.Lock()
	p.refreshing = true
	p.mu.Unlock()
	slog.Info(why, "model", p.ModelName, "key", p.key, "server", p.server)
	// force: a pinned model is refreshed too. A pin keeps a model warm; it is
	// not a reason to keep serving weights that may have flipped.
	if _, err := m.unloadKey(p.key, p.Name, true); err != nil {
		p.mu.Lock()
		p.refreshing = false
		p.mu.Unlock()
		slog.Warn("non-ECC refresh: could not start", "model", p.ModelName, "key", p.key, "err", err)
		return false
	}
	go m.reloadAfterRefresh(p)
	return true
}

// reloadAfterRefresh waits for p's drain to finish, then loads its model again.
func (m *Manager) reloadAfterRefresh(p *Process) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshDrainWait)
	err := m.awaitGone(ctx, p.key, p)
	cancel()
	if err != nil {
		slog.Warn("non-ECC refresh: drain still running; the model loads on its next request",
			"model", p.ModelName, "key", p.key, "waited", refreshDrainWait)
		return
	}
	m.reloadAfter(WithRequester(context.Background(), refreshRequester), p.Name, "non-ECC refresh")
}

// awaitGone blocks until p is no longer the process under key — drained and
// evicted, or exited.
func (m *Manager) awaitGone(ctx context.Context, key string, p *Process) error {
	for {
		m.mu.Lock()
		cur := m.procs[key]
		m.mu.Unlock()
		if cur != p {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(refreshPoll):
		}
	}
}
