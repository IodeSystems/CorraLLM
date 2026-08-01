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

// TestWarnIfStaleDisabledWithoutStamp: a plain `go install` or a released build
// leaves srcDir empty, and must not warn about a tree that is not there.
func TestWarnIfStaleDisabledWithoutStamp(t *testing.T) {
	old := srcDir
	t.Cleanup(func() { srcDir = old })
	srcDir = ""
	warnIfStale() // must not panic, must not consult the filesystem
}
