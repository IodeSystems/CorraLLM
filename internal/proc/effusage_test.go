//go:build unix

// Exercises real process groups (Setpgid, kill by negative pid), which are a
// POSIX construct; Windows groups a tree with a Job Object instead.
package proc

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/tune"
)

func usageMgr(t *testing.T, prof *tune.Profile) *Manager {
	t.Helper()
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	m := NewManager(&config.Config{Servers: map[string]config.Server{"box": {}}})
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	if prof != nil {
		c.Update("Fake GPU", "m", *prof)
	}
	m.SetTuneCache(c)
	return m
}

const mib = 1024 * 1024

// The bug this fixes: ramUsage is what admission and eviction trust, and it is a
// number a human typed. ternary-bonsai-27b declared 16GB and really took 23098
// MiB once its context window was restored — a 7 GB under-declaration that would
// let the scheduler admit a neighbour which could not fit, while the tune profile
// held the truth and nothing consulted it.
func TestEffectiveUsage_PrefersMeasuredOverUnderDeclaredConfig(t *testing.T) {
	m := usageMgr(t, &tune.Profile{
		BaseMiB: 20000, PerSlotMiB: 1500, PeakMiB: 23098,
		MeasuredSlots: 2, Ctx: 440000, Source: tune.SourceBench,
	})
	mdl := config.Model{
		Server: "box", MaxConcurrent: 2,
		RAMUsage: map[string]string{"gpu0": "16GB", "system": "3GB"},
	}
	got := m.effectiveUsage("m", mdl)
	// Compare against the PARSER's notion of these sizes — ParseSize reads GB as
	// decimal (10^9), so hardcoding binary GiB here would test my arithmetic
	// rather than the behavior.
	want16, _ := config.ParseSize("16GB")
	want3, _ := config.ParseSize("3GB")
	if got["gpu0"] <= want16 {
		t.Errorf("gpu0 = %d MiB, must exceed the under-declared 16GB", got["gpu0"]/mib)
	}
	// system RAM is not measured by the GPU profile and must come from config.
	if got["system"] != want3 {
		t.Errorf("system = %d MiB, want the configured 3GB", got["system"]/mib)
	}
}

// The estimate must be computed for THIS spawn's slot count: a profile measured
// at one slot cannot be applied unchanged to two.
func TestEffectiveUsage_ScalesWithSlots(t *testing.T) {
	prof := tune.Profile{
		BaseMiB: 10000, PerSlotMiB: 2000, PeakMiB: 12000,
		MeasuredSlots: 1, Ctx: 100000, Source: tune.SourceBench,
	}
	m := usageMgr(t, &prof)
	one := m.effectiveUsage("m", config.Model{Server: "box", MaxConcurrent: 1,
		RAMUsage: map[string]string{"gpu0": "1GB"}})
	two := m.effectiveUsage("m", config.Model{Server: "box", MaxConcurrent: 2,
		RAMUsage: map[string]string{"gpu0": "1GB"}})
	if two["gpu0"] <= one["gpu0"] {
		t.Errorf("two slots (%d MiB) must reserve more than one (%d MiB)",
			two["gpu0"]/mib, one["gpu0"]/mib)
	}
}

// PeakMiB is a floor: it is the largest footprint ever observed, so any estimate
// below it is known to be an under-estimate.
func TestEffectiveUsage_PeakIsAFloor(t *testing.T) {
	m := usageMgr(t, &tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 10, PeakMiB: 30000,
		MeasuredSlots: 1, Ctx: 1000, Source: tune.SourceBench,
	})
	got := m.effectiveUsage("m", config.Model{Server: "box", MaxConcurrent: 1,
		RAMUsage: map[string]string{"gpu0": "1GB"}})
	if got["gpu0"] < 30000*mib {
		t.Errorf("reservation %d MiB fell below the observed peak of 30000 MiB", got["gpu0"]/mib)
	}
}

