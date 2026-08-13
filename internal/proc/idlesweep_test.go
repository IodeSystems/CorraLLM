package proc

import (
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// mkResident builds a Process in the state sweepIdle inspects, without spawning
// anything: the sweeper reads residency bookkeeping, not the backend.
func mkResident(m *Manager, key string, idleUnload, idleFor time.Duration, opts ...func(*Process)) *Process {
	now := time.Now()
	p := &Process{
		key: key, Name: key, state: StateReady,
		idleUnload: idleUnload,
		lastUsed:   now.Add(-idleFor),
		readyAt:    now.Add(-time.Hour), // well past minResidency
	}
	for _, o := range opts {
		o(p)
	}
	m.procs[key] = p
	return p
}

func stillResident(m *Manager, key string) bool { _, ok := m.procs[key]; return ok }

// TestIdleSweepUnloadsQuietBackend is the feature: a backend quiet past its
// idleUnload is released WITHOUT anything else asking for the memory.
func TestIdleSweepUnloadsQuietBackend(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "quiet", 5*time.Minute, 6*time.Minute)
	m.sweepIdle()
	if stillResident(m, "quiet") {
		t.Error("a backend quiet past idleUnload should have been unloaded")
	}
}

// TestIdleSweepLeavesWarmBackend: still inside the quiet period, so untouched.
func TestIdleSweepLeavesWarmBackend(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "warm", 5*time.Minute, 2*time.Minute)
	m.sweepIdle()
	if !stillResident(m, "warm") {
		t.Error("a backend inside its quiet period must not be unloaded")
	}
}

// TestIdleSweepIsOptIn: every model without idleUnload keeps the pre-existing
// behaviour — resident until something else needs the memory. This feature must
// not change residency for a config that never asked for it.
func TestIdleSweepIsOptIn(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "unset", 0, 24*time.Hour)
	m.sweepIdle()
	if !stillResident(m, "unset") {
		t.Error("idleUnload unset must mean never, however long the model has been idle")
	}
}

// TestIdleSweepSkipsPersistent: pinned means pinned.
func TestIdleSweepSkipsPersistent(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "pinned", time.Minute, time.Hour, func(p *Process) { p.persistent = true })
	m.sweepIdle()
	if !stillResident(m, "pinned") {
		t.Error("a persistent backend must never be idle-unloaded")
	}
}

// TestIdleSweepSkipsInFlight guards the sharpest failure mode. lastUsed is only
// stamped when a request RELEASES, so a long generation looks progressively more
// idle the longer it runs — without the refs guard, a 20-minute completion would
// have the backend unloaded out from under it.
func TestIdleSweepSkipsInFlight(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "busy", time.Minute, time.Hour, func(p *Process) { p.refs = 1 })
	m.sweepIdle()
	if !stillResident(m, "busy") {
		t.Error("a backend with in-flight requests is not idle, whatever lastUsed says")
	}
}

// TestIdleSweepSkipsMinResidency: a backend that just finished loading is
// protected, so a too-short idleUnload cannot thrash it immediately.
func TestIdleSweepSkipsMinResidency(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "fresh", time.Second, time.Hour, func(p *Process) { p.readyAt = time.Now() })
	m.sweepIdle()
	if !stillResident(m, "fresh") {
		t.Error("a just-loaded backend is min-residency protected")
	}
}

// TestIdleSweepSkipsNotReady: a loading or evicting process is not a candidate.
func TestIdleSweepSkipsNotReady(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "loading", time.Minute, time.Hour, func(p *Process) { p.state = StateLoading })
	m.sweepIdle()
	if !stillResident(m, "loading") {
		t.Error("a backend that is not ready must not be idle-unloaded")
	}
}

// TestIdleUnloadRefusedBelowTTL: idleUnload <= ttl means the two settings
// disagree about the same model — one calls it warm, the other unloads it.
// Refused (0 = never) rather than silently clamped, so the mistake is visible.
func TestIdleUnloadRefusedBelowTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *config.Sticky
		want time.Duration
	}{
		{"below ttl", &config.Sticky{TTL: "10m", IdleUnload: "5m"}, 0},
		{"equal to ttl", &config.Sticky{TTL: "10m", IdleUnload: "10m"}, 0},
		{"above ttl", &config.Sticky{TTL: "10m", IdleUnload: "30m"}, 30 * time.Minute},
		{"no ttl", &config.Sticky{IdleUnload: "5m"}, 5 * time.Minute},
		{"unset", &config.Sticky{TTL: "10m"}, 0},
		{"garbage", &config.Sticky{IdleUnload: "soon"}, 0},
		{"nil", nil, 0},
	} {
		if got := stickyIdleUnload(tc.s); got != tc.want {
			t.Errorf("%s: stickyIdleUnload = %v, want %v", tc.name, got, tc.want)
		}
	}
}
