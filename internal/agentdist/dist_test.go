package agentdist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
)

// dirWithBinaries is a serving directory that `make agents` has populated.
// InstallScript renders the real installer only when there is something to
// install, so tests about its CONTENT have to supply one.
func dirWithBinaries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "corrallm-linux-amd64"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The installer must write EXACTLY agent.LauncherScript.
//
// If the rendered heredoc differs by even a byte, every freshly-installed agent
// looks locally-edited to ReconcileLauncher and can never be upgraded again.
func TestInstallScriptWritesTheCanonicalLauncher(t *testing.T) {
	h := &Handler{Dir: dirWithBinaries(t), Version: "test"}
	s := h.InstallScript("http://primary:8111")

	_, rest, ok := strings.Cut(s, "<<'SH'\n")
	if !ok {
		t.Fatal("no quoted launcher heredoc in the install script")
	}
	// The body keeps its own trailing newline; "SH\n" is the terminator line.
	got, _, ok := strings.Cut(rest, "SH\n")
	if !ok {
		t.Fatal("unterminated launcher heredoc")
	}

	if got != agent.LauncherScript {
		t.Fatalf("installer launcher differs from agent.LauncherScript\n got: %q\nwant: %q", got, agent.LauncherScript)
	}
}

// A primary with no built agents must say so INSIDE the script.
//
// The documented invocation is `curl -fsSL … | sh`, and -f makes curl print
// nothing at all on a 4xx — so returning an HTTP error here would show the
// operator an empty line and no reason. Serving a script that explains itself
// and exits non-zero fails just as loudly and says what to do.
func TestInstallScriptWithoutBinariesExplainsItself(t *testing.T) {
	dir := t.TempDir() // `make agents` never ran
	h := &Handler{Dir: dir, Version: "test"}

	s := h.InstallScript("http://primary:8111")
	if strings.Contains(s, "<<'SH'") {
		t.Fatal("rendered the real installer with no binaries to install")
	}
	for _, want := range []string{"make agents", dir, "exit 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("script does not mention %q:\n%s", want, s)
		}
	}
}

// Only corrallm-* artifacts count. A directory holding just the VERSION stamp
// (or leftover scratch) has nothing installable in it.
func TestHasBinariesIgnoresNonArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if (&Handler{Dir: dir}).hasBinaries() {
		t.Error("hasBinaries = true for a directory with no corrallm-* artifact")
	}
	if err := os.WriteFile(filepath.Join(dir, "corrallm-darwin-arm64"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !(&Handler{Dir: dir}).hasBinaries() {
		t.Error("hasBinaries = false with an artifact present")
	}
}

// A missing directory is the normal state of a downloaded primary, not an error.
func TestHasBinariesMissingDir(t *testing.T) {
	if (&Handler{Dir: filepath.Join(t.TempDir(), "absent")}).hasBinaries() {
		t.Error("hasBinaries = true for a directory that does not exist")
	}
}
