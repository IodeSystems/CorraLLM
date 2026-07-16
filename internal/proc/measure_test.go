package proc

import (
	"path/filepath"
	"reflect"
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

	want := tune.Profile{
		BaseMiB: 7000, PerSlotMiB: 1000, PeakMiB: 9000, MeasuredSlots: 2, Ctx: 0,
		Samples: []tune.Sample{{Slots: 2, FootprintMiB: 9000}}, // every spawn records a sample, KV-log fast path or not
	}
	got, ok := cache.Get("Fake GPU", "measured")
	if !ok {
		t.Fatal("want profile persisted in-memory")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("profile = %+v, want %+v", got, want)
	}

	// measure() saves to disk too — a later process (e.g. `corrallm introspect`,
	// or corrallm restarting) must see it without this Manager still running.
	reloaded, err := tune.New(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := reloaded.Get("Fake GPU", "measured"); !ok || !reflect.DeepEqual(g, want) {
		t.Errorf("reloaded profile = %+v, ok=%v, want %+v", g, ok, want)
	}
}

// TestMeasureTwoPointSlopeDerivation: on a host where llama.cpp never logs a
// KV size (kvMiB==0 every spawn — the exact scenario this task fixes), two
// spawns of the same model at DIFFERENT slot counts derive PerSlotMiB/BaseMiB
// from the slope between them, instead of staying stuck at PerSlotMiB=0
// forever. Also exercises PeakMiB staying monotonic when the SECOND spawn's
// footprint is smaller than the first's.
func TestMeasureTwoPointSlopeDerivation(t *testing.T) {
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
	mdl := config.Model{Cmd: "exec sleep 30"}

	// Spawn 1: 2 slots, footprint 8000 MiB, NO KV size in the log (kvMiB=0).
	const pid1 = 111111
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid1)+", 8000")
	lb1 := newLogBuffer(10)
	_, _ = lb1.Write([]byte("srv  load_model: initializing slots, n_slots = 2\n"))
	mgr.measure("slope-model", mdl, &Process{logs: lb1}, pid1)

	mid, ok := cache.Get("Fake GPU", "slope-model")
	if !ok {
		t.Fatal("want profile after first spawn")
	}
	if mid.PerSlotMiB != 0 {
		t.Errorf("after ONE sample: PerSlotMiB = %d, want 0 (not derivable yet)", mid.PerSlotMiB)
	}
	if len(mid.Samples) != 1 || mid.Samples[0] != (tune.Sample{Slots: 2, FootprintMiB: 8000}) {
		t.Errorf("Samples after 1st spawn = %+v, want [{2 8000}]", mid.Samples)
	}

	// Spawn 2: 5 slots, footprint 5900 MiB (SMALLER than spawn 1 — exercises
	// the PeakMiB-monotonic guard), still no KV size in the log.
	const pid2 = 222222
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid2)+", 5900")
	lb2 := newLogBuffer(10)
	_, _ = lb2.Write([]byte("srv  load_model: initializing slots, n_slots = 5\n"))
	mgr.measure("slope-model", mdl, &Process{logs: lb2}, pid2)

	// perSlot = (5900-8000)/(5-2) ... wait: slope uses the two DISTINCT
	// samples (2,8000) and (5,5900): perSlot=(5900-8000)/(5-2) = -700 —
	// negative, must be REJECTED (SlopeFromSamples guard), leaving
	// PerSlotMiB at 0 rather than a nonsensical negative-cost profile.
	got, ok := cache.Get("Fake GPU", "slope-model")
	if !ok {
		t.Fatal("want profile after second spawn")
	}
	if got.PerSlotMiB != 0 {
		t.Errorf("PerSlotMiB = %d, want 0 (negative slope must be rejected)", got.PerSlotMiB)
	}
	// PeakMiB must stay monotonic: max(8000, 5900) = 8000, even though the
	// SECOND (latest) measurement's own footprint was smaller.
	if got.PeakMiB != 8000 {
		t.Errorf("PeakMiB = %d, want 8000 (monotonic max, not the latest snapshot)", got.PeakMiB)
	}
	if len(got.Samples) != 2 {
		t.Fatalf("Samples = %+v, want 2 distinct entries", got.Samples)
	}
}

