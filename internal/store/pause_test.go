package store

import (
	"context"
	"database/sql"
	"testing"
)

// TestModelPauseRoundTrip: a pause survives a write/read cycle, an upsert
// replaces rather than duplicates (the model is the primary key), and a delete
// clears it. This is what makes a pause survive a restart.
func TestModelPauseRoundTrip(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if rows, err := st.LoadPauses(); err != nil || len(rows) != 0 {
		t.Fatalf("fresh db = (%v, %v), want empty", rows, err)
	}

	if err := st.SavePause("m", "maintenance", 1000, 2000); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadPauses()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0] != (ModelPause{Target: "m", Reason: "maintenance", AtMS: 1000, ResumeAtMS: 2000}) {
		t.Errorf("row = %+v", rows[0])
	}

	// Re-pausing the same model replaces its row: one pause per model.
	if err := st.SavePause("m", "", 3000, 0); err != nil {
		t.Fatal(err)
	}
	rows, err = st.LoadPauses()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AtMS != 3000 || rows[0].ResumeAtMS != 0 || rows[0].Reason != "" {
		t.Errorf("after upsert = %+v", rows)
	}

	if err := st.DeletePause("m"); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.LoadPauses(); err != nil || len(rows) != 0 {
		t.Fatalf("after delete = (%v, %v), want empty", rows, err)
	}
	// Deleting a pause that is not there is a no-op, not an error — unpause
	// must be idempotent.
	if err := st.DeletePause("m"); err != nil {
		t.Errorf("redundant delete: %v", err)
	}
}

// TestDropStaleModelPause: a model_pause left over from the model-keyed first
// cut is dropped so the process-keyed schema can recreate it. Without this,
// every pause query fails on the missing `target` column.
func TestDropStaleModelPause(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/stale.db"

	// Hand-build the OLD shape, then open through the normal path.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE model_pause (model TEXT NOT NULL PRIMARY KEY, reason TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL DEFAULT 0, resume_at INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO model_pause (model, reason) VALUES ('m','stale')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open over a stale model_pause: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The stale row is gone and the new shape works.
	rows, err := st.LoadPauses()
	if err != nil {
		t.Fatalf("LoadPauses after migration: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("stale rows survived: %+v", rows)
	}
	if err := st.SavePause("extension:oidio", "", 1, 0); err != nil {
		t.Errorf("SavePause on the migrated table: %v", err)
	}

	// Re-opening an already-migrated database must not drop it again.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	if rows, err := st2.LoadPauses(); err != nil || len(rows) != 1 {
		t.Errorf("reopen dropped a live pause: (%+v, %v)", rows, err)
	}
}
