package api

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

const gib = int64(1) << 30

// budget mirrors what proc.NewManager computes per pool: total − reserve. The
// point of these tests is that this number is honest, so it is worth deriving
// the same way the scheduler does rather than eyeballing the strings.
func budget(t *testing.T, pools, reserve map[string]string) map[string]int64 {
	t.Helper()
	tot, err := config.ParseSizes(pools)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	res, err := config.ParseSizes(reserve)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	out := map[string]int64{}
	for pool, b := range tot {
		if b -= res[pool]; b < 0 {
			b = 0
		}
		out[pool] = b
	}
	return out
}

// A unified-memory host must be declared ONCE. The regression: system was sized
// from hw.memsize and gpu0 from the wired limit — the same RAM in two ledgers
// that fitsLocked checks independently, so a 64 GiB Mac offered 119 GB.
func TestSizeFromUnifiedDeclaresMemoryOnce(t *testing.T) {
	const memsize = 64 * gib
	wired := memsize * 3 / 4 // macOS's default when iogpu.wired_limit_mb is unset

	pools, reserve, devicePool := sizeFrom(agent.Capacity{
		Host:    &agent.DeviceMem{Name: "system", TotalBytes: memsize},
		GPU:     &agent.DeviceMem{Name: "Apple M3 Max", TotalBytes: wired},
		Unified: true,
	})

	if len(pools) != 1 {
		t.Fatalf("unified memory must be one pool, got %v", pools)
	}
	if devicePool != "system" {
		t.Errorf("devicePool = %q, want system (there is no discrete device)", devicePool)
	}
	// Every pool the server declares must be one it actually has, or a measured
	// footprint is charged against a budget of zero and the model goes
	// permanently unschedulable.
	if _, ok := pools[devicePool]; !ok {
		t.Errorf("devicePool %q is not among pools %v", devicePool, pools)
	}

	b := budget(t, pools, reserve)
	// Total declared may not exceed the machine.
	var declared int64
	for _, v := range b {
		declared += v
	}
	if declared > memsize {
		t.Errorf("declared %d bytes of budget on a %d byte machine", declared, memsize)
	}
	// Reserve carries the wired limit, so the budget lands on it: what the GPU
	// may actually hold, with the remainder visible as headroom. Pool floors and
	// reserve ceils, so it may sit up to 2 GB low — never high, which would be
	// budget the GPU cannot actually wire.
	if got := b["system"]; got > wired || got < wired-2*int64(gb) {
		t.Errorf("budget = %d, want the wired limit %d (or up to 2GB under)", got, wired)
	}
}

// A discrete card is genuinely two independent budgets and must stay that way.
func TestSizeFromDiscreteKeepsTwoPools(t *testing.T) {
	pools, reserve, devicePool := sizeFrom(agent.Capacity{
		Host: &agent.DeviceMem{Name: "system", TotalBytes: 128 * gib},
		GPU:  &agent.DeviceMem{Name: "NVIDIA GeForce RTX 5090", TotalBytes: 32 * gib},
	})
	if len(pools) != 2 {
		t.Fatalf("discrete GPU should declare system and gpu0, got %v", pools)
	}
	if devicePool != "gpu0" {
		t.Errorf("devicePool = %q, want gpu0", devicePool)
	}
	if len(reserve) != 0 {
		t.Errorf("discrete host should not invent a reserve, got %v", reserve)
	}
	if _, ok := pools[devicePool]; !ok {
		t.Errorf("devicePool %q is not among pools %v", devicePool, pools)
	}
}

// An operator who raises iogpu.wired_limit_mb to physical memory still needs a
// machine that runs macOS. Reserve must not collapse to zero.
func TestSizeFromUnifiedKeepsHeadroomWhenWiredLimitIsRaised(t *testing.T) {
	const memsize = 64 * gib
	pools, reserve, _ := sizeFrom(agent.Capacity{
		Host:    &agent.DeviceMem{Name: "system", TotalBytes: memsize},
		GPU:     &agent.DeviceMem{Name: "Apple M3 Ultra", TotalBytes: memsize},
		Unified: true,
	})
	b := budget(t, pools, reserve)
	if b["system"] >= memsize {
		t.Errorf("budget %d leaves the OS nothing on a %d byte machine", b["system"], memsize)
	}
}

// The host probe can fail independently of the GPU one. Falling back to the
// wired limit is the only figure left, and it must still be a single pool.
func TestSizeFromUnifiedSurvivesAFailedHostProbe(t *testing.T) {
	pools, _, devicePool := sizeFrom(agent.Capacity{
		GPU:       &agent.DeviceMem{Name: "Apple M2", TotalBytes: 16 * gib},
		HostError: "vm_stat: exit status 1",
		Unified:   true,
	})
	if len(pools) != 1 || devicePool != "system" {
		t.Fatalf("want one system pool, got pools=%v devicePool=%q", pools, devicePool)
	}
	if pools["system"] == "0" {
		t.Error("the wired limit was known; sizing it 0 discards a real measurement")
	}
}

