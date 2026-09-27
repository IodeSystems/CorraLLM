package toolchain

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// A build that serves "old" and produces "new", on a fake host.
func checkedBuilder(check func(context.Context, string, string, io.Writer) (bool, error)) (*Builder, *fakeRunner) {
	b, f := builderWith(map[Verb]any{
		VerbPreflight: Preflight{OK: true},
		VerbBuilds:    BuildList{Versioned: true, Active: "old"},
		VerbBuild:     Build{OK: true, ID: "new", Version: "2"},
		VerbActivate:  Activation{OK: true, Active: "old", Previous: "new"},
	})
	b.Check = check
	return b, f
}

// activated lists the build ids Activate was asked for, in order.
func activated(f *fakeRunner) []string {
	var ids []string
	for i, v := range f.calls {
		if v == VerbActivate {
			ids = append(ids, f.specs[i].SelectBuild)
		}
	}
	return ids
}

func runOnce(t *testing.T, b *Builder) Job {
	t.Helper()
	if _, err := b.Start("llama.cpp", "box1", false); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, b)
	_, last := b.State()
	return last.Snapshot()
}

// Switch if it works, roll back if it does not: a new build whose model fails
// to start must not stay current.
func TestABuildThatFailsItsCheckIsRolledBack(t *testing.T) {
	b, f := checkedBuilder(func(context.Context, string, string, io.Writer) (bool, error) {
		return true, errors.New("exit 127: libllama-server-impl.so: cannot open shared object file")
	})
	j := runOnce(t, b)
	if got := activated(f); len(got) != 1 || got[0] != "old" {
		t.Fatalf("activate calls = %v, want one, back to the build that was serving", got)
	}
	if j.Status != "failed" {
		t.Errorf("status = %q; a rolled-back build must not read as a success", j.Status)
	}
	for _, want := range []string{"new", "rolled back to old", "libllama-server-impl.so"} {
		if !strings.Contains(j.Error, want) {
			t.Errorf("error %q does not say %q", j.Error, want)
		}
	}
}

func TestABuildThatPassesItsCheckStays(t *testing.T) {
	b, f := checkedBuilder(func(context.Context, string, string, io.Writer) (bool, error) { return true, nil })
	j := runOnce(t, b)
	if got := activated(f); len(got) != 0 {
		t.Errorf("activate called %v after a passing check", got)
	}
	if j.Status != "ok" {
		t.Errorf("status = %q, error %q", j.Status, j.Error)
	}
}

// Nothing could be run (no idle model, no room): that says nothing about the
// build, so it stays — rolling back because the box is busy would keep every
// build off a loaded machine.
func TestAnUncheckableBuildStaysAndSaysSo(t *testing.T) {
	b, f := checkedBuilder(func(context.Context, string, string, io.Writer) (bool, error) {
		return false, errors.New("no idle model on box1 runs ${tool:llama.cpp}")
	})
	if _, err := b.Start("llama.cpp", "box1", false); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, b)
	_, last := b.State()
	if got := activated(f); len(got) != 0 {
		t.Errorf("activate called %v for an unchecked build", got)
	}
	if last.Snapshot().Status != "ok" {
		t.Errorf("status = %q", last.Snapshot().Status)
	}
	lines, _ := last.LogFrom(0)
	if !strings.Contains(strings.Join(lines, "\n"), "NOT CHECKED") {
		t.Errorf("the log does not say the build went unchecked:\n%s", strings.Join(lines, "\n"))
	}
}

// A skipped build changed nothing, so there is nothing to check.
func TestASkippedBuildIsNotChecked(t *testing.T) {
	called := false
	b, _ := checkedBuilder(func(context.Context, string, string, io.Writer) (bool, error) {
		called = true
		return true, nil
	})
	b.Reg.RunnerFor = func(string) (Runner, error) {
		return &fakeRunner{answers: map[Verb]any{
			VerbPreflight: Preflight{OK: true},
			VerbBuilds:    BuildList{Versioned: true, Active: "old"},
			VerbBuild:     Build{OK: true, ID: "old", Skipped: true},
		}}, nil
	}
	runOnce(t, b)
	if called {
		t.Error("a skipped build was checked")
	}
}
