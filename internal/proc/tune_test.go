package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/tune"
)

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
	fakeNvidiaSMI(t, "0, Fake GPU, 32000, 20000, 40098", "")

	port := listenTCP(t)
	mgr := NewManager(&config.Config{})
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	cache, err := tune.New(filepath.Join(t.TempDir(), "vram-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	// budget = 40098 - 512 = 39586; growth = max(0, 20000-(10000+2*5000))=0;
	// n = (39586-10000-0)/5000 = 5
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