// Measuring nothing must not invent a budget: an invented pool is one the
// scheduler admits against, and being wrong there means OOM.
func TestSizeFromUnmeasurableHostIsUnsized(t *testing.T) {
	pools, reserve, devicePool := sizeFrom(agent.Capacity{
		GPUError: "nvidia-smi: not found", HostError: "no /proc/meminfo",
	})
	if pools["system"] != "0" || len(pools) != 1 {
		t.Errorf("want a single zero pool, got %v", pools)
	}
	if len(reserve) != 0 {
		t.Errorf("nothing measured, nothing to reserve, got %v", reserve)
	}
	if _, ok := pools[devicePool]; !ok {
		t.Errorf("devicePool %q is not among pools %v", devicePool, pools)
	}
}

// An agent predating the Unified field reports false. That is the old two-pool
// behavior — wrong on a Mac, but no worse than before, and it must not panic or
// produce a config that fails validation.
func TestSizeFromOlderAgentStillValidates(t *testing.T) {
	pools, reserve, devicePool := sizeFrom(agent.Capacity{
		Host: &agent.DeviceMem{Name: "system", TotalBytes: 64 * gib},
		GPU:  &agent.DeviceMem{Name: "Apple M3 Max", TotalBytes: 48 * gib},
	})
	cfg := &config.Config{Servers: map[string]config.Server{
		"legacy": {Pools: pools, Reserve: reserve, DevicePool: devicePool},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enrollment produced an invalid config: %v", err)
	}
}

// The repair path. A Mac enrolled before sizeFrom was fixed carries a pool shape
// that counts its unified memory twice; re-running the install command must
// replace it, because re-measuring the machine is what enrollment IS. Preserving
// the old sizing made the operation a no-op exactly when it was needed.
func TestReEnrollmentResizesAStaleServer(t *testing.T) {
	// What the buggy enrollment wrote: one machine, two ledgers.
	prev := config.Server{
		Pools:           map[string]string{"system": "68GB", "gpu0": "51GB"},
		DevicePool:      "gpu0",
		NoProcessMemory: true,
		MaxConcurrent:   3,
		Notes:           "Enrolled 2026-07-30 from carls-mac.",
	}
	pools, reserve, devicePool := sizeFrom(agent.Capacity{
		Host:    &agent.DeviceMem{Name: "system", TotalBytes: 64 * gib},
		GPU:     &agent.DeviceMem{Name: "Apple M3 Max", TotalBytes: 48 * gib},
		Unified: true,
	})
	fresh := config.Server{Pools: pools, Reserve: reserve, DevicePool: devicePool, NoProcessMemory: true}

	got := mergeEnrollment(prev, fresh)

	if len(got.Pools) != 1 || got.DevicePool != "system" {
		t.Fatalf("re-enrollment did not resize: pools=%v devicePool=%q", got.Pools, got.DevicePool)
	}
	if len(got.Reserve) == 0 {
		t.Error("the derived reserve was dropped — on a unified host that is the whole safety margin")
	}
	// Operator policy is not a measurement and must survive.
	if got.MaxConcurrent != 3 {
		t.Errorf("maxConcurrent = %d, want the operator's 3", got.MaxConcurrent)
	}
	if !strings.Contains(got.Notes, "Enrolled 2026-07-30") {
		t.Error("the operator's notes were lost")
	}
	// A silent resize is how a hand-tuned pool disappears without trace.
	if !strings.Contains(got.Notes, "resized this server") {
		t.Errorf("the resize was not recorded in the notes: %q", got.Notes)
	}
}

// Re-enrolling an unchanged machine should not accrete a resize note on every
// run, or the notes become a log nobody reads.
func TestReEnrollmentIsQuietWhenNothingChanged(t *testing.T) {
	pools := map[string]string{"system": "68GB"}
	prev := config.Server{Pools: pools, DevicePool: "system", Notes: "hand-written note"}
	fresh := config.Server{Pools: map[string]string{"system": "68GB"}, DevicePool: "system"}

	got := mergeEnrollment(prev, fresh)
	if strings.Contains(got.Notes, "resized") {
		t.Errorf("reported a resize that did not happen: %q", got.Notes)
	}
	if !strings.Contains(got.Notes, "hand-written note") {
		t.Error("the operator's notes were lost")
	}
}
