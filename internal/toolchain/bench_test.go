package toolchain

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// A build that serves "old" and produces "new", where the host can say where
// builds live (for BuildDir) and what each one links.
func benchedBuilder(bench func(ctx context.Context, tool, host, prevDir, nextDir string, log io.Writer) (func(), bool, error)) (*Builder, *fakeRunner) {
	b, f := builderWith(map[Verb]any{
		VerbPreflight: Preflight{OK: true},
		VerbProbe:     Probe{Present: true, Path: "/t/llama.cpp/bin/llama-server", Source: "stamp"},
		VerbBuilds: BuildList{Versioned: true, Active: "old", Builds: []BuildEntry{
			{ID: "new", Libs: []string{"libcublas.so.12=/usr/lib/libcublas.so.12.4.5.8", "libc.so.6=/usr/lib/libc.so.6"}},
			{ID: "old", Active: true, Libs: []string{"libcublas.so.13=/usr/local/cuda/lib/libcublas.so.13.6.0.2", "libc.so.6=/usr/lib/libc.so.6"}},
		}},
		VerbBuild:    Build{OK: true, ID: "new", Version: "2"},
		VerbActivate: Activation{OK: true, Active: "old", Previous: "new"},
	})
	b.Bench = bench
	return b, f
}

func logOf(t *testing.T, b *Builder) string {
	t.Helper()
	_, last := b.State()
	lines, _ := last.LogFrom(0)
	return strings.Join(lines, "\n")
}

// The case it exists for: a slower build is rolled back, and the pause the
// benchmark holds is lifted only AFTER bin/ points at the old build again —
// otherwise production respawns on the build being rejected.
func TestASlowerBuildIsRolledBackBeforeRelease(t *testing.T) {
	var f *fakeRunner
	var activatedAtRelease []string
	b, f := benchedBuilder(func(_ context.Context, _, _, prevDir, nextDir string, _ io.Writer) (func(), bool, error) {
		if prevDir != "/t/llama.cpp/builds/old" || nextDir != "/t/llama.cpp/builds/new" {
			t.Errorf("dirs = %q, %q", prevDir, nextDir)
		}
		return func() { activatedAtRelease = activated(f) }, true, errors.New("prefill 2020 -> 264 tok/s")
	})
	j := runOnce(t, b)
	if got := activated(f); len(got) != 1 || got[0] != "old" {
		t.Fatalf("activate calls = %v, want one, back to old", got)
	}
	if len(activatedAtRelease) != 1 {
		t.Errorf("release ran before the rollback (activations at release: %v)", activatedAtRelease)
	}
	if j.Status != "failed" || !strings.Contains(j.Error, "rolled back to old") || !strings.Contains(j.Error, "264") {
		t.Errorf("status %q error %q", j.Status, j.Error)
	}
}

func TestAFastEnoughBuildStays(t *testing.T) {
	released := false
	b, f := benchedBuilder(func(context.Context, string, string, string, string, io.Writer) (func(), bool, error) {
		return func() { released = true }, true, nil
	})
	j := runOnce(t, b)
	if got := activated(f); len(got) != 0 || j.Status != "ok" {
		t.Errorf("activations %v, status %q, error %q", got, j.Status, j.Error)
	}
	if !released {
		t.Error("the benchmark's hold was never released — production would stay paused")
	}
}

// Nothing measured says nothing about the build: it stays, and the log says
// it went unbenchmarked rather than implying it passed.
func TestAnUnbenchmarkedBuildStaysAndSaysSo(t *testing.T) {
	b, f := benchedBuilder(func(context.Context, string, string, string, string, io.Writer) (func(), bool, error) {
		return func() {}, false, errors.New("no benchmark model is placed on box1")
	})
	j := runOnce(t, b)
	if got := activated(f); len(got) != 0 || j.Status != "ok" {
		t.Errorf("activations %v, status %q", got, j.Status)
	}
	if !strings.Contains(logOf(t, b), "NOT BENCHMARKED") {
		t.Errorf("log does not say so:\n%s", logOf(t, b))
	}
}

// A build that fails its start check is not benchmarked on top of it.
func TestAFailedCheckSkipsTheBenchmark(t *testing.T) {
	benched := false
	b, _ := benchedBuilder(func(context.Context, string, string, string, string, io.Writer) (func(), bool, error) {
		benched = true
		return func() {}, true, nil
	})
	b.Check = func(context.Context, string, string, io.Writer) (bool, error) { return true, errors.New("exit 127") }
	runOnce(t, b)
	if benched {
		t.Error("benchmarked a build that already failed its check")
	}
}

// The library diff is what would have explained 2026-10-05 in one glance.
func TestTheLogShowsChangedLibraries(t *testing.T) {
	b, _ := benchedBuilder(func(context.Context, string, string, string, string, io.Writer) (func(), bool, error) {
		return func() {}, true, nil
	})
	runOnce(t, b)
	log := logOf(t, b)
	for _, want := range []string{
		"libraries CHANGED since old",
		"- libcublas.so.13=/usr/local/cuda/lib/libcublas.so.13.6.0.2",
		"+ libcublas.so.12=/usr/lib/libcublas.so.12.4.5.8",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "libc.so.6") {
		t.Error("an unchanged library is listed as a change")
	}
}

func TestBuildDirRefusesPathTricks(t *testing.T) {
	b, _ := benchedBuilder(nil)
	for _, id := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := b.Reg.BuildDir(context.Background(), "llama.cpp", "box1", id); err == nil {
			t.Errorf("BuildDir accepted %q", id)
		}
	}
}
