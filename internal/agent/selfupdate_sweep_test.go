package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSweepStaleDownloads(t *testing.T) {
	dir := t.TempDir()
	stale := []string{updateTempPrefix + "123", updateTempPrefix + "456789"}
	// Everything the sweep must NOT touch: the agent, its credential, its
	// launcher, and anything of the operator's that merely looks similar.
	keep := []string{"corrallm", "agent.yml", LauncherName, "corrallm-update-notours", ".corrallm-other"}
	for _, n := range append(append([]string{}, stale...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, updateTempPrefix+"adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	sweepStaleDownloads(dir)

	for _, n := range stale {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep (err=%v)", n, err)
		}
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed but should not have been: %v", n, err)
		}
	}
	// A directory sharing the prefix is not a download; removing it would need
	// RemoveAll, and that is not a power this should have.
	if _, err := os.Stat(filepath.Join(dir, updateTempPrefix+"adir")); err != nil {
		t.Errorf("prefixed directory was removed: %v", err)
	}
}

func TestSweepStaleDownloadsMissingDirIsNotAPanic(t *testing.T) {
	sweepStaleDownloads(filepath.Join(t.TempDir(), "nope"))
}
