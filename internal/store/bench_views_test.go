package store

import (
	"context"
	"testing"
)

// seedViews lays down two runs across two models, one skipped row, and an
// artifact record for only ONE of the runs — so every asymmetry the views have
// to survive is present.
func seedViews(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	rows := []BenchProbeResult{
		// r1: two models, model "a" also has an A/B second arm on p1.
		{RunID: "r1", Model: "a", At: 100, Probe: "p1", Capability: "chat", Toolset: "baseline", Stages: 2, StagesPassed: 2, Pass: true, WallMS: 10},
		{RunID: "r1", Model: "a", At: 100, Probe: "p1", Capability: "chat", Toolset: "toon", Stages: 2, StagesPassed: 1, WallMS: 12},
		{RunID: "r1", Model: "a", At: 100, Probe: "p2", Capability: "chat", Toolset: "baseline", Stages: 2, StagesPassed: 1, WallMS: 8},
		{RunID: "r1", Model: "b", At: 100, Probe: "p1", Capability: "chat", Toolset: "baseline", Stages: 2, StagesPassed: 0, WallMS: 9},
		// b cannot serve p2 at all.
		{RunID: "r1", Model: "b", At: 100, Probe: "p2", Capability: "chat", Toolset: "baseline", Skipped: true, SkipReason: "no chat capability"},
		// r2: newer, model "a" only.
		{RunID: "r2", Model: "a", At: 200, Probe: "p1", Capability: "chat", Toolset: "baseline", Stages: 2, StagesPassed: 2, Pass: true, WallMS: 7},
	}
	if err := s.SaveBenchProbeResults(ctx, rows); err != nil {
		t.Fatal(err)
	}
	// Only r1 has artifacts on disk.
	if err := s.SaveBenchRun(ctx, BenchRun{RunID: "r1", OutDir: "/out/r1", Host: "box1", At: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestBenchRuns_IndexesRunsWithoutArtifacts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedViews(t, s)

	runs, err := s.BenchRuns(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %+v", len(runs), runs)
	}
	// Newest first.
	if runs[0].RunID != "r2" || runs[1].RunID != "r1" {
		t.Fatalf("want r2 then r1, got %s then %s", runs[0].RunID, runs[1].RunID)
	}
	// r2 has NO bench_runs row. It must still be listed, dated from its results,
	// and reported as having no artifacts — the whole reason the index is built
	// from results rather than from bench_runs.
	if runs[0].At != 200 {
		t.Errorf("r2 should take its date from its results, got %d", runs[0].At)
	}
	if runs[0].OutDir != "" {
		t.Errorf("r2 should have no artifact dir, got %q", runs[0].OutDir)
	}
	if runs[1].OutDir != "/out/r1" || runs[1].Host != "box1" {
		t.Errorf("r1 should carry its artifact row, got %+v", runs[1])
	}

	r1 := runs[1]
	if r1.Models != 2 {
		t.Errorf("r1 models: want 2, got %d", r1.Models)
	}
	if r1.Probes != 2 {
		t.Errorf("r1 distinct probes: want 2, got %d", r1.Probes)
	}
	// 5 rows because p1/model-a ran two arms; probes counts 2. Conflating the
	// two would misreport an A/B as extra coverage.
	if r1.Rows != 5 {
		t.Errorf("r1 rows: want 5, got %d", r1.Rows)
	}
	if r1.Passed != 1 {
		t.Errorf("r1 passed: want 1, got %d", r1.Passed)
	}
	if r1.Skipped != 1 {
		t.Errorf("r1 skipped: want 1, got %d", r1.Skipped)
	}
	if r1.WallMSum != 39 {
		t.Errorf("r1 wall sum: want 39, got %d", r1.WallMSum)
	}
}

func TestBenchProbeResultsForRun_ReturnsEveryModel(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedViews(t, s)

	rows, err := s.BenchProbeResultsForRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("want all 5 r1 rows, got %d", len(rows))
	}
	models := map[string]int{}
	for _, r := range rows {
		if r.RunID != "r1" {
			t.Fatalf("row from the wrong run: %+v", r)
		}
		models[r.Model]++
	}
	if models["a"] != 3 || models["b"] != 2 {
		t.Fatalf("want a=3 b=2, got %v", models)
	}
	// The skipped row must survive the query: "b ran nothing" and "b was not a
	// candidate" are different answers and only the row distinguishes them.
	skipped := 0
	for _, r := range rows {
		if r.Skipped {
			skipped++
			if r.SkipReason == "" {
				t.Error("skipped row lost its reason")
			}
		}
	}
	if skipped != 1 {
		t.Errorf("want 1 skipped row preserved, got %d", skipped)
	}
}

func TestBenchProbeHistory_SpansModelsAndRuns(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedViews(t, s)

	rows, err := s.BenchProbeHistory(ctx, "p1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// p1: r1/a baseline, r1/a toon, r1/b baseline, r2/a baseline.
	if len(rows) != 4 {
		t.Fatalf("want 4 p1 rows across runs and models, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Probe != "p1" {
			t.Fatalf("wrong probe leaked in: %+v", r)
		}
	}
	// Newest first, so a regression reads top-down.
	if rows[0].RunID != "r2" {
		t.Errorf("want newest run first, got %s", rows[0].RunID)
	}
	seenModels := map[string]bool{}
	for _, r := range rows {
		seenModels[r.Model] = true
	}
	if !seenModels["a"] || !seenModels["b"] {
		t.Errorf("history must span models, got %v", seenModels)
	}
}

func TestBenchProbeHistory_LimitIsApplied(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedViews(t, s)

	rows, err := s.BenchProbeHistory(ctx, "p1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit ignored: got %d rows", len(rows))
	}
}
