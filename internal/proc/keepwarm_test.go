package proc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// warmConfig: resConfig with A keepWarm. budget 10, each 6: one at a time.
func warmConfig(t *testing.T) *config.Config {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	a := cfg.Models["A"]
	a.Sticky = &config.Sticky{KeepWarm: true}
	cfg.Models["A"] = a
	return cfg
}

func warmMgr(t *testing.T, cfg *config.Config) *Manager {
	m := NewManager(cfg)
	m.activeUse = 0
	m.healthTimeout = 5 * time.Second
	t.Cleanup(m.Shutdown)
	return m
}

func waitPass(t *testing.T, m *Manager) {
	t.Helper()
	select {
	case <-m.keepWarmPass(context.Background()):
	case <-time.After(10 * time.Second):
		t.Fatal("keepWarm pass did not resolve")
	}
}

func warmUp(m *Manager, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[key]
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == StateReady
}

func waitTeardown(t *testing.T, m *Manager, key string) {
	t.Helper()
	if err := m.awaitStop(context.Background(), key); err != nil {
		t.Fatal(err)
	}
}

func warmLoad(t *testing.T, m *Manager, cfg *config.Config, name string) {
	t.Helper()
	_, done, _, err := m.EnsureReady(context.Background(), name, cfg.Models[name], nil, nil)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	done()
}

// TestKeepWarmRespawnsAfterEvictionWhenRoomFrees is the feature, end to end:
// evicted for capacity, held while that capacity is taken (no counter-eviction)
// and for keepWarmAfterEvict, then back as soon as the room frees.
func TestKeepWarmRespawnsAfterEvictionWhenRoomFrees(t *testing.T) {
	cfg := warmConfig(t)
	m := warmMgr(t, cfg)
	warmLoad(t, m, cfg, "A")
	warmLoad(t, m, cfg, "B") // evicts A
	if warmUp(m, "A") {
		t.Fatal("precondition: B should have evicted A")
	}
	waitTeardown(t, m, "A")

	// Inside keepWarmAfterEvict: held even if it fit.
	waitPass(t, m)
	if warmUp(m, "A") {
		t.Fatal("respawned inside keepWarmAfterEvict")
	}

	// Past the hold, but B holds the room: no respawn, and B is not evicted.
	m.mu.Lock()
	m.warm["A"].evictedAt = time.Now().Add(-time.Hour)
	m.mu.Unlock()
	waitPass(t, m)
	if warmUp(m, "A") || !warmUp(m, "B") {
		t.Fatalf("a keepWarm respawn must never evict: A=%v B=%v", warmUp(m, "A"), warmUp(m, "B"))
	}
	m.mu.Lock()
	fails := m.warm["A"].failures
	m.mu.Unlock()
	if fails != 0 {
		t.Errorf("no room is not a failure: failures = %d", fails)
	}

	// Room frees: A comes back.
	if _, err := m.UnloadModel("B"); err != nil {
		t.Fatal(err)
	}
	waitPass(t, m)
	if !warmUp(m, "A") {
		t.Fatal("keepWarm model not respawned once it fit")
	}
}

// TestKeepWarmExplicitUnloadSuppresses: an operator's unload holds it down
// until a request loads it again or the config reloads.
func TestKeepWarmExplicitUnloadSuppresses(t *testing.T) {
	cfg := warmConfig(t)
	m := warmMgr(t, cfg)
	warmLoad(t, m, cfg, "A")
	if _, err := m.UnloadModel("A"); err != nil {
		t.Fatal(err)
	}
	waitTeardown(t, m, "A")
	waitPass(t, m)
	if warmUp(m, "A") {
		t.Fatal("respawned after an explicit unload")
	}

	// A request load lifts the hold.
	warmLoad(t, m, cfg, "A")
	m.mu.Lock()
	sup := m.warm["A"].suppressed
	m.mu.Unlock()
	if sup {
		t.Error("a request load must end the unload's hold")
	}

	// So does a reload.
	if _, err := m.UnloadModel("A"); err != nil {
		t.Fatal(err)
	}
	waitTeardown(t, m, "A")
	m.SetConfig(cfg)
	waitPass(t, m)
	if !warmUp(m, "A") {
		t.Error("a config reload must end the unload's hold")
	}
}

