package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touch writes a file with an explicit modtime.
func touch(t *testing.T, dir, name string, mod time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNewestBuildInputFindsChangedSource: a .go file newer than the binary is
// what the warning is for.
func TestNewestBuildInputFindsChangedSource(t *testing.T) {
	dir := t.TempDir()
	built := time.Now().Add(-time.Hour)
	touch(t, dir, "old.go", built.Add(-time.Hour))
	want := touch(t, dir, "internal/thing/new.go", built.Add(time.Minute))
	_ = want

	got, name := newestBuildInput(dir, built)
	if got.IsZero() {
		t.Fatal("no newer source found, but one is newer than the binary")
	}
	if name != filepath.Join("internal", "thing", "new.go") {
		t.Errorf("named %q, want the relative path of the newest file", name)
	}
}

// TestNewestBuildInputQuietWhenUpToDate: no warning when nothing changed —
// a check that always fires is one people learn to ignore.
func TestNewestBuildInputQuietWhenUpToDate(t *testing.T) {
	dir := t.TempDir()
	built := time.Now()
	touch(t, dir, "a.go", built.Add(-time.Hour))
	touch(t, dir, "go.mod", built.Add(-time.Minute))

	if got, name := newestBuildInput(dir, built); !got.IsZero() {
		t.Errorf("claimed stale against %q, but the binary is newer than everything", name)
	}
}

// TestBuildInputIgnoresTheUI: the dashboard is served from --web-root as files
// on disk, not compiled in, so a changed .tsx does not make the binary stale.
// Flagging it would cry wolf on every dashboard edit.
func TestBuildInputIgnoresTheUI(t *testing.T) {
	for _, n := range []string{"index.tsx", "app.css", "schema.graphql", "README.md"} {
		if buildInput(n) {
			t.Errorf("%s counted as a build input", n)
		}
	}
	for _, n := range []string{"main.go", "go.mod", "go.sum", "go.work"} {
		if !buildInput(n) {
			t.Errorf("%s should count as a build input", n)
		}
	}
}

// TestNewestBuildInputSkipsNoise: build output and vendored trees change
// constantly and say nothing about whether this binary is current.
func TestNewestBuildInputSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	built := time.Now().Add(-time.Hour)
	for _, d := range []string{"node_modules", ".git", "bin", "out", "local", "tmp", "testdata"} {
		touch(t, dir, filepath.Join(d, "noise.go"), built.Add(time.Hour))
	}
	if got, name := newestBuildInput(dir, built); !got.IsZero() {
		t.Errorf("claimed stale from %q, which is not a build input", name)
	}
}

// TestWarnIfStaleDisabledWithoutStamp: an unstamped binary must not warn about
// a tree that is not there, and must not panic looking for one.
func TestWarnIfStaleDisabledWithoutStamp(t *testing.T) {
	old := srcDir
	t.Cleanup(func() { srcDir = old })
	srcDir = ""
	warnIfStale()
}

// An unstamped binary is either a RELEASE (nothing to compare against) or a
// hand-built `go build -o bin/corrallm` in the tree someone is editing. Only
// the second can be stale in a way that matters, and the difference is whether
// the binary is sitting inside a module.
//
// This is not hypothetical: the trap that produced three wrong `config export`
// answers on 2026-09-06 was exactly a hand-built, unstamped CLI, which the
// stamped-only check stayed silent about.
func TestModuleRootAboveFindsTheTreeAHandBuiltBinarySitsIn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "corrallm")
	if err := os.WriteFile(exe, []byte("#!/bin/true\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := moduleRootAbove(exe)
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("moduleRootAbove = %q, want the module root %q", got, want)
	}
}

// A released binary installed somewhere ordinary has no module above it, and
// must stay silent — a warning naming a directory that does not exist on that
// machine is worse than none.
func TestModuleRootAboveIsEmptyOutsideAModule(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "corrallm")
	if err := os.WriteFile(exe, []byte("#!/bin/true\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := moduleRootAbove(exe); got != "" {
		t.Errorf("moduleRootAbove = %q, want empty outside a module", got)
	}
}