// A fresh install must schedule before anything has been measured.
func TestEffectiveUsage_FallsBackToConfigWithoutProfile(t *testing.T) {
	m := usageMgr(t, nil)
	mdl := config.Model{Server: "box", MaxConcurrent: 1,
		RAMUsage: map[string]string{"gpu0": "16GB", "system": "3GB"}}
	got := m.effectiveUsage("m", mdl)
	want16, _ := config.ParseSize("16GB")
	if got["gpu0"] != want16 {
		t.Errorf("gpu0 = %d MiB, want the configured 16GB when nothing is measured", got["gpu0"]/mib)
	}
}

// A pure-proxy model consumes no local pools; there is nothing to reconcile.
func TestEffectiveUsage_ProxyModelUntouched(t *testing.T) {
	m := usageMgr(t, &tune.Profile{BaseMiB: 9999, PeakMiB: 9999, MeasuredSlots: 1})
	got := m.effectiveUsage("m", config.Model{}) // no Server
	if len(got) != 0 {
		t.Errorf("pure proxy should reserve nothing, got %+v", got)
	}
}

// A model with NO profile and NO ramUsage is of unknown size. The old behavior
// was the worst available: an empty usage map meant the spawn reserved NOTHING,
// so corrallm admitted an unknown-sized model while believing it consumed zero —
// and kept believing that until something OOMed.
//
// Unknown now means "assume it needs the whole pool": clear the board, spawn
// alone, measure. One heavy eviction, once, and then the measurement governs.
// An unmeasured model claims NOTHING on a device pool: the spawn is the fit
// test. This replaced "reserve the whole pool", which evicted every evictable
// resident on the first spawn of every new model to learn a number the spawn
// itself reports.
//
// Safe to let fail because a CUDA OOM is contained to the process that asked and
// costs one cold load. It is NOT the rule for host RAM — see the sibling test.
func TestEffectiveUsage_UnknownSizeClaimsNothingOnADevicePool(t *testing.T) {
	m := usageMgr(t, nil) // no profile
	m.budget["box"] = map[string]int64{"gpu0": 31654 * mib}

	mdl := config.Model{Server: "box", MaxConcurrent: 1} // no RAMUsage at all
	got := m.effectiveUsage("mystery", mdl)
	if _, claimed := got["gpu0"]; claimed {
		t.Errorf("gpu0 = %d MiB; an unmeasured model must claim nothing on a device pool",
			got["gpu0"]/mib)
	}
}

// Host RAM is the exception, and it is not a stylistic one. A CUDA OOM kills the
// allocator; an anonymous-RSS blowout takes the machine — oidio reached 119 GB
// anon-rss on this box and took corrallm down with it. Claiming nothing there
// removes the only ledger-side check.
func TestEffectiveUsage_UnknownSizeStillReservesHostRAM(t *testing.T) {
	m := usageMgr(t, nil)
	m.SetConfig(&config.Config{Servers: map[string]config.Server{
		"box": {Pools: map[string]string{"gpu0": "31654MiB", "system": "64GB"}},
	}})
	m.budget["box"] = map[string]int64{"gpu0": 31654 * mib, "system": 64000 * mib}

	got := m.effectiveUsage("mystery", config.Model{Server: "box", MaxConcurrent: 1})
	if _, claimed := got["gpu0"]; claimed {
		t.Errorf("gpu0 must be unclaimed, got %d MiB", got["gpu0"]/mib)
	}
	if got["system"] != 64000*mib {
		t.Errorf("system = %d MiB, want the whole pool — host RAM keeps the conservative path",
			got["system"]/mib)
	}
}