// TestKeepWarmBackoff: a model that cannot start is retried with exponential
// backoff, not on every pass.
func TestKeepWarmBackoff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := warmConfig(t)
	a := resModel(t, "box", map[string]string{"gpu": "6"}, dead)
	a.Cmd = "exit 1"
	a.Sticky = &config.Sticky{KeepWarm: true}
	cfg.Models["A"] = a
	m := warmMgr(t, cfg)

	failures := func() (int, time.Time) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.warm["A"].failures, m.warm["A"].nextTry
	}
	waitPass(t, m)
	n, next := failures()
	if n != 1 || time.Until(next) < 8*time.Second {
		t.Fatalf("after one failure: failures=%d retryIn=%s, want 1 / ~10s", n, time.Until(next))
	}
	waitPass(t, m) // inside the backoff: nothing starts
	if n, _ := failures(); n != 1 {
		t.Fatalf("retried inside the backoff: failures=%d", n)
	}
	m.mu.Lock()
	m.warm["A"].nextTry = time.Time{}
	m.mu.Unlock()
	waitPass(t, m)
	if n, next := failures(); n != 2 || time.Until(next) < 18*time.Second {
		t.Fatalf("after two failures: failures=%d retryIn=%s, want 2 / ~20s", n, time.Until(next))
	}

	for n, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 6: 320 * time.Second, 7: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := keepWarmBackoff(n); got != want {
			t.Errorf("keepWarmBackoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// TestKeepWarmCrashCountsAndHealthyResets: a ready keepWarm backend that exits
// on its own counts as a failure; one that has stayed up keepWarmHealthy
// clears the count. An ordered stop is not a crash.
func TestKeepWarmCrashCountsAndHealthyResets(t *testing.T) {
	cfg := warmConfig(t)
	m := warmMgr(t, cfg)
	p := &Process{key: "A", Name: "A", ModelName: "A", state: StateReady, keepWarm: true}
	m.recordExit(p, nil)
	m.mu.Lock()
	n := m.warm["A"].failures
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("crash: failures = %d, want 1", n)
	}
	ordered := &Process{key: "A", state: StateReady, keepWarm: true, stopped: true}
	m.recordExit(ordered, nil)
	m.mu.Lock()
	n = m.warm["A"].failures
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("an ordered stop counted as a crash: failures = %d", n)
	}

	mkResident(m, "A", 0, 0, func(q *Process) { q.ModelName = "A"; q.keepWarm = true })
	waitPass(t, m)
	m.mu.Lock()
	n = m.warm["A"].failures
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("up past keepWarmHealthy: failures = %d, want reset to 0", n)
	}
}

// TestKeepWarmNeverIdleUnloads: idleUnload on a keepWarm process (a lane
// override; Validate refuses it on the model) is ignored — the respawn would
// bring it straight back.
func TestKeepWarmNeverIdleUnloads(t *testing.T) {
	m := NewManager(&config.Config{})
	mkResident(m, "warm", time.Minute, time.Hour, func(p *Process) { p.keepWarm = true })
	m.sweepIdle()
	if !stillResident(m, "warm") {
		t.Error("a keepWarm backend must not be idle-unloaded")
	}
}

// TestKeepWarmBootPreloadNeverEvicts: two keepWarm models that cannot both
// fit — boot loads one and leaves the other to the respawn pass.
func TestKeepWarmBootPreloadNeverEvicts(t *testing.T) {
	cfg := warmConfig(t)
	b := cfg.Models["B"]
	b.Sticky = &config.Sticky{KeepWarm: true}
	cfg.Models["B"] = b
	m := warmMgr(t, cfg)
	m.Preload(context.Background())
	if a, b := warmUp(m, "A"), warmUp(m, "B"); a == b {
		t.Fatalf("boot preload: A=%v B=%v, want exactly one (no eviction)", a, b)
	}
	m.mu.Lock()
	stopping := len(m.stopping)
	m.mu.Unlock()
	if stopping != 0 {
		t.Errorf("boot preload evicted %d backend(s)", stopping)
	}
}
