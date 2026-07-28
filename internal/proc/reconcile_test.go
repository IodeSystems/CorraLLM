package proc

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

func reconcileMgr(t *testing.T) (*Manager, *config.Config) {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  mac1:
    pools: { system: 8GB }
    devicePool: system
    agent: { endpoints: ["http://127.0.0.1:1"] }
models:
  m:
    cmd: "exec sleep 300"
    server: mac1
    ramUsage: { system: 1GB }
    proxy: 5810
`))
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(cfg), cfg
}

// The steady state: what the agent reports is what we believe. Nothing is
// killed, nothing is freed — a brief control-plane outage must cost nothing,
// which is the entire reason adoption exists rather than kill-on-connect.
func TestReconcile_AdoptsMatchingBackends(t *testing.T) {
	m, _ := reconcileMgr(t)
	m.mu.Lock()
	m.procs["m"] = &Process{Name: "m", key: "m", server: "mac1", usage: map[string]int64{"system": 1 << 30}}
	m.reserveLocked("mac1", map[string]int64{"system": 1 << 30})
	m.mu.Unlock()

	adopted, reaped, vanished := m.ReconcileAgent(context.Background(), "mac1",
		[]agent.Backend{{ID: "b1", Key: "m", Alive: true, Started: time.Now().Add(-5 * time.Minute).UnixMilli()}})

	if adopted != 1 || reaped != 0 || vanished != 0 {
		t.Errorf("adopted=%d reaped=%d vanished=%d; want 1/0/0", adopted, reaped, vanished)
	}
	m.mu.Lock()
	used := m.used["mac1"]["system"]
	m.mu.Unlock()
	if used != 1<<30 {
		t.Errorf("reservation = %d; adoption must not disturb the ledger", used)
	}
}

// The primary holds a Process the agent does not report — the agent restarted,
// or the backend died while we could not hear. Its pools must be freed, or that
// server's budget stays spoken for by nothing that exists.
func TestReconcile_FreesPoolsForVanishedBackends(t *testing.T) {
	m, _ := reconcileMgr(t)
	m.mu.Lock()
	m.procs["m"] = &Process{Name: "m", key: "m", server: "mac1", usage: map[string]int64{"system": 1 << 30}, ready: make(chan struct{})}
	m.reserveLocked("mac1", map[string]int64{"system": 1 << 30})
	m.mu.Unlock()

	_, _, vanished := m.ReconcileAgent(context.Background(), "mac1", nil)
	if vanished != 1 {
		t.Fatalf("vanished = %d, want 1", vanished)
	}
	m.mu.Lock()
	used := m.used["mac1"]["system"]
	m.mu.Unlock()
	if used != 0 {
		t.Errorf("pool still reserved (%d) for a backend that is gone", used)
	}
}

// A backend the agent started seconds ago may simply not be registered here
// yet — registration is not simultaneous on both sides. Reaping it would kill
// something the primary deliberately spawned.
func TestReconcile_GraceProtectsAJustStartedBackend(t *testing.T) {
	m, _ := reconcileMgr(t)
	_, reaped, _ := m.ReconcileAgent(context.Background(), "mac1",
		[]agent.Backend{{ID: "b9", Key: "not-ours-yet", Alive: true, Started: time.Now().UnixMilli()}})
	if reaped != 0 {
		t.Errorf("reaped %d backends inside the adoption grace", reaped)
	}
}

// A backend the agent reports as already dead needs no reaping.
func TestReconcile_IgnoresDeadBackends(t *testing.T) {
	m, _ := reconcileMgr(t)
	_, reaped, _ := m.ReconcileAgent(context.Background(), "mac1",
		[]agent.Backend{{ID: "b1", Key: "gone", Alive: false, Started: time.Now().Add(-time.Hour).UnixMilli()}})
	if reaped != 0 {
		t.Errorf("reaped %d already-dead backends", reaped)
	}
}

// Reconciliation is scoped to ONE server. A backend resident on box1 must never
// be freed because mac1's agent did not mention it.
func TestReconcile_DoesNotTouchOtherServers(t *testing.T) {
	m, _ := reconcileMgr(t)
	m.mu.Lock()
	m.procs["other"] = &Process{Name: "other", key: "other", server: "box1", usage: map[string]int64{"gpu0": 1 << 30}}
	m.mu.Unlock()

	_, _, vanished := m.ReconcileAgent(context.Background(), "mac1", nil)
	if vanished != 0 {
		t.Errorf("vanished = %d; mac1's heartbeat must not evict box1's residents", vanished)
	}
	m.mu.Lock()
	_, still := m.procs["other"]
	m.mu.Unlock()
	if !still {
		t.Error("a box1 resident was dropped by mac1's reconciliation")
	}
}
