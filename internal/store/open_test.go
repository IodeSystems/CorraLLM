package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// SQLite creates the FILE but not the directory holding it, so a first run
// against the default path (<home>/var/corrallm.db, where <home> does not exist
// yet) died at boot with "unable to open database file (14)" — a message naming
// neither the path nor the cause.
func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "nested", "corrallm.db")

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open into a missing directory: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created at %s: %v", path, err)
	}
}

// ":memory:" is not a filesystem path. Treating it as one would create a
// literal directory next to wherever the tests happened to run.
func TestOpenMemoryCreatesNoDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open in-memory: %v", err)
	}
	defer func() { _ = st.Close() }()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory Open created %d filesystem entries, want 0", len(entries))
	}
}
