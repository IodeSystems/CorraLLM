package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileLauncherNoFileIsNotCreated(t *testing.T) {
	dir := t.TempDir()
	if err := ReconcileLauncher(dir); err != nil {
		t.Fatalf("ReconcileLauncher: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LauncherName)); !os.IsNotExist(err) {
		t.Fatalf("start.sh was created in a directory that had none (err=%v)", err)
	}
}

// Every shipped launcher must still be recognised, or installs carrying it are
// stranded on it forever.
func TestEveryPastLauncherUpgradesToCurrent(t *testing.T) {
	if len(pastLaunchers) == 0 {
		t.Skip("no shipped launchers yet")
	}
	for i, old := range pastLaunchers {
		if old == LauncherScript {
			t.Fatalf("pastLaunchers[%d] equals LauncherScript; drop the duplicate", i)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, LauncherName)
		if err := os.WriteFile(path, []byte(old), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ReconcileLauncher(dir); err != nil {
			t.Fatalf("pastLaunchers[%d]: %v", i, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != LauncherScript {
			t.Fatalf("pastLaunchers[%d] was not upgraded", i)
		}
	}
}

func TestReconcileLauncherUpgradesAKnownOldLauncher(t *testing.T) {
	const old = "#!/bin/sh\n# an earlier build's launcher\nexec ./corrallm agent\n"
	pastLaunchers = append(pastLaunchers, old)
	t.Cleanup(func() { pastLaunchers = pastLaunchers[:len(pastLaunchers)-1] })

	dir := t.TempDir()
	path := filepath.Join(dir, LauncherName)
	if err := os.WriteFile(path, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileLauncher(dir); err != nil {
		t.Fatalf("ReconcileLauncher: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != LauncherScript {
		t.Fatalf("launcher not upgraded:\n%s", got)
	}
	// Still runnable, or the machine can no longer be started by hand.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("launcher is not executable: %v", st.Mode())
	}
}

func TestReconcileLauncherLeavesLocalEditsAlone(t *testing.T) {
	edited := LauncherScript + "# operator added this\n"
	dir := t.TempDir()
	path := filepath.Join(dir, LauncherName)
	if err := os.WriteFile(path, []byte(edited), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileLauncher(dir); err != nil {
		t.Fatalf("ReconcileLauncher: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Fatalf("clobbered a locally-edited launcher:\n%s", got)
	}
}

// A current install must be left untouched — otherwise every agent rewrites
// start.sh on every start and the "did anything change" signal is worthless.
func TestReconcileLauncherIsANoOpWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LauncherName)
	if err := os.WriteFile(path, []byte(LauncherScript), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileLauncher(dir); err != nil {
		t.Fatalf("ReconcileLauncher: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rewrote an already-current launcher")
	}
}
