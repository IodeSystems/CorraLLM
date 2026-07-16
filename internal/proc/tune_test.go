package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/tune"
)

// TestVramBudgetPostEviction: the budget for a model is total − pre-crowded −
// persistent (non-evictable) footprints − margin. Evictable residents are NOT
// subtracted, and the model being sized excludes ITSELF from non-evictable.
func TestVramBudgetPostEviction(t *testing.T) {
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "embed", tune.Profile{BaseMiB: 2000, PeakMiB: 2000, MeasuredSlots: 1})
	mgr := NewManager(&config.Config{Models: map[string]config.Model{
		"embed": {Persistent: true}, // pinned → non-evictable
		"chat":  {},                 // sticky/evictable → not subtracted
	}})
	mgr.SetTuneCache(cache)
	mgr.SetVRAMMargin(512)

	// No resident procs → ownUsed 0 → preCrowded == UsedMiB.
	stats := gpu.Stats{Name: "Fake GPU", TotalMiB: 32000, UsedMiB: 8000, FreeMiB: 24000}
	if b := mgr.vramBudget(stats, "chat"); b != 32000-8000-2000-512 {
		t.Errorf("chat budget = %d, want %d (persistent embed subtracted)", b, 32000-8000-2000-512)
	}
	if b := mgr.vramBudget(stats, "embed"); b != 32000-8000-0-512 {
		t.Errorf("embed budget = %d, want %d (self excluded from non-evictable)", b, 32000-8000-512)
	}
}

// fakeNvidiaSMI puts a fake `nvidia-smi` script FIRST on PATH for the
// duration of the test (the rest of PATH is kept, so `sh` and everything else
// `load()` needs still resolves) — gpu.Probe/gpu.ProcVRAM shell out via
// os/exec + PATH lookup, so this makes their output deterministic regardless
// of whether the test host actually has an NVIDIA GPU.
func fakeNvidiaSMI(t *testing.T, gpuLine, procLine string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --query-gpu=*) echo '" + gpuLine + "' ;;\n" +
		"  --query-compute-apps=*) echo '" + procLine + "' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeNvidiaSMIFailing makes nvidia-smi resolvable but always fail (nonzero
