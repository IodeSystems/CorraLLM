package store

import (
	"context"
	"testing"
)

func TestBenchProbeResults_UpsertAndLatestRun(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	must := func(rows ...BenchProbeResult) {
		t.Helper()
		if err := s.SaveBenchProbeResults(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	must(
		BenchProbeResult{RunID: "r1", Model: "a", At: 100, Probe: "p1", Capability: "chat", Stages: 2, StagesPassed: 1},
		BenchProbeResult{RunID: "r1", Model: "a", At: 100, Probe: "p2", Capability: "chat", Stages: 2, StagesPassed: 2, Pass: true},
	)
	must(BenchProbeResult{RunID: "r2", Model: "a", At: 200, Probe: "p1", Capability: "chat", Stages: 2, StagesPassed: 2, Pass: true})

	// Empty runID must return ONLY the newest run. Mixing runs would average a
	// regression away against the history that preceded it.
	got, err := s.BenchProbeResultsFor(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != "r2" || !got[0].Pass {
		t.Fatalf("want only r2's row, got %+v", got)
	}

	got, err = s.BenchProbeResultsFor(ctx, "a", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want both r1 rows, got %d: %+v", len(got), got)
	}

	// A republished run replaces rather than duplicates: llm-bench retries the
	// publish, and a doubled row set would double every capability's stage count.
	must(BenchProbeResult{RunID: "r1", Model: "a", At: 100, Probe: "p1", Capability: "chat", Stages: 2, StagesPassed: 2, Pass: true})
	got, err = s.BenchProbeResultsFor(ctx, "a", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("republish must upsert, got %d rows: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Probe == "p1" && !r.Pass {
			t.Errorf("p1 should have been updated to pass: %+v", r)
		}
	}
}

// Cold and warm are separate rows for the same probe: a disagreement between
// them is the finding, so collapsing them would erase it.
func TestBenchProbeResults_ColdAndWarmAreDistinctRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SaveBenchProbeResults(ctx, []BenchProbeResult{
		{RunID: "r1", Model: "a", At: 1, Probe: "vision", RunMode: "cold", Stages: 1, StagesPassed: 0},
		{RunID: "r1", Model: "a", At: 1, Probe: "vision", RunMode: "warm", Stages: 1, StagesPassed: 1, Pass: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BenchProbeResultsFor(ctx, "a", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("cold and warm must both persist, got %d: %+v", len(got), got)
	}
}

func TestBenchProbeResults_SkippedRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SaveBenchProbeResults(ctx, []BenchProbeResult{{
		RunID: "r1", Model: "stt", At: 1, Probe: "codex-plan-0", Capability: "chat",
		Skipped: true, SkipReason: `probe needs capability "chat", model serves "audio.stt"`,
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BenchProbeResultsFor(ctx, "stt", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Skipped || got[0].SkipReason == "" {
		t.Fatalf("skip reason must survive the round trip: %+v", got)
	}
}
