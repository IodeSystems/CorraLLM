package store

import (
	"context"
	"testing"
)

func seedActivity(t *testing.T, st *Store, rows []Activity) {
	t.Helper()
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
}

// The sum is what a bench subtracts from its wall clock, so the scoping has to
// be exact: another caller's queueing, another model's, or another stage's
// window would all inflate the execution time it computes.
func TestOverheadForScopesByKeyModelAndWindow(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	seedActivity(t, st, []Activity{
		// In scope: right key, right model, inside the window.
		{TS: 100, Served: "m1", Key: "bench-1", QueuedMS: 10, LoadMS: 100},
		{TS: 200, Served: "m1", Key: "bench-1", QueuedMS: 5, LoadMS: 0},
		// Boundaries are inclusive — a request logged exactly on the edge
		// belongs to the stage that was running.
		{TS: 50, Served: "m1", Key: "bench-1", QueuedMS: 1, LoadMS: 0},
		{TS: 300, Served: "m1", Key: "bench-1", QueuedMS: 2, LoadMS: 0},
		// Out of scope, one reason each.
		{TS: 150, Served: "m1", Key: "someone-else", QueuedMS: 999, LoadMS: 999},
		{TS: 150, Served: "m2", Key: "bench-1", QueuedMS: 999, LoadMS: 999},
		{TS: 49, Served: "m1", Key: "bench-1", QueuedMS: 999, LoadMS: 999},
		{TS: 301, Served: "m1", Key: "bench-1", QueuedMS: 999, LoadMS: 999},
	})

	o, err := st.OverheadFor(context.Background(), "bench-1", "m1", 50, 300)
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 4 {
		t.Errorf("Requests = %d, want 4", o.Requests)
	}
	if o.QueuedMS != 18 {
		t.Errorf("QueuedMS = %d, want 18 (10+5+1+2)", o.QueuedMS)
	}
	if o.LoadMS != 100 {
		t.Errorf("LoadMS = %d, want 100", o.LoadMS)
	}
}

// An empty window is not an error, and must not look like "no waiting": a
// caller that cannot tell them apart would silently treat a failed correlation
// as a clean measurement.
func TestOverheadForEmptyWindowReportsZeroRequests(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	o, err := st.OverheadFor(context.Background(), "bench-1", "m1", 0, 10)
	if err != nil {
		t.Fatalf("an empty window must not error: %v", err)
	}
	if o.Requests != 0 || o.QueuedMS != 0 || o.LoadMS != 0 {
		t.Errorf("got %+v, want all zero", o)
	}
}

// load_ms arrived as a migration on a table with history, so a row written
// before it existed must read back as zero rather than failing the scan.
func TestLoadMSRoundTrips(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	seedActivity(t, st, []Activity{{TS: 1, Served: "m1", Key: "k", QueuedMS: 7, LoadMS: 6705}})
	got, err := st.RecentActivity(10, "m1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].LoadMS != 6705 {
		t.Errorf("LoadMS = %d, want 6705", got[0].LoadMS)
	}
}

// The stage timings are the point of the whole shared-bench change, and they
// travel runner → API → store → dashboard. A column that silently drops them
// would leave the UI showing wall time — the number that moves with the
// neighbours — while the honest one sat in a file on disk.
func TestStageTimingsRoundTrip(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	in := BenchProbeStage{
		RunID: "r1", Model: "m1", Probe: "p1", Toolset: "baseline", ToolFormat: "json",
		Stage: 0, WallMS: 29687, QueuedMS: 18664, ExecMS: 11023,
	}
	if err := st.SaveBenchProbeStages(ctx, []BenchProbeStage{in}); err != nil {
		t.Fatal(err)
	}
	got, err := st.BenchProbeStagesFor(ctx, "r1", "m1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stages, want 1", len(got))
	}
	if got[0].WallMS != 29687 || got[0].QueuedMS != 18664 || got[0].ExecMS != 11023 {
		t.Errorf("timings did not survive: wall=%d queued=%d exec=%d",
			got[0].WallMS, got[0].QueuedMS, got[0].ExecMS)
	}
}

// Re-publishing a stage must overwrite its timings, not keep the first ones —
// a retried publish otherwise pins the numbers from a run that was superseded.
func TestStageTimingsUpsert(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	s := BenchProbeStage{RunID: "r1", Model: "m1", Probe: "p1", Stage: 0,
		WallMS: 100, QueuedMS: 90, ExecMS: 10}
	if err := st.SaveBenchProbeStages(ctx, []BenchProbeStage{s}); err != nil {
		t.Fatal(err)
	}
	s.WallMS, s.QueuedMS, s.ExecMS = 200, 20, 180
	if err := st.SaveBenchProbeStages(ctx, []BenchProbeStage{s}); err != nil {
		t.Fatal(err)
	}
	got, err := st.BenchProbeStagesFor(ctx, "r1", "m1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExecMS != 180 || got[0].QueuedMS != 20 {
		t.Errorf("upsert did not replace timings: %+v", got)
	}
}
