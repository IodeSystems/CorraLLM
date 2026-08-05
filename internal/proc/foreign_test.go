package proc

import (
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

func macCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  mac1:
    pools: { system: 68GB }
    reserve: { system: 18GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.58:6503"] }
models:
  candidate:
    cmd: "exec llama-server --port 5810"
    server: mac1
    ramUsage: { system: 12GB }
    proxy: 5810
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The failure this prevents, measured on a real machine: 39.3 GB wired of a
// ~51.5 GB ceiling, only 33.5 GB of it corrallm's. Declaration-only arithmetic
// saw 50 − 33.5 = 16.5 GB free and would have admitted a 12 GB model, taking
// the machine past the ceiling and into failed Metal allocations.
func TestForeignMemoryCountsAgainstTheBudget(t *testing.T) {
	const gb = int64(1e9)
	m := NewManager(macCfg(t))
	live := agent.NewLiveness()
	live.Beat("mac1", time.Now())
	m.SetLiveness(live)

	mdl := m.config().Models["candidate"]
	usage, err := config.ParseSizes(mdl.RAMUsage)
	if err != nil {
		t.Fatal(err)
	}

	// corrallm's own resident model.
	ours := map[string]int64{"system": 33 * gb}
	m.mu.Lock()
	m.reserveLocked("mac1", ours)
	m.procs["resident"] = &Process{
		Name: "resident", key: "resident", server: "mac1", usage: ours,
		state: StateReady, ready: make(chan struct{}),
	}
	m.mu.Unlock()

	// Without a capacity report, the ledger sees 50 − 33 = 17 GB free.
	m.mu.Lock()
	fitsBlind := m.fitsLocked("mac1", usage, nil)
	m.mu.Unlock()
	if !fitsBlind {
		t.Fatal("precondition: 12GB should fit in a declaration-only ledger")
	}

	// The agent now reports what the MACHINE is doing: 39 GB used, of which
	// only our 33 GB is accounted for. The other 6 GB is the OS and whatever
	// else the operator is running.
	live.RecordCapacity("mac1", agent.Capacity{
		Host: &agent.DeviceMem{Name: "system", TotalBytes: 68 * gb, UsedBytes: 39 * gb},
	})

	m.mu.Lock()
	fitsInformed := m.fitsLocked("mac1", usage, nil)
	m.mu.Unlock()
	if fitsInformed {
		t.Error("admitted a model into memory another tenant is already using — " +
			"the ledger counted only its own footprint and called the rest free")
	}
}

// An agent that reports nothing must leave the arithmetic exactly as it was.
// This runs on every admission decision, so a silent behaviour change for
// local servers and older agents would be the worst kind of regression.
func TestNoCapacityReportKeepsDeclarationOnlyBehaviour(t *testing.T) {
	const gb = int64(1e9)
	m := NewManager(macCfg(t))
	live := agent.NewLiveness()
	live.Beat("mac1", time.Now())
	m.SetLiveness(live)

	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.foreignUsedLocked("mac1", "system"); got != 0 {
		t.Errorf("foreign = %d with no capacity report, want 0", got)
	}
	if !m.fitsLocked("mac1", map[string]int64{"system": 40 * gb}, nil) {
		t.Error("40GB should fit a 50GB budget when nothing is reserved or reported")
	}
}

// Only the device pool has a live reading behind it; other pools must keep
// their declared arithmetic rather than borrowing an unrelated number.
func TestForeignUsageAppliesOnlyToTheDevicePool(t *testing.T) {
	const gb = int64(1e9)
	m := NewManager(macCfg(t))
	live := agent.NewLiveness()
	live.Beat("mac1", time.Now())
	live.RecordCapacity("mac1", agent.Capacity{
		Host: &agent.DeviceMem{Name: "system", TotalBytes: 68 * gb, UsedBytes: 39 * gb},
	})
	m.SetLiveness(live)

	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.foreignUsedLocked("mac1", "system"); got != 39*gb {
		t.Errorf("device pool foreign = %d, want %d", got, 39*gb)
	}
	if got := m.foreignUsedLocked("mac1", "gpu0"); got != 0 {
		t.Errorf("non-device pool foreign = %d, want 0", got)
	}
}

// Placement selection is what makes a multi-box model schedulable at all.
func TestSelectPlacementPrefersAReachableBoxWithRoom(t *testing.T) {
	const gb = int64(1e9)
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.58:6503"] }
models:
  qwen:
    type: chat
    placements:
      - server: box1
        cmd: "exec a --port 5800"
        proxy: 5800
        ramUsage: { gpu0: 24GB }
      - server: mac1
        cmd: "exec b --port 5810"
        proxy: 5810
        ramUsage: { system: 20GB }
`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	live := agent.NewLiveness()
	live.Beat("mac1", time.Now())
	m.SetLiveness(live)
	mdl := cfg.Models["qwen"]

	// Both free: the first placement wins, so a single-box config and a
	// multi-box one behave identically until something forces a choice.
	p, err := m.selectPlacement("qwen", mdl)
	if err != nil || p.Server != "box1" {
		t.Fatalf("with everything free, want box1, got %q (%v)", p.Server, err)
	}

	// Fill box1. The model must move rather than queue behind a full box while
	// another placement sits idle — the entire point of declaring two.
	m.mu.Lock()
	m.reserveLocked("box1", map[string]int64{"gpu0": 29 * gb})
	m.mu.Unlock()

	p, err = m.selectPlacement("qwen", mdl)
	if err != nil {
		t.Fatal(err)
	}
	if p.Server != "mac1" {
		t.Errorf("box1 is full; want the mac1 placement, got %q", p.Server)
	}
	// And it must carry ITS OWN cmd and sizing, not box1's.
	rm := mdl.ForPlacement(p)
	if rm.Cmd != "exec b --port 5810" || rm.RAMUsage["system"] != "20GB" {
		t.Errorf("selected placement did not bring its own cmd/sizing: %+v", rm)
	}
	// Distinct process keys, so loading one is not mistaken for the other.
	k1 := mdl.PlacementProcKey("qwen", mdl.PlacementList()[0])
	k2 := mdl.PlacementProcKey("qwen", p)
	if k1 == k2 {
		t.Errorf("both placements key to %q", k1)
	}
}

// A box that is down is not a way to serve anything, however much room it has.
func TestSelectPlacementSkipsADownBox(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
    agent: { endpoints: ["http://10.0.0.9:6503"] }
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.58:6503"] }
models:
  qwen:
    type: chat
    placements:
      - server: box1
        cmd: "exec a --port 5800"
        proxy: 5800
      - server: mac1
        cmd: "exec b --port 5810"
        proxy: 5810
`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	live := agent.NewLiveness()
	// Only mac1 has ever reported in; box1 is past its miss window.
	live.Beat("box1", time.Now().Add(-10*time.Minute))
	live.Beat("mac1", time.Now())
	m.SetLiveness(live)

	p, err := m.selectPlacement("qwen", cfg.Models["qwen"])
	if err != nil {
		t.Fatal(err)
	}
	if p.Server != "mac1" {
		t.Errorf("box1 is down; want mac1, got %q", p.Server)
	}
}
