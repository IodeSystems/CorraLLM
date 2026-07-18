package proc

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
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