// exit) — simulates "installed but errors" deterministically, distinct from
// "not on PATH at all" but exercising the same gpu.Probe-returns-error path.
func fakeNvidiaSMIFailing(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestFailSafeNoTuningWithoutProfile is the single most important test in
// this diff: with the tune cache left at its zero-value default (nil — no
// SetTuneCache call, as every pre-existing NewManager(cfg) call site still
// does), a model's cmd — including a --parallel flag baked into it — is
// spawned BYTE-IDENTICAL to its configured Cmd, and admission (TunedSlots)
// falls back to the config default. A bug anywhere in the tuning mechanism
// can never reach a spawn when introspection was never opted into.
func TestFailSafeNoTuningWithoutProfile(t *testing.T) {
	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	// mgr.tuneCache is nil (zero value) — introspection never wired up.

	mdl := modelCmd(t, "exec sleep 30 --parallel 2", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "untuned-nocache", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.cmd.Args[2]; got != mdl.Cmd {
		t.Fatalf("spawned cmd = %q, want byte-identical to configured %q", got, mdl.Cmd)
	}
	if p.tunedSlots != 0 {
		t.Errorf("tunedSlots = %d, want 0 (untuned)", p.tunedSlots)
	}
	if got := mgr.TunedSlots("untuned-nocache", 2); got != 2 {
		t.Errorf("TunedSlots = %d, want config default 2", got)
	}
}

// TestFailSafeNoProfileForModel: a tune cache IS wired up (so gpu.Probe runs)
// but holds no entry for this model — SlotsFor reports ok=false, so the cmd
// is still spawned unchanged. Proves an EMPTY cache (fresh box, nothing
// measured yet) is exactly as safe as no cache at all.
func TestFailSafeNoProfileForModel(t *testing.T) {
	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetTuneCache(cache) // empty: no profile for any model

	mdl := modelCmd(t, "exec sleep 30 --parallel 2", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "untuned-empty-cache", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.cmd.Args[2]; got != mdl.Cmd {
		t.Fatalf("spawned cmd = %q, want byte-identical to configured %q", got, mdl.Cmd)
	}
	if p.tunedSlots != 0 {
		t.Errorf("tunedSlots = %d, want 0 (no profile)", p.tunedSlots)
	}
}

// TestFailSafeGPUProbeUnavailable: a cached profile EXISTS for this model,
// but nvidia-smi errors at spawn time — tuning is skipped entirely (never
// partially applied) and the cmd is spawned unchanged.
func TestFailSafeGPUProbeUnavailable(t *testing.T) {
	fakeNvidiaSMIFailing(t)

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "untuned-nogpu", tune.Profile{BaseMiB: 100, PerSlotMiB: 100, PeakMiB: 100, MeasuredSlots: 1})
	mgr.SetTuneCache(cache)

	mdl := modelCmd(t, "exec sleep 30 --parallel 2", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "untuned-nogpu", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.cmd.Args[2]; got != mdl.Cmd {
		t.Fatalf("spawned cmd = %q, want byte-identical to configured %q", got, mdl.Cmd)
	}
	if p.tunedSlots != 0 {
		t.Errorf("tunedSlots = %d, want 0 (gpu unavailable)", p.tunedSlots)
	}
}

// TestFailSafeNoParallelFlagLeftUntouched: a cached profile and a healthy GPU
// both say "tune", but the configured cmd has no --parallel flag to rewrite —
// spec is explicit that this must NOT inject one; the cmd is left completely
// untouched.
func TestFailSafeNoParallelFlagLeftUntouched(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "no-parallel-flag", tune.Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5000, MeasuredSlots: 1})
	mgr.SetTuneCache(cache)

	mdl := modelCmd(t, "exec sleep 30 --ctx-size 4096", port) // no --parallel at all
	p, _, _, err := mgr.EnsureReady(context.Background(), "no-parallel-flag", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.cmd.Args[2]; got != mdl.Cmd {
		t.Fatalf("spawned cmd = %q, want byte-identical to configured %q (no --parallel to rewrite)", got, mdl.Cmd)
	}
	if p.tunedSlots != 0 {
		t.Errorf("tunedSlots = %d, want 0 (nothing to rewrite)", p.tunedSlots)
	}
}

// TestParallelRewriteWithProfile: with a cached profile and a healthy GPU,
// --parallel N is rewritten to the tuned slot count and the rest of the
// command line is left untouched. Budget = FreeMiB(12000) - default
// margin(512) = 11488; profile base=4000 per_slot=1000 peak=5500
// measuredSlots=1 -> growth=max(0, 5500-(4000+1*1000))=500 ->
// n=(11488-4000-500)/1000=6.
func TestParallelRewriteWithProfile(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "tuned", tune.Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5500, MeasuredSlots: 1})
	mgr.SetTuneCache(cache)

	// `true` swallows the --parallel/--ctx-size flags harmlessly (a real
	// llama-server wouldn't) so the exec'd sleep at the end starts clean and
	// stays resident long enough for the mgr.TunedSlots lookup below — the
	// regex rewrite itself doesn't care what the surrounding command does.
	mdl := modelCmd(t, "true --parallel 2 --ctx-size 4096 && exec sleep 30", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "tuned", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	want := "true --parallel 6 --ctx-size 4096 && exec sleep 30"
	if got := p.cmd.Args[2]; got != want {
		t.Fatalf("spawned cmd = %q, want %q", got, want)
	}
	if p.tunedSlots != 6 {
		t.Errorf("tunedSlots = %d, want 6", p.tunedSlots)
	}
	if got := mgr.TunedSlots("tuned", 2); got != 6 {
		t.Errorf("TunedSlots = %d, want 6", got)
	}
	// The original config.Model's Cmd string must be untouched (tuneCmd copies
	// before rewriting) — a shared *config.Config used across many spawns must
	// not have its cmd mutated by tuning one of them.
	if mdl.Cmd != "true --parallel 2 --ctx-size 4096 && exec sleep 30" {
		t.Errorf("mdl.Cmd mutated: %q", mdl.Cmd)
	}
}

// TestParallelRewritePreservesRestOfCmd verifies the regex only touches the
// --parallel token, including when it appears mid-string with other flags on
// both sides and a different original slot count.
func TestParallelRewritePreservesRestOfCmd(t *testing.T) {
	fakeNvidiaSMI(t, "0, Fake GPU, 60098, 20000, 40098", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	// post-eviction budget = total(60098) - preCrowded(20000 used, no corrallm
	// residents) - nonEvictable(0, no persistent models) - margin(512) = 39586;
	// growth = max(0, 20000-(10000+2*5000)) = 0; n = (39586-10000-0)/5000 = 5
	cache.Update("Fake GPU", "wrapped", tune.Profile{BaseMiB: 10000, PerSlotMiB: 5000, PeakMiB: 20000, MeasuredSlots: 2})
	mgr.SetTuneCache(cache)

	mdl := modelCmd(t, "exec llama-server -m model.gguf -ngl 60 --parallel 2 --host 0.0.0.0", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "wrapped", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	want := "exec llama-server -m model.gguf -ngl 60 --parallel 5 --host 0.0.0.0"
	if got := p.cmd.Args[2]; got != want {
		t.Fatalf("spawned cmd = %q, want %q", got, want)
	}
}

// --- calibration probe (two-point empirical KV measurement) ---
//
// A profile with exactly one measured slot count can't derive PerSlotMiB via
// SlotsFor (it needs the slope between TWO distinct slot counts). These
// tests cover calibrationProbe's safety gate directly: footprint(k+1) <=
// footprint(k)*(k+1)/k always holds when BaseMiB>=0, so probing k+1 is safe
// exactly when budget covers that worst case — for k=2, footprint(k)=6000,
// the bound is ceil(6000*3/2)=9000.

// TestCalibrationProbeSafeWhenBudgetCoversWorstCase: budget exactly at the
// worst-case bound (9000) is sufficient — probe k+1=3.
func TestCalibrationProbeSafeWhenBudgetCoversWorstCase(t *testing.T) {
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "probe-me", tune.Profile{
		BaseMiB: 6000, PerSlotMiB: 0, PeakMiB: 6000, MeasuredSlots: 2,
		Samples: []tune.Sample{{Slots: 2, FootprintMiB: 6000}},
	})
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	n, ok := mgr.calibrationProbe(gpu.Stats{Name: "Fake GPU"}, 9000, "probe-me")
	if !ok {
		t.Fatal("want ok=true: budget exactly covers the worst-case bound")
	}
	if n != 3 {
		t.Errorf("n = %d, want 3 (k+1)", n)
	}
}

// TestCalibrationProbeRefusesUnderBudget: one MiB short of the worst-case
// bound must refuse to probe — fail-safe: if it's not provably safe, don't.
func TestCalibrationProbeRefusesUnderBudget(t *testing.T) {
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "probe-me", tune.Profile{
		BaseMiB: 6000, PerSlotMiB: 0, PeakMiB: 6000, MeasuredSlots: 2,
		Samples: []tune.Sample{{Slots: 2, FootprintMiB: 6000}},
	})
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	if n, ok := mgr.calibrationProbe(gpu.Stats{Name: "Fake GPU"}, 8999, "probe-me"); ok {
		t.Errorf("want ok=false when budget(8999) < worst-case(9000); got n=%d", n)
	}
}

// TestCalibrationProbeRefusesWhenAlreadyTuned: PerSlotMiB already known
// (>0, from the KV log or a prior 2-point derivation) — nothing to
// calibrate, regardless of how much budget is available.
func TestCalibrationProbeRefusesWhenAlreadyTuned(t *testing.T) {
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "tuned", tune.Profile{
		BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5000, MeasuredSlots: 1,
		Samples: []tune.Sample{{Slots: 1, FootprintMiB: 5000}},
	})
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	if n, ok := mgr.calibrationProbe(gpu.Stats{Name: "Fake GPU"}, 1_000_000, "tuned"); ok {
		t.Errorf("want ok=false when PerSlotMiB already known; got n=%d", n)
	}
}

