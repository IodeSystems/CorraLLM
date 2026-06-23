package proc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

func listenTCP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func resBackend(t *testing.T, server string, usage map[string]string, port int) config.Backend {
	t.Helper()
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Backend{Cmd: "exec sleep 30", Server: server, RAMUsage: usage, Proxy: pn, Type: "local"}
}

// resConfig: a server "box" with a `gpu` pool budget, and two models A/B each
// reserving `each` bytes on gpu. With budget < 2*each only one fits at a time.
func resConfig(t *testing.T, budget string, each string, portA, portB int) *config.Config {
	return &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": budget}}},
		Models: map[string]config.Model{
			"A": {Backends: []config.Backend{resBackend(t, "box", map[string]string{"gpu": each}, portA)}},
			"B": {Backends: []config.Backend{resBackend(t, "box", map[string]string{"gpu": each}, portB)}},
		},
	}
}

// TestEvictIdleToFit: A is loaded then idle; loading B doesn't fit, so the
// solver evicts A (idle, non-pinned) to make room, and B becomes ready.
func TestEvictIdleToFit(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB) // 6+6 > 10 → mutually exclusive
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	pA, doneA, _, err := mgr.EnsureReady(ctx, "A#0", "A", cfg.Models["A"].Backends[0])
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	pidA := pA.cmd.Process.Pid
	doneA() // A is now idle (evictable)

	pB, _, _, err := mgr.EnsureReady(ctx, "B#0", "B", cfg.Models["B"].Backends[0])
	if err != nil {
		t.Fatalf("load B should evict A and succeed, got: %v", err)
	}
	if pB.state != StateReady {
		t.Fatalf("B state = %s", pB.state)
	}

	// A's process should be gone, and the ledger should account only B.
	deadline := time.Now().Add(3 * time.Second)
	for alive(pidA) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pidA) {
		t.Errorf("evicted A (pid %d) still alive", pidA)
	}
	mgr.mu.Lock()
	used := mgr.used["box"]["gpu"]
	_, aResident := mgr.procs["A#0"]
	mgr.mu.Unlock()
	if used != 6 {
		t.Errorf("gpu used = %d, want 6 (only B)", used)
	}
	if aResident {
		t.Errorf("A still in ledger after eviction")
	}
}

// TestSnapshot: an empty manager reports the configured pool budget with zero
// usage; after loading A the snapshot reflects the reservation and resident model.
func TestSnapshot(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB)
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	// Before any load: one server, gpu budget 10, used 0, no resident models.
	snap := mgr.Snapshot()
	if len(snap.Servers) != 1 || snap.Servers[0].Server != "box" {
		t.Fatalf("servers = %+v", snap.Servers)
	}
	if len(snap.Servers[0].Pools) != 1 || snap.Servers[0].Pools[0] != (PoolResidency{Pool: "gpu", Budget: 10, Used: 0}) {
		t.Fatalf("pools = %+v", snap.Servers[0].Pools)
	}
	if len(snap.Models) != 0 {
		t.Fatalf("want no resident models, got %+v", snap.Models)
	}

	if _, _, _, err := mgr.EnsureReady(context.Background(), "A#0", "A", cfg.Models["A"].Backends[0]); err != nil {
		t.Fatalf("load A: %v", err)
	}

	snap = mgr.Snapshot()
	if snap.Servers[0].Pools[0].Used != 6 {
		t.Errorf("used = %d, want 6", snap.Servers[0].Pools[0].Used)
	}
	if len(snap.Models) != 1 {
		t.Fatalf("want 1 resident model, got %d", len(snap.Models))
	}
	m := snap.Models[0]
	if m.Name != "A#0" || m.ModelName != "A" || m.Server != "box" || m.State != string(StateReady) {
		t.Errorf("model = %+v", m)
	}
	if len(m.Usage) != 1 || m.Usage[0] != (PoolUsage{Pool: "gpu", Bytes: 6}) {
		t.Errorf("usage = %+v", m.Usage)
	}
}

// TestNoCapacityWhenBusy: A holds an in-flight ref, so it can't be evicted;
// loading B returns ErrNoCapacity (the edge would spill).
func TestNoCapacityWhenBusy(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB)
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	_, doneA, _, err := mgr.EnsureReady(ctx, "A#0", "A", cfg.Models["A"].Backends[0])
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	defer doneA() // keep A busy (ref held) across the B attempt

	_, _, _, err = mgr.EnsureReady(ctx, "B#0", "B", cfg.Models["B"].Backends[0])
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity (A busy), got: %v", err)
	}
}

// TestPersistentNotEvicted: a pinned model stays resident even when idle, so
// loading a conflicting model returns ErrNoCapacity.
func TestPersistentNotEvicted(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB)
	a := cfg.Models["A"]
	a.Persistent = true
	cfg.Models["A"] = a

	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	_, doneA, _, err := mgr.EnsureReady(ctx, "A#0", "A", cfg.Models["A"].Backends[0])
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	doneA() // idle but pinned

	if _, _, _, err := mgr.EnsureReady(ctx, "B#0", "B", cfg.Models["B"].Backends[0]); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity (A pinned), got: %v", err)
	}
}

// TestFitsAlongside: when the budget holds both, B loads without evicting A.
func TestFitsAlongside(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "16", "6", portA, portB) // 6+6 ≤ 16
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	pA, doneA, _, err := mgr.EnsureReady(ctx, "A#0", "A", cfg.Models["A"].Backends[0])
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	defer doneA()
	_, doneB, _, err := mgr.EnsureReady(ctx, "B#0", "B", cfg.Models["B"].Backends[0])
	if err != nil {
		t.Fatalf("load B alongside A: %v", err)
	}
	defer doneB()

	if !alive(pA.cmd.Process.Pid) {
		t.Errorf("A should remain resident alongside B")
	}
	mgr.mu.Lock()
	used := mgr.used["box"]["gpu"]
	mgr.mu.Unlock()
	if used != 12 {
		t.Errorf("gpu used = %d, want 12 (A+B)", used)
	}
}