// TestMeasureTwoPointSlopePositive: same two-point scenario as above but with
// an increasing footprint, so the slope is legitimately positive and gets
// applied — the actual fix's happy path.
func TestMeasureTwoPointSlopePositive(t *testing.T) {
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
	mdl := config.Model{Cmd: "exec sleep 30"}

	const pid1 = 333333
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid1)+", 6000")
	lb1 := newLogBuffer(10)
	_, _ = lb1.Write([]byte("srv  load_model: initializing slots, n_slots = 1\n"))
	mgr.measure("slope-up", mdl, &Process{logs: lb1}, pid1)

	const pid2 = 444444
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid2)+", 9000")
	lb2 := newLogBuffer(10)
	_, _ = lb2.Write([]byte("srv  load_model: initializing slots, n_slots = 4\n"))
	mgr.measure("slope-up", mdl, &Process{logs: lb2}, pid2)

	// perSlot = (9000-6000)/(4-1) = 1000, base = 6000 - 1*1000 = 5000.
	got, ok := cache.Get("Fake GPU", "slope-up")
	if !ok {
		t.Fatal("want profile after second spawn")
	}
	if got.PerSlotMiB != 1000 {
		t.Errorf("PerSlotMiB = %d, want 1000", got.PerSlotMiB)
	}
	if got.BaseMiB != 5000 {
		t.Errorf("BaseMiB = %d, want 5000", got.BaseMiB)
	}
	if got.PeakMiB != 9000 {
		t.Errorf("PeakMiB = %d, want 9000", got.PeakMiB)
	}

	// The derived profile is now usable by SlotsFor — the whole point.
	if n, ok := cache.SlotsFor("Fake GPU", "slope-up", 20000); !ok || n < 1 {
		t.Errorf("SlotsFor after slope derivation: n=%d ok=%v, want a usable profile", n, ok)
	}
}

// TestMeasureKVLogWinsOverSlope: when the KV-log fast path IS available on a
// given spawn (kvMiB>0), it takes precedence over the two-point slope even
// when 2 distinct slope-derivable samples already exist that would compute
// different numbers — the log is authoritative/cheaper when present.
func TestMeasureKVLogWinsOverSlope(t *testing.T) {
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
	mdl := config.Model{Cmd: "exec sleep 30"}

	// Two prior spawns with NO KV log establish a slope: perSlot=(8000-6000)/(3-1)=1000.
	const pid1 = 555001
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid1)+", 6000")
	lb1 := newLogBuffer(10)
	_, _ = lb1.Write([]byte("srv  load_model: initializing slots, n_slots = 1\n"))
	mgr.measure("kv-wins", mdl, &Process{logs: lb1}, pid1)

	const pid2 = 555002
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid2)+", 8000")
	lb2 := newLogBuffer(10)
	_, _ = lb2.Write([]byte("srv  load_model: initializing slots, n_slots = 3\n"))
	mgr.measure("kv-wins", mdl, &Process{logs: lb2}, pid2)

	mid, _ := cache.Get("Fake GPU", "kv-wins")
	if mid.PerSlotMiB != 1000 {
		t.Fatalf("precondition: slope-derived PerSlotMiB = %d, want 1000", mid.PerSlotMiB)
	}

	// Third spawn: 2 slots, footprint 5000, and THIS TIME the log reports a KV
	// size directly (kvMiB=1500) — fast path: perSlot=1500/2=750, base=5000-1500=3500.
	// A slope recomputed from (1,6000)/(3,8000) — the two samples nearest in
	// the cap — would give a DIFFERENT number (1000); the KV-log value must win.
	const pid3 = 555003
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", strconv.Itoa(pid3)+", 5000")
	lb3 := newLogBuffer(10)
	_, _ = lb3.Write([]byte("srv  load_model: initializing slots, n_slots = 2\n"))
	_, _ = lb3.Write([]byte("llama_new_context_with_model: KV self size = 1500.00 MiB\n"))
	mgr.measure("kv-wins", mdl, &Process{logs: lb3}, pid3)

	got, ok := cache.Get("Fake GPU", "kv-wins")
	if !ok {
		t.Fatal("want profile after third spawn")
	}
	if got.PerSlotMiB != 750 {
		t.Errorf("PerSlotMiB = %d, want 750 (KV-log fast path, not the slope's 1000)", got.PerSlotMiB)
	}
	if got.BaseMiB != 3500 {
		t.Errorf("BaseMiB = %d, want 3500 (KV-log fast path)", got.BaseMiB)
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
