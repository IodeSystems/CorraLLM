package proc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// listenTCP starts a minimal HTTP server that answers /health with 200 (the
// llama-server readiness contract corrallm's waitHealthy now requires) and
// returns its port. The spawned `sleep` stands in for the backend process; this
// server stands in for its HTTP port.
func listenTCP(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func resModel(t *testing.T, server string, usage map[string]string, port int) config.Model {
	t.Helper()
	var pn yaml.Node
	if err := pn.Encode(port); err != nil {
		t.Fatal(err)
	}
	return config.Model{Cmd: "exec sleep 30", Server: server, RAMUsage: usage, Proxy: pn, Type: "local"}
}

// resConfig: a server "box" with a `gpu` pool budget, and two models A/B each
// reserving `each` bytes on gpu. With budget < 2*each only one fits at a time.
func resConfig(t *testing.T, budget string, each string, portA, portB int) *config.Config {
	return &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": budget}}},
		Models: map[string]config.Model{
			"A": resModel(t, "box", map[string]string{"gpu": each}, portA),
			"B": resModel(t, "box", map[string]string{"gpu": each}, portB),
		},
	}
}

// TestEvictIdleToFit: A is loaded then idle; loading B doesn't fit, so the
// solver evicts A (idle, non-pinned) to make room, and B becomes ready.
func TestEvictIdleToFit(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB) // 6+6 > 10 → mutually exclusive
	mgr := NewManager(cfg)
	mgr.activeUse = 0 // this test exercises eviction mechanics, not activity semantics
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	pA, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	pidA := pA.cmd.Process.Pid
	doneA() // A is now idle (evictable)

	pB, _, _, err := mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil)
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
	_, aResident := mgr.procs["A"]
	mgr.mu.Unlock()
	if used != 6 {
		t.Errorf("gpu used = %d, want 6 (only B)", used)
	}
	if aResident {
		t.Errorf("A still in ledger after eviction")
	}
}

// TestLoadUnloadModel: an explicit load warms a model (resident, ref-free), and
// unload evicts it. Persistent models refuse to unload.
func TestLoadUnloadModel(t *testing.T) {
	portA := listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, listenTCP(t))
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	name, err := mgr.LoadModel(ctx, "A")
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if name != "A" {
		t.Errorf("loaded %q, want A", name)
	}
	// Resident and idle (ref dropped) → in the snapshot, refs 0.
	snap := mgr.Snapshot()
	if len(snap.Models) != 1 || snap.Models[0].Name != "A" || snap.Models[0].Refs != 0 {
		t.Fatalf("after load, models = %+v", snap.Models)
	}

	n, err := mgr.UnloadModel("A")
	if err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted %d, want 1", n)
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("after unload, models = %+v", got.Models)
	}

	// Unloading a not-resident model is a no-op (0, nil).
	if n, err := mgr.UnloadModel("A"); err != nil || n != 0 {
		t.Errorf("unload of absent model = (%d, %v), want (0, nil)", n, err)
	}
}

// TestUnloadPersistentRefused: a persistent model can't be unloaded.
func TestUnloadPersistentRefused(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	// Pin A.
	m := cfg.Models["A"]
	m.Persistent = true
	cfg.Models["A"] = m
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	if _, err := mgr.LoadModel(context.Background(), "A"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if _, err := mgr.UnloadModel("A"); err == nil {
		t.Error("expected error unloading a persistent model")
	}
}

// TestSetHealthTimeout overrides the default and ignores non-positive values.
func TestSetHealthTimeout(t *testing.T) {
	mgr := NewManager(&config.Config{})
	if mgr.healthTimeout != 120*time.Second {
		t.Fatalf("default = %s, want 120s", mgr.healthTimeout)
	}
	mgr.SetHealthTimeout(600 * time.Second)
	if mgr.healthTimeout != 600*time.Second {
		t.Errorf("after override = %s, want 600s", mgr.healthTimeout)
	}
	mgr.SetHealthTimeout(0) // ignored
	if mgr.healthTimeout != 600*time.Second {
		t.Errorf("non-positive changed it to %s", mgr.healthTimeout)
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

	if _, _, _, err := mgr.EnsureReady(context.Background(), "A", cfg.Models["A"], nil); err != nil {
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
	if m.Name != "A" || m.ModelName != "A" || m.Server != "box" || m.State != string(StateReady) {
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

	_, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	defer doneA() // keep A busy (ref held) across the B attempt

	_, _, _, err = mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil)
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

	_, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	doneA() // idle but pinned

	if _, _, _, err := mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil); !errors.Is(err, ErrNoCapacity) {
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

	pA, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	defer doneA()
	_, doneB, _, err := mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil)
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

// TestActiveUseBlocksEviction: a model whose last request finished within the
// activeUse window is NOT an eviction victim, even with refs==0 — the
// between-turn gap of an agent session must not read as idle. The incoming
// load gets ErrNoCapacity (→ spill/queue) instead. Regression for the
// bench-vs-chat-lane thrash (107 no-capacity spills as each evicted the other
// between turns).
func TestActiveUseBlocksEviction(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB) // 6+6 > 10 → mutually exclusive
	mgr := NewManager(cfg)                       // default activeUse window
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	pA, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	doneA() // refs → 0, but lastUsed is NOW: A is between turns, not idle

	if _, _, _, err := mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("load B during A's active-use window: want ErrNoCapacity, got %v", err)
	}
	if pA.state != StateReady {
		t.Errorf("A must survive: state = %s", pA.state)
	}

	// Backdate A's last use past the window → now it's a legitimate victim.
	pA.mu.Lock()
	pA.lastUsed = time.Now().Add(-defaultActiveUse - time.Second)
	pA.mu.Unlock()
	if _, _, _, err := mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil); err != nil {
		t.Fatalf("load B after A idled past the window: %v", err)
	}
}
