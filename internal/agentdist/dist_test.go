package agentdist

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
)

// The installer must write EXACTLY agent.LauncherScript.
//
// If the rendered heredoc differs by even a byte, every freshly-installed agent
// looks locally-edited to ReconcileLauncher and can never be upgraded again.
func TestInstallScriptWritesTheCanonicalLauncher(t *testing.T) {
	h := &Handler{Dir: t.TempDir(), Version: "test"}
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
