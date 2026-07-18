package proc

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/tune"
)

// The tuner may LOWER slots to fit VRAM but must never RAISE them above the
// configured maxConcurrent.
//
// Two independent reasons: slots beyond maxConcurrent are unreachable (the
// scheduler admits at most that many concurrent requests), and --parallel
// DIVIDES the context window, so extra slots silently shrink a context the
// operator explicitly configured. Observed live: gemma-4-12b with -c 131072 and
// maxConcurrent 2 was spawned at --parallel 32 because 32 slots fit in VRAM,
// giving every request n_ctx_slot=4096 — a 32x cut with no error anywhere.
func TestTuneCmd_NeverExceedsMaxConcurrent(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	cache, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	// A profile so cheap per slot that the VRAM math wants far more than 2.
	cache.Update("Fake GPU", "roomy", tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 10, PeakMiB: 1200, MeasuredSlots: 1,
		Source: tune.SourceBench,
	})

	cfg := &config.Config{Models: map[string]config.Model{
		"roomy": {Cmd: "llama-server --parallel 2", MaxConcurrent: 2},
	}}
	m := NewManager(cfg)
	m.SetTuneCache(cache)

	cmd := "llama-server --parallel 2"
	n := m.tuneCmd("roomy", &cmd, 2, 0)
	if n > 2 {
		t.Errorf("tuner raised slots to %d above maxConcurrent 2 — unreachable concurrency bought with context window", n)
	}
	if n > 0 && !contains(cmd, "--parallel 2") {
		t.Errorf("cmd should stay at the configured cap, got %q", cmd)
	}
}

// Lowering is still allowed: a model that cannot fit its configured slots must
// come down, or it will not fit at all.
func TestTuneCmd_StillLowersWhenVRAMIsTight(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	cache, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	// Expensive per slot: the budget will not cover the configured 8.
	cache.Update("Fake GPU", "hungry", tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 100000, PeakMiB: 1000, MeasuredSlots: 1,
		Source: tune.SourceBench,
	})
	cfg := &config.Config{Models: map[string]config.Model{
		"hungry": {Cmd: "llama-server --parallel 8", MaxConcurrent: 8},
	}}
	m := NewManager(cfg)
	m.SetTuneCache(cache)

	cmd := "llama-server --parallel 8"
	n := m.tuneCmd("hungry", &cmd, 8, 0)
	if n > 1 {
		t.Errorf("tuner should have lowered slots for a model that cannot fit 8, got %d", n)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
