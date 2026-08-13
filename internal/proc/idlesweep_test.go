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

// TestIdleUnloadHonouredBelowTTL: idleUnload at or below ttl is honoured, not
// refused. Every TTL on this box is minutes, so refusing would turn the most
// natural setting an operator could write ("unload after 5m quiet") into a
// silent no-op. The ttl merely becomes moot — the process is gone before the
// eviction ordering consults it.
func TestIdleUnloadHonouredBelowTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *config.Sticky
		want time.Duration
	}{
		{"below ttl", &config.Sticky{TTL: "10m", IdleUnload: "5m"}, 5 * time.Minute},
		{"equal to ttl", &config.Sticky{TTL: "10m", IdleUnload: "10m"}, 10 * time.Minute},
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

// TestIdleClockRestartsOnRelease pins the invariant the refs guard depends on:
// the quiet period counts only while NO request is running.
//
// lastUsed is stamped on acquire AND on release (manager.go, refs++/refs--), so
// a long generation cannot age its own backend out. Were it stamped only on
// acquire, a 20-minute completion would release with lastUsed 20 minutes old
// and be unloaded on the very next sweep — instantly, having just finished
// serving. This asserts the release-side stamp, which is easy to drop in a
// refactor and silent when it goes.
func TestIdleClockRestartsOnRelease(t *testing.T) {
	m := NewManager(&config.Config{})
	p := mkResident(m, "long", 5*time.Minute, 0)

	// A 20-minute request: acquired long ago, still in flight.
	p.mu.Lock()
	p.refs = 1
	p.lastUsed = time.Now().Add(-20 * time.Minute)
	p.mu.Unlock()

	m.sweepIdle()
	if !stillResident(m, "long") {
		t.Fatal("in-flight request was unloaded mid-generation")
	}

	// Release: refs drops and the clock restarts from now.
	p.mu.Lock()
	p.refs = 0
	p.lastUsed = time.Now()
	p.mu.Unlock()

	m.sweepIdle()
	if !stillResident(m, "long") {
		t.Error("unloaded immediately after release; the idle clock did not restart")
	}

	// And it does go, once genuinely quiet for the whole period.
	p.mu.Lock()
	p.lastUsed = time.Now().Add(-6 * time.Minute)
	p.mu.Unlock()
	m.sweepIdle()
	if stillResident(m, "long") {
		t.Error("should unload after a full quiet period with no request running")
	}
}
