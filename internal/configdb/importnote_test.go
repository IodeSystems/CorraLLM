package configdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A revision's note has to let an owner tell their own doing from somebody
// else's. Dashboard edits carry an address ("— from 127.0.0.1"); a CLI import
// carried only a file path, so the row read as nobody — and on a live box it
// was the RUNNING config that read that way (Lenny run 8: "it's the one entry I
// can't tie to a person").
//
// It must NOT invent an address. There is no request behind a CLI load, and a
// fabricated origin reads as a person who was never there. Naming the route is
// the honest half.
func TestImportedRevisionNamesTheRouteItCameIn_By(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "corrallm.yaml")
	if err := os.WriteFile(path, []byte("priorityGroups:\n  default:\n    weight: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := &Source{DB: openDB(t)}
	if _, err := src.ImportFile(ctx, path); err != nil {
		t.Fatalf("import: %v", err)
	}
	revs, err := Revisions(ctx, src.DB, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) == 0 {
		t.Fatal("import recorded no revision")
	}
	note := revs[0].Note
	if !strings.Contains(note, path) {
		t.Errorf("note lost the file it came from: %q", note)
	}
	if !strings.Contains(note, "command line") {
		t.Errorf("note does not say which way in was used: %q", note)
	}
	// The giveaway for a fabricated origin: the dashboard's phrasing.
	if strings.Contains(note, "— from 1") || strings.Contains(note, "127.0.0.1") {
		t.Errorf("note invented an address for a request that never existed: %q", note)
	}
}

// An explicit note still wins: a caller that said why is not overridden by the
// default, which exists only for the case where nobody said anything.
func TestExplicitNoteSurvivesImport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "corrallm.yaml")
	if err := os.WriteFile(path, []byte("priorityGroups:\n  default:\n    weight: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := (&Source{DB: openDB(t)}).WithNote("restoring last night's backup")
	if _, err := src.ImportFile(ctx, path); err != nil {
		t.Fatalf("import: %v", err)
	}
	revs, err := Revisions(ctx, src.DB, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) == 0 || revs[0].Note != "restoring last night's backup" {
		t.Errorf("explicit note lost: %v", revs)
	}
}