// "Claim nothing, then measure" has no measuring step on a host that cannot
// attribute memory per process, so it would degrade to "claim nothing, forever"
// and the server would silently over-admit.
func TestEffectiveUsage_UnknownSizeStillReservesWhereNothingCanMeasure(t *testing.T) {
	m := usageMgr(t, nil)
	m.SetConfig(&config.Config{Servers: map[string]config.Server{
		"box": {Pools: map[string]string{"gpu0": "31654MiB"}, NoProcessMemory: true},
	}})
	m.budget["box"] = map[string]int64{"gpu0": 31654 * mib}

	got := m.effectiveUsage("mystery", config.Model{Server: "box", MaxConcurrent: 1})
	if got["gpu0"] != 31654*mib {
		t.Errorf("gpu0 = %d MiB, want the whole pool on a host that cannot measure",
			got["gpu0"]/mib)
	}
}

// Once measured, the profile governs and the whole-pool assumption is dropped —
// otherwise a model would monopolise the box forever.
func TestEffectiveUsage_MeasuredReplacesUnknown(t *testing.T) {
	m := usageMgr(t, &tune.Profile{
		BaseMiB: 8000, PerSlotMiB: 500, PeakMiB: 8500,
		MeasuredSlots: 1, Ctx: 100000, Source: tune.SourceBench,
	})
	m.budget["box"] = map[string]int64{"gpu0": 31654 * mib}

	got := m.effectiveUsage("m", config.Model{Server: "box", MaxConcurrent: 1})
	if got["gpu0"] >= 31654*mib {
		t.Errorf("a measured model must not keep reserving the whole pool: %d MiB", got["gpu0"]/mib)
	}
	if got["gpu0"] != 8500*mib {
		t.Errorf("gpu0 = %d MiB, want the measured 8500", got["gpu0"]/mib)
	}
}

// A unified-memory host (Apple silicon) has no discrete VRAM: it declares one
// `system` pool and names it as its devicePool. The measured footprint must be
// charged THERE.
//
// The bug this fixes was silent and total. The measured footprint went to a
// hardcoded "gpu0", so on such a server every measured model was charged against
// a pool the server does not declare — budget zero — and fitsLocked refused it
// forever with a PERMANENT capacity error. That surfaces as a 503 that reads
// like a broken backend, not like a pool-naming mistake, on a model that had
// been working right up until the moment it was first measured.
func TestEffectiveUsage_ChargesMeasuredToServerDevicePool(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	m := NewManager(&config.Config{Servers: map[string]config.Server{
		"mac": {Pools: map[string]string{"system": "64GB"}, DevicePool: "system"},
	}})
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Update("Fake GPU", "m", tune.Profile{
		BaseMiB: 20000, PerSlotMiB: 0, PeakMiB: 20000, MeasuredSlots: 1,
	})
	m.SetTuneCache(c)

	mdl := config.Model{
		Server: "mac", MaxConcurrent: 1,
		RAMUsage: map[string]string{"system": "16GB"},
	}
	got := m.effectiveUsage("m", mdl)

	if want := int64(20000) * mib; got["system"] != want {
		t.Errorf("system = %d, want %d (the measured footprint)", got["system"], want)
	}
	if _, ok := got["gpu0"]; ok {
		t.Errorf("charged %d to gpu0, a pool this server does not declare", got["gpu0"])
	}
}

// THE BUG THE SPLIT FIXES, end to end.
//
// chandra-ocr-2 runs on gpu1 and has a measured profile. Once ramUsage stopped
// being declared — measurement supersedes it — nothing said which card it was
// on, so it fell back to the server-wide default pool (gpu0). The measured
// 8240 MiB then landed on the 5090's ledger while the 3080's looked free, and
// the scheduler would place another model onto memory already spoken for.
//
// `pool: gpu1` says it outright, with no size hint anywhere.
func TestEffectiveUsage_PoolChargesMeasuredToTheRightCard(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	m := NewManager(&config.Config{Servers: map[string]config.Server{
		"box1": {
			Pools:   map[string]string{"gpu0": "31654MiB", "gpu1": "9877MiB"},
			Devices: map[string]string{"gpu0": "GPU-ee90af07", "gpu1": "GPU-76a4c775"},
		},
	}})
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Update("Fake GPU", "chandra-ocr-2", tune.Profile{
		BaseMiB: 6196, PerSlotMiB: 0, PeakMiB: 8240, MeasuredSlots: 1,
	})
	m.SetTuneCache(c)

	mdl := config.Model{Server: "box1", MaxConcurrent: 1, Pool: "gpu1"} // no ramUsage
	got := m.effectiveUsage("chandra-ocr-2", mdl)

	if want := int64(8240) * mib; got["gpu1"] != want {
		t.Errorf("gpu1 = %d MiB, want %d — the card it actually runs on",
			got["gpu1"]/mib, want/mib)
	}
	if _, ok := got["gpu0"]; ok {
		t.Errorf("charged %d MiB to gpu0; that is the 5090, and this model is on the 3080",
			got["gpu0"]/mib)
	}
}