// TestCalibrationProbeRefusesWithoutProfile: no profile at all for the
// model — nothing to calibrate from.
func TestCalibrationProbeRefusesWithoutProfile(t *testing.T) {
	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&config.Config{})
	mgr.SetTuneCache(cache)

	if n, ok := mgr.calibrationProbe(gpu.Stats{Name: "Fake GPU"}, 1_000_000, "never-measured"); ok {
		t.Errorf("want ok=false without a profile; got n=%d", n)
	}
}

// TestCalibrationProbeEndToEnd: wired through tuneCmd/EnsureReady, a
// one-sample profile with a safely-covering budget causes --parallel to be
// rewritten to k+1 (NOT the config's own --parallel N) — the calibration
// spawn that gives the next measurement the second slot-count point it
// needs to derive PerSlotMiB via slope.
func TestCalibrationProbeEndToEnd(t *testing.T) {
	// budget = FreeMiB(12000) - margin(512) = 11488, comfortably clears the
	// worst-case bound (9000) for a k=2, footprint=6000 profile.
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 12000", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "calibrate", tune.Profile{
		BaseMiB: 6000, PerSlotMiB: 0, PeakMiB: 6000, MeasuredSlots: 2,
		Samples: []tune.Sample{{Slots: 2, FootprintMiB: 6000}},
	})
	mgr.SetTuneCache(cache)

	mdl := modelCmd(t, "exec sleep 30 --parallel 2", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "calibrate", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	want := "exec sleep 30 --parallel 3"
	if got := p.cmd.Args[2]; got != want {
		t.Fatalf("spawned cmd = %q, want %q (calibration probe to k+1)", got, want)
	}
	if p.tunedSlots != 3 {
		t.Errorf("tunedSlots = %d, want 3", p.tunedSlots)
	}
}

