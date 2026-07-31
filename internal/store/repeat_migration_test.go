package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// oldProbeResultsDDL is the table as it shipped before the repeat column: arm
// aware (so dropStaleProbeTables leaves it alone) but with the narrower UNIQUE.
const oldProbeResultsDDL = `
CREATE TABLE bench_probe_results (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  at            INTEGER NOT NULL,
  probe         TEXT    NOT NULL,
  class         TEXT    NOT NULL DEFAULT '',
  capability    TEXT    NOT NULL DEFAULT '',
  run_mode      TEXT    NOT NULL DEFAULT '',
  toolset       TEXT    NOT NULL DEFAULT '',
  tool_format   TEXT    NOT NULL DEFAULT '',
  stages        INTEGER NOT NULL DEFAULT 0,
  stages_passed INTEGER NOT NULL DEFAULT 0,
  checks_passed INTEGER NOT NULL DEFAULT 0,
  checks_total  INTEGER NOT NULL DEFAULT 0,
  pass          INTEGER NOT NULL DEFAULT 0,
  wall_ms       INTEGER NOT NULL DEFAULT 0,
  new_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  skip_reason   TEXT    NOT NULL DEFAULT '',
  note          TEXT    NOT NULL DEFAULT '',
  UNIQUE(run_id, model, probe, run_mode, toolset, tool_format)
);
CREATE INDEX bench_probe_results_model_at ON bench_probe_results(model, at DESC);
CREATE INDEX bench_probe_results_run ON bench_probe_results(run_id, model);
`

// The widening must PRESERVE history. dropStaleProbeTables' own comment says a
// rebuild-by-dropping stops being acceptable once the table carries real runs —
// it does now, so this asserts the rows survive rather than the schema merely
// ending up the right shape.
func TestRepeatMigrationPreservesExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrallm.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(oldProbeResultsDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO bench_probe_results
  (run_id, model, at, probe, class, run_mode, toolset, tool_format,
   stages, stages_passed, checks_passed, checks_total, pass, note)
VALUES ('r1','m1',100,'p1','coding','','baseline','json',3,3,5,5,1,'kept')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open (which runs the migration): %v", err)
	}
	defer func() { _ = st.Close() }()

	got, err := st.BenchProbeResultsForRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want the 1 pre-existing row preserved", len(got))
	}
	r := got[0]
	if r.Note != "kept" || r.Stages != 3 || r.StagesPassed != 3 || r.ChecksTotal != 5 {
		t.Errorf("row was not carried across intact: %+v", r)
	}
	if r.Repeat != 0 {
		t.Errorf("Repeat = %d, want 0 for a row written before repeats existed", r.Repeat)
	}
}

// The point of the whole change: two repeats of one arm must coexist as
// separate rows. Under the old UNIQUE the second silently replaced the first.
func TestRepeatsAreStoredSeparately(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "corrallm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	base := BenchProbeResult{
		RunID: "r1", Model: "m1", At: 100, Probe: "p1",
		Toolset: "baseline", ToolFormat: "json", Stages: 3,
	}
	first, second := base, base
	first.Repeat, first.StagesPassed, first.Pass = 0, 3, true
	second.Repeat, second.StagesPassed, second.Pass = 1, 1, false

	if err := st.SaveBenchProbeResults(context.Background(),
		[]BenchProbeResult{first, second}); err != nil {
		t.Fatal(err)
	}
	got, err := st.BenchProbeResultsForRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 — repeats must not collapse onto each other", len(got))
	}
	// Neither may have inherited the other's counts: that is the summing bug.
	for _, r := range got {
		if r.Stages != 3 {
			t.Errorf("repeat %d has stages=%d, want the probe's own 3", r.Repeat, r.Stages)
		}
	}
}

// Re-publishing the same repeat still replaces it, so a retried publish does not
// duplicate a run.
func TestRepublishSameRepeatReplaces(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "corrallm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	r := BenchProbeResult{RunID: "r1", Model: "m1", At: 100, Probe: "p1",
		Toolset: "baseline", ToolFormat: "json", Repeat: 0, Stages: 3, StagesPassed: 1}
	ctx := context.Background()
	if err := st.SaveBenchProbeResults(ctx, []BenchProbeResult{r}); err != nil {
		t.Fatal(err)
	}
	r.StagesPassed = 3
	if err := st.SaveBenchProbeResults(ctx, []BenchProbeResult{r}); err != nil {
		t.Fatal(err)
	}
	got, err := st.BenchProbeResultsForRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 — the same repeat must upsert", len(got))
	}
	if got[0].StagesPassed != 3 {
		t.Errorf("StagesPassed = %d, want the republished 3", got[0].StagesPassed)
	}
}
