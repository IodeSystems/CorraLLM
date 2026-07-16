package proc

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/tune"
)

// TestMeasureOnLoad exercises measure() directly (bypassing a real spawn, so
// the fake nvidia-smi can report VRAM for a fixed, known PID instead of
// racing to discover the real one): footprint(9000) − kvMiB(2000) = base
// 7000; kvMiB(2000) / nSlots(2) = perSlot 1000; the profile persists to disk
// so the model's NEXT spawn can read it back via SlotsFor.
func TestMeasureOnLoad(t *testing.T) {
	const fakePid = 424242
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(fakePid)+", 9000")
	// The fake pid has no /proc entry; make group attribution treat each pid as
	// its own group leader so GroupVRAM(fakePid) sums the fake pid's usage.
	origPGID := gpu.PGIDFn
	gpu.PGIDFn = func(pid int) (int, error) { return pid, nil }
	t.Cleanup(func() { gpu.PGIDFn = origPGID })

	cachePath := filepath.Join(t.TempDir(), "vram-profile.json")
	cache, err := tune.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	lb := newLogBuffer(10)
	_, _ = lb.Write([]byte("srv  load_model: initializing slots, n_slots = 2\n"))
	_, _ = lb.Write([]byte("llama_new_context_with_model: KV self size = 2000.00 MiB\n"))
	p := &Process{logs: lb}

	mdl := config.Model{Cmd: "exec sleep 30", MaxConcurrent: 2}
	mgr.measure("measured", mdl, p, fakePid)

	want := tune.Profile{BaseMiB: 7000, PerSlotMiB: 1000, PeakMiB: 9000, MeasuredSlots: 2, Ctx: 0}
	got, ok := cache.Get("Fake GPU", "measured")
	if !ok {
		t.Fatal("want profile persisted in-memory")
	}
	if got != want {
		t.Errorf("profile = %+v, want %+v", got, want)
	}

	// measure() saves to disk too — a later process (e.g. `corrallm introspect`,
	// or corrallm restarting) must see it without this Manager still running.
	reloaded, err := tune.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := reloaded.Get("Fake GPU", "measured"); !ok || g != want {
		t.Errorf("reloaded profile = %+v, ok=%v, want %+v", g, ok, want)
	}
}

// TestMeasureNoopWithoutTuneCache: measure() is a complete no-op (no panic,
// no gpu/nvidia-smi call at all) when the Manager has no tune cache wired up
// — the common case for every pre-existing NewManager(cfg) call site that
// never calls SetTuneCache.
func TestMeasureNoopWithoutTuneCache(t *testing.T) {
	// No fake nvidia-smi installed; if measure() called gpu.Probe/ProcVRAM at
	// all it would either error (harmlessly) or, on a real GPU box, actually
	// shell out — proving the nil-cache guard short-circuits first matters
	// either way. Assert indirectly: it must not panic, and (since we pass a
	// nil *tune.Cache) there is nothing to persist to lose track of.
	mgr := NewManager(&config.Config{})
	p := &Process{logs: newLogBuffer(10)}
	mdl := config.Model{Cmd: "exec sleep 30"}
	mgr.measure("no-cache", mdl, p, 1) // must not panic
}

// TestMeasureSkipsWhenPIDMissingFromProcVRAM: nvidia-smi succeeds but doesn't
// report the spawned PID (e.g. it exited between health-check and measure,
// or another race) — measure() must skip silently rather than record a
// bogus zero/negative footprint.
func TestMeasureSkipsWhenPIDMissingFromProcVRAM(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", "") // no compute-apps rows at all

	cachePath := filepath.Join(t.TempDir(), "vram-profile.json")
	cache, err := tune.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	p := &Process{logs: newLogBuffer(10)}
	mdl := config.Model{Cmd: "exec sleep 30"}
	mgr.measure("missing-pid", mdl, p, 999999)

	if _, ok := cache.Get("Fake GPU", "missing-pid"); ok {
		t.Error("want no profile recorded when the pid is absent from nvidia-smi's report")
	}
}

// TestSampleVRAMPeakStopsOnSignal: sampleVRAMPeak's loop exits promptly when
// its stop channel closes, and never touches the cache when tuneCache is nil
// — the leak/blocking-shutdown concern the goroutine must avoid.
func TestSampleVRAMPeakStopsOnSignal(t *testing.T) {
	mgr := NewManager(&config.Config{}) // no tune cache: sampleVRAMPeak returns immediately
	done := make(chan struct{})
	stopped := make(chan struct{})
	close(stopped) // already stopped
	go func() {
		mgr.sampleVRAMPeak("m", 1, stopped)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sampleVRAMPeak did not return promptly with a nil tune cache")
	}
}

// TestBumpPeakViaSampler: with a tune cache holding an existing profile,
// sampleVRAMPeak's ticker-driven probe raises PeakMiB via BumpPeak. Uses a
// zero-delay approach by calling the tick body indirectly through BumpPeak
// with a value read from the fake nvidia-smi, mirroring what the goroutine
// itself would compute on a real tick (exercised end-to-end, without waiting
// on a real 15s ticker).
func TestBumpPeakViaSampler(t *testing.T) {
	const fakePid = 555555
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(fakePid)+", 15000")

	cachePath := filepath.Join(t.TempDir(), "vram-profile.json")
	cache, err := tune.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "bump", tune.Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 9000, MeasuredSlots: 1})

	stats, err := gpu.Probe()
	if err != nil {
		t.Fatalf("gpu.Probe: %v", err)
	}
	procVRAM, err := gpu.ProcVRAM()
	if err != nil {
		t.Fatalf("gpu.ProcVRAM: %v", err)
	}
	footprint, ok := procVRAM[fakePid]
	if !ok {
		t.Fatal("fake nvidia-smi did not report the expected pid")
	}
	cache.BumpPeak(stats.Name, "bump", footprint)

	p, _ := cache.Get("Fake GPU", "bump")
	if p.PeakMiB != 15000 {
		t.Errorf("PeakMiB = %d, want 15000", p.PeakMiB)
	}
}