// TestCalibrationProbeEndToEndUnsafeLeavesCmdUntouched: same one-sample
// profile, but budget too tight to safely cover k+1 — the config cmd is
// spawned byte-identical, exactly like every other fail-safe path (probing
// only ever RAISES --parallel within a provably-safe bound; if uncertain,
// don't).
func TestCalibrationProbeEndToEndUnsafeLeavesCmdUntouched(t *testing.T) {
	// budget = 32000 - 22500 - margin(512) = 8988 < worst-case bound (9000)
	// for a k=2, footprint=6000 profile.
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 22500, 9500", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	cache.Update("Fake GPU", "calibrate-unsafe", tune.Profile{
		BaseMiB: 6000, PerSlotMiB: 0, PeakMiB: 6000, MeasuredSlots: 2,
		Samples: []tune.Sample{{Slots: 2, FootprintMiB: 6000}},
	})
	mgr.SetTuneCache(cache)

	mdl := modelCmd(t, "exec sleep 30 --parallel 2", port)
	p, _, _, err := mgr.EnsureReady(context.Background(), "calibrate-unsafe", mdl, nil)
	if err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if got := p.cmd.Args[2]; got != mdl.Cmd {
		t.Fatalf("spawned cmd = %q, want byte-identical to configured %q (probe unsafe)", got, mdl.Cmd)
	}
	if p.tunedSlots != 0 {
		t.Errorf("tunedSlots = %d, want 0 (probe refused)", p.tunedSlots)
	}
}
