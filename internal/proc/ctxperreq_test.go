package proc

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/tune"
)

func ctxCache(t *testing.T, p tune.Profile) *tune.Cache {
	t.Helper()
	// tuneCmd bails out entirely when the GPU is not probeable, which would make
	// every assertion below pass vacuously against slots=0.
	fakeNvidiaSMI(t, "0, Fake GPU, 60000, 0, 60000", "")
	c, err := tune.New(t.TempDir() + "/vram-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	c.Update("Fake GPU", "m", p)
	return c
}

// The inversion: --ctx-size is llama.cpp's TOTAL, divided across slots. With
// contextPerRequest declared, corrallm multiplies it back up so each request
// actually gets the window the operator asked for.
//
// Without this, `-c 131072 --parallel 2` gives every request 65536 — a silent
// halving that no error reports and the config does not show.
func TestTuneCmd_ContextPerRequestScalesTotal(t *testing.T) {
	// Cheap KV so slots are not the binding constraint here.
	cache := ctxCache(t, tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 100, PeakMiB: 1100,
		MeasuredSlots: 1, Ctx: 100000, Source: tune.SourceBench,
	})
	m := NewManager(&config.Config{})
	m.SetTuneCache(cache)

	cmd := "llama-server -c 131072 --parallel 2"
	n := m.tuneCmd("m", &cmd, 2, 131072)
	if n != 2 {
		t.Fatalf("slots = %d, want 2", n)
	}
	if !strings.Contains(cmd, "-c 262144") {
		t.Errorf("total ctx should be perRequest*slots = 262144, got %q", cmd)
	}
}

// Context is the invariant; SLOTS are what give. A window too large for the
// configured concurrency must reduce slots, never the window.
func TestTuneCmd_ReducesSlotsToPreserveContext(t *testing.T) {
	// perToken = 500 MiB / (100000/1) = 0.005 MiB per token.
	// At 200000 tokens per slot that is 1000 MiB per slot.
	cache := ctxCache(t, tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 500, PeakMiB: 1500,
		MeasuredSlots: 1, Ctx: 100000, Source: tune.SourceBench,
	})
	m := NewManager(&config.Config{})
	m.SetTuneCache(cache)
	m.vramMargin = 0

	cmd := "llama-server -c 200000 --parallel 4"
	n := m.tuneCmd("m", &cmd, 4, 200000)
	if n > 4 {
		t.Fatalf("slots = %d, must not exceed maxConcurrent", n)
	}
	// Whatever slot count survives, the per-request window must be intact:
	// total must be exactly perRequest * slots.
	want := 200000 * n
	if !strings.Contains(cmd, "-c "+itoa(want)) {
		t.Errorf("cmd %q should carry -c %d (perRequest*slots), context must never be the thing reduced", cmd, want)
	}
}

// A model with no contextPerRequest keeps llama.cpp's native semantics, so
// existing configs are untouched until they opt in.
func TestTuneCmd_NoContextPerRequestLeavesCtxAlone(t *testing.T) {
	cache := ctxCache(t, tune.Profile{
		BaseMiB: 1000, PerSlotMiB: 100, PeakMiB: 1100,
		MeasuredSlots: 1, Ctx: 100000, Source: tune.SourceBench,
	})
	m := NewManager(&config.Config{})
	m.SetTuneCache(cache)

	cmd := "llama-server -c 131072 --parallel 2"
	m.tuneCmd("m", &cmd, 2, 0)
	if !strings.Contains(cmd, "-c 131072") {
		t.Errorf("ctx must be untouched without contextPerRequest, got %q", cmd)
	}
}

// KVMiBPerToken must refuse to guess: a profile with no recorded Ctx cannot
// support a per-token estimate, and inventing one would size VRAM off a number
// nobody measured.
func TestKVMiBPerToken_RefusesWithoutCtx(t *testing.T) {
	if got := (tune.Profile{PerSlotMiB: 500, MeasuredSlots: 1}).KVMiBPerToken(); got != 0 {
		t.Errorf("no Ctx recorded should yield 0, got %v", got)
	}
	if got := (tune.Profile{PerSlotMiB: 500, MeasuredSlots: 1, Ctx: 100000}).KVMiBPerToken(); got != 0.005 {
		t.Errorf("perToken = %v, want 0.005", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
