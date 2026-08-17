package toolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func TestProbeReportsAbsentWithoutError(t *testing.T) {
	requireBash(t)
	dir := t.TempDir() // empty: nothing installed here
	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}

	p, err := RunProbe(context.Background(), l, Spec{
		Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: dir,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p.Present {
		t.Error("reported present with nothing installed")
	}
	// "absent" is an ANSWER, not a failure — the caller must be able to tell it
	// from "I could not ask", which is the error return.
	if p.Error != "" {
		t.Errorf("absent should not carry an error, got %q", p.Error)
	}
	if !strings.HasSuffix(p.Path, "llama-server") {
		t.Errorf("path %q does not name the binary it looked for", p.Path)
	}
}

// A tool whose version can be read from the binary reports source "binary".
// Faked with a script that prints llama.cpp's real banner shape — to STDERR,
// which is where llama-server actually writes it and the reason a naive
// $(... --version) capture reports "unknown" on a working binary.
func TestProbeReadsVersionFromStderr(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "llama-server")
	script := "#!/bin/sh\necho 'version: 10380 (0b1bad14f)' >&2\necho 'built with Clang 18.1.3 for Linux x86_64' >&2\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}
	p, err := RunProbe(context.Background(), l, Spec{
		Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: bin,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !p.Present {
		t.Fatal("did not see the installed binary")
	}
	if p.Version != "10380 (0b1bad14f)" {
		t.Errorf("version = %q, want %q — llama-server prints it on STDERR", p.Version, "10380 (0b1bad14f)")
	}
	if p.Commit != "0b1bad14f" {
		t.Errorf("commit = %q, want 0b1bad14f", p.Commit)
	}
	if p.Source != "binary" {
		t.Errorf("source = %q, want binary", p.Source)
	}
	if !p.Identified() {
		t.Error("Identified() false for a binary that reported its own version")
	}
}

// ninfer has no --version, so an install corrallm did not build cannot be
// identified. The registry must say so rather than invent a version.
func TestPresentButUnidentifiable(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ninfer-serve"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}
	p, err := RunProbe(context.Background(), l, Spec{
		Name: "ninfer", Recipe: "ninfer", Bin: "ninfer-serve", InstalledAt: bin,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !p.Present {
		t.Fatal("did not see the installed binary")
	}
	if p.Version != "" || p.Source != "" {
		t.Errorf("invented a version for a tool that cannot report one: version=%q source=%q", p.Version, p.Source)
	}
	if p.Identified() {
		t.Error("Identified() true for a binary with no version source")
	}
}

// A stamp is the only version source for a tool with no --version. It is also
// what makes a corrallm-built ninfer identifiable when a hand-built one is not.
func TestProbeFallsBackToTheStamp(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ninfer-serve"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := "head=abc123def patches=none archs=120a"
	if err := os.WriteFile(filepath.Join(bin, ".corrallm-stamp"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}
	p, err := RunProbe(context.Background(), l, Spec{
		Name: "ninfer", Recipe: "ninfer", Bin: "ninfer-serve", InstalledAt: bin,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if p.Source != "stamp" {
		t.Errorf("source = %q, want stamp", p.Source)
	}
	if p.Commit != "abc123def" {
		t.Errorf("commit = %q, want abc123def (from the stamp)", p.Commit)
	}
}

// Missing environment must produce a clean error, not malformed JSON. A recipe
// that aborts mid-printf under `set -u` emits `{"ref":,...}`, which surfaces as
// an unparseable-JSON complaint that says nothing about the real mistake.
func TestMissingSpecFieldsFailCleanly(t *testing.T) {
	requireBash(t)
	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}
	_, err := RunProbe(context.Background(), l, Spec{Name: "ninfer", Recipe: "ninfer"})
	if err == nil {
		t.Fatal("expected an error with neither prefix nor installedAt set")
	}
	if strings.Contains(err.Error(), "unparseable") {
		t.Errorf("got a JSON parse failure instead of a stated reason: %v", err)
	}
	if !strings.Contains(err.Error(), "TOOL_PREFIX") {
		t.Errorf("error does not name what was missing: %v", err)
	}
}

func TestUnknownRecipeIsRefused(t *testing.T) {
	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "box1"}
	_, err := l.Run(context.Background(), Spec{Name: "nope", Recipe: "nope"}, VerbProbe)
	if err == nil {
		t.Fatal("expected an error for an unknown recipe")
	}
	if !strings.Contains(err.Error(), "box1") {
		t.Errorf("error should name the host it was asked of: %v", err)
	}
}

// install-deps is refused unless the host enabled it, and the refusal is a
// well-formed answer rather than a failure — the operator gets "not allowed
// here", not "something went wrong".
func TestInstallDepsRefusedByDefault(t *testing.T) {
	l := &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "box1"}
	res, err := RunInstallDeps(context.Background(), l, Spec{Name: "ninfer", Recipe: "ninfer", Prefix: t.TempDir()})
	if err == nil {
		t.Fatal("expected the refusal to surface as an error")
	}
	if res == nil {
		t.Fatal("refusal must still return a result the caller can render")
	}
	if res.Allowed {
		t.Error("Allowed true without --allow-install-deps")
	}
	if !strings.Contains(res.Error, "allow-install-deps") {
		t.Errorf("refusal does not say how to enable it: %q", res.Error)
	}
}

func TestTimeoutsAreOrderedByCost(t *testing.T) {
	if Timeout(VerbProbe) >= Timeout(VerbInstallDeps) {
		t.Error("a probe must not be given as long as a package install")
	}
	if Timeout(VerbInstallDeps) >= Timeout(VerbBuild) {
		t.Error("a build needs longer than an install")
	}
	if Timeout(VerbProbe) < 5*time.Second {
		t.Error("probe timeout is too tight for a cold fork+exec on a loaded box")
	}
}

func TestLastJSONLineIgnoresStrayOutput(t *testing.T) {
	got := lastJSONLine("configuring\nsome noise\n{\"present\":true}\n")
	if got != `{"present":true}` {
		t.Errorf("lastJSONLine = %q", got)
	}
	if lastJSONLine("no json here\n") != "" {
		t.Error("lastJSONLine invented a result")
	}
}
