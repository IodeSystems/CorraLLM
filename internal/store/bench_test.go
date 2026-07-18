package store

import (
	"context"
	"testing"
)

func TestBenchResults_UpsertAndLatest(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	must := func(r BenchResult) {
		t.Helper()
		if err := s.SaveBenchResult(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	must(BenchResult{RunID: "r1", Model: "a", At: 100, Stages: 4, StagesPassed: 2})
	must(BenchResult{RunID: "r1", Model: "b", At: 100, Stages: 4, StagesPassed: 4})
	must(BenchResult{RunID: "r2", Model: "a", At: 200, Stages: 4, StagesPassed: 3})

	got, err := s.LatestBenchResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want one row per model, got %d: %+v", len(got), got)
	}
	byModel := map[string]BenchResult{}
	for _, r := range got {
		byModel[r.Model] = r
	}
	// The comparison view must show the NEWEST run per model, not every run —
	// otherwise an old result competes with the current one side by side.
	if byModel["a"].RunID != "r2" || byModel["a"].StagesPassed != 3 {
		t.Errorf("latest for a should be r2: %+v", byModel["a"])
	}

	// Re-publishing the same (run, model) replaces it: a retried publish must
	// not double-count a run.
	must(BenchResult{RunID: "r2", Model: "a", At: 200, Stages: 4, StagesPassed: 4})
	hist, err := s.BenchResultsFor(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Errorf("re-publish should upsert, not append: %d rows", len(hist))
	}
	if hist[0].StagesPassed != 4 {
		t.Errorf("upsert did not take: %+v", hist[0])
	}
}

// History is newest-first so a console can show "last N runs" without sorting.
func TestBenchResults_HistoryOrder(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()
	for i, at := range []int64{10, 30, 20} {
		if err := s.SaveBenchResult(ctx, BenchResult{
			RunID: string(rune('a' + i)), Model: "m", At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.BenchResultsFor(ctx, "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].At != 30 || got[2].At != 10 {
		t.Errorf("want newest-first, got %+v", got)
	}
}