// A stale ramUsage naming the wrong card must not override an explicit `pool:`.
// This is the migration case: an operator adds `pool:` to correct a placement
// without hunting down every size hint that implied the old one.
func TestEffectiveUsage_PoolOverridesAStaleRAMUsageKey(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	m := NewManager(&config.Config{Servers: map[string]config.Server{
		"box1": {
			Pools:   map[string]string{"gpu0": "31654MiB", "gpu1": "9877MiB"},
			Devices: map[string]string{"gpu0": "GPU-ee90af07", "gpu1": "GPU-76a4c775"},
		},
	}})
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Update("Fake GPU", "moved", tune.Profile{
		BaseMiB: 6000, PerSlotMiB: 0, PeakMiB: 6000, MeasuredSlots: 1,
	})
	m.SetTuneCache(c)

	mdl := config.Model{
		Server: "box1", MaxConcurrent: 1,
		Pool:     "gpu1",
		RAMUsage: map[string]string{"gpu0": "6GB"}, // stale: it used to live here
	}
	got := m.effectiveUsage("moved", mdl)

	if want := int64(6000) * mib; got["gpu1"] != want {
		t.Errorf("gpu1 = %d MiB, want %d", got["gpu1"]/mib, want/mib)
	}
}

// vramBudget describes ONE device, so every term in it must be scoped to the
// server that device belongs to.
//
// The bug this fixes stayed invisible while there was one server and fires the
// moment there are two: a pinned model on box2 was subtracted from box1's
// budget, so box1 sized `--parallel` against memory that was never on its card.
// Two hosts each under-count by the other's footprint, and the more the second
// box holds, the fewer slots the first one gives itself.
func TestVRAMBudget_IgnoresOtherServersPinnedModels(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	m := NewManager(&config.Config{
		Servers: map[string]config.Server{
			"box1": {Pools: map[string]string{"gpu0": "60GB"}},
			"mac":  {Pools: map[string]string{"system": "64GB"}, DevicePool: "system"},
		},
		Models: map[string]config.Model{
			"target":      {Server: "box1", Cmd: "x"},
			"pinned-box1": {Server: "box1", Cmd: "x", Persistent: true, RAMUsage: map[string]string{"gpu0": "5GB"}},
			"pinned-mac":  {Server: "mac", Cmd: "x", Persistent: true, RAMUsage: map[string]string{"system": "40GB"}},
		},
	})
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	m.SetTuneCache(c)

	got := m.vramBudget(gpu.Stats{Name: "Fake GPU", TotalMiB: 60000, UsedMiB: 0}, "target")

	// Only box1's pinned model counts. Derived rather than hand-written so the
	// test asserts the scoping, not a unit convention (5GB is 5e9, not 5 GiB).
	box1Pinned, err := config.ParseSize("5GB")
	if err != nil {
		t.Fatal(err)
	}
	want := 60000 - int(box1Pinned/mib) - m.vramMargin

	if got != want {
		t.Errorf("budget = %d, want %d — the mac's 40GB pinned model was subtracted from box1's card", got, want)
	}
}
