package configdb

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func cfgWithKeys(keys ...string) *config.Config {
	m := map[string]string{}
	for _, k := range keys {
		m[k] = "default"
	}
	return &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           m,
	}
}

// Every save records a revision, and the note is what makes the history
// readable a month later.
func TestSaveRecordsARevision(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	if err := src.WithNote("first").Save(ctx, cfgWithKeys("a")); err != nil {
		t.Fatal(err)
	}
	if err := src.WithNote("second").Save(ctx, cfgWithKeys("a", "b")); err != nil {
		t.Fatal(err)
	}

	revs, err := Revisions(ctx, src.DB, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("got %d revisions, want 2", len(revs))
	}
	// Newest first: that is the order anybody reads a history in.
	if revs[0].Note != "second" || revs[1].Note != "first" {
		t.Errorf("wrong order or notes: %q then %q", revs[0].Note, revs[1].Note)
	}
	if revs[0].Size == 0 || revs[0].At.IsZero() {
		t.Error("a revision carries no size or timestamp")
	}
}

// The point of history: get back a config you no longer have.
func TestRestoreBringsBackAnEarlierConfig(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}

	if err := src.WithNote("with b").Save(ctx, cfgWithKeys("a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := src.WithNote("dropped b").Save(ctx, cfgWithKeys("a")); err != nil {
		t.Fatal(err)
	}
	cur, err := src.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cur.Keys["b"]; ok {
		t.Fatal("setup wrong: b should be gone")
	}

	revs, _ := Revisions(ctx, src.DB, 10)
	oldest := revs[len(revs)-1]
	if err := src.Restore(ctx, oldest.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	back, err := src.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Keys["b"]; !ok {
		t.Error("the restored config does not have the key it was restored for")
	}
}

// A restore is a CHANGE, not a rewind. History that can be rewritten is not
// history, and "we rolled back at 14:02" is the fact somebody needs later.
func TestRestoreAppendsRatherThanRewinds(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	if err := src.WithNote("one").Save(ctx, cfgWithKeys("a")); err != nil {
		t.Fatal(err)
	}
	if err := src.WithNote("two").Save(ctx, cfgWithKeys("a", "b")); err != nil {
		t.Fatal(err)
	}
	revs, _ := Revisions(ctx, src.DB, 10)
	before := len(revs)

	if err := src.Restore(ctx, revs[len(revs)-1].ID); err != nil {
		t.Fatal(err)
	}
	after, _ := Revisions(ctx, src.DB, 10)
	if len(after) != before+1 {
		t.Errorf("restore produced %d revisions, want %d — history was rewound", len(after), before+1)
	}
	if !strings.Contains(after[0].Note, "restored") {
		t.Errorf("the restore is not labelled as one: %q", after[0].Note)
	}
	// Exactly one, not two: Save records, so Restore must not record again.
	n := 0
	for _, r := range after {
		if strings.Contains(r.Note, "restored") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a single restore recorded %d revisions", n)
	}
}

// An invalid revision must be refused rather than restored into a daemon that
// cannot run it.
func TestRestoreRefusesAnInvalidRevision(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	if err := src.WithNote("ok").Save(ctx, cfgWithKeys("a")); err != nil {
		t.Fatal(err)
	}
	// A key pointing at a group that does not exist: valid YAML, invalid config.
	// Written as a real block mapping — `keys:{k: nope}` has no space after the
	// colon, so YAML reads the whole line as a scalar and the config comes out
	// empty and perfectly valid, which is a test that proves nothing.
	if _, err := src.DB.ExecContext(ctx,
		"INSERT INTO config_revision (ts, note, yaml) VALUES (1, 'bad', 'keys:\n  k: nope\n')"); err != nil {
		t.Fatal(err)
	}
	revs, _ := Revisions(ctx, src.DB, 10)
	if err := src.Restore(ctx, revs[0].ID); err == nil {
		t.Error("restored a revision that does not validate")
	}
}

func TestPruneRevisionsKeepsNewest(t *testing.T) {
	ctx := context.Background()
	src := &Source{DB: openDB(t)}
	for i := range 6 {
		if err := src.WithNote(string(rune('a'+i))).Save(ctx, cfgWithKeys("a")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PruneRevisions(ctx, src.DB, 3); err != nil {
		t.Fatal(err)
	}
	revs, _ := Revisions(ctx, src.DB, 50)
	if len(revs) != 3 {
		t.Fatalf("kept %d, want 3", len(revs))
	}
	if revs[0].Note != "f" {
		t.Errorf("newest kept is %q, want the last write", revs[0].Note)
	}
}
