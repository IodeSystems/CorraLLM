package store

import (
	"context"
	"testing"
)

// TestActivityRoundTrip persists a metered record and reads it back, covering
// the P6 token/cost columns.
func TestActivityRoundTrip(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	in := Activity{
		TS: 1, Served: "m", Backend: "m#0", Key: "k", Path: "/v1/chat/completions",
		Status: 200, DwellMS: 42, PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.00105,
	}
	if err := st.InsertActivity(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecentActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	in.ID = 1 // RecentActivity now returns the autoincrement id (P10b)
	if got[0] != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], in)
	}

	// ActivityByID returns the full row including captured payloads.
	full := Activity{TS: 2, Served: "m", Backend: "m#0", Status: 200,
		ReqBody: `{"model":"m"}`, RespBody: "hi", TTFBMs: 12}
	if err := st.InsertActivity(full); err != nil {
		t.Fatal(err)
	}
	got2, err := st.ActivityByID(2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != 2 || got2.ReqBody != `{"model":"m"}` || got2.RespBody != "hi" || got2.TTFBMs != 12 {
		t.Errorf("ActivityByID = %+v", got2)
	}
}

// TestRollupByModel aggregates per served model, ordered by cost desc, and
// honors the since cutoff.
func TestRollupByModel(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "cheap", Status: 200, PromptTokens: 10, CompletionTokens: 5, DwellMS: 50, CostUSD: 0.001},
		{TS: 200, Served: "cheap", Status: 200, PromptTokens: 10, CompletionTokens: 5, DwellMS: 50, CostUSD: 0.001},
		{TS: 300, Served: "pricey", Status: 200, PromptTokens: 100, CompletionTokens: 50, DwellMS: 500, CostUSD: 0.5},
		{TS: 50, Served: "old", Status: 200, CostUSD: 9.9}, // before cutoff
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// Cutoff at ts=100 excludes "old"; pricey outranks cheap by cost.
	got, err := st.RollupByModel(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(got), got)
	}
	if got[0].Served != "pricey" || got[0].CostUSD != 0.5 {
		t.Errorf("first group = %+v, want pricey/0.5", got[0])
	}
	if got[1].Served != "cheap" || got[1].Requests != 2 || got[1].PromptTokens != 20 || got[1].DwellMS != 100 {
		t.Errorf("cheap aggregation = %+v", got[1])
	}

	// sinceMS=0 includes everything.
	all, err := st.RollupByModel(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 groups all-time, got %d", len(all))
	}
}

// TestRollupByKey aggregates per caller key, ordered by cost desc.
func TestRollupByKey(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.5, DwellMS: 100, PromptTokens: 5},
		{TS: 200, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.01, DwellMS: 5},
		{TS: 300, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.01, DwellMS: 5},
		{TS: 400, Served: "m", Key: "", Status: 200, CostUSD: 0.02}, // unkeyed
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RollupByKey(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d: %+v", len(got), got)
	}
	if got[0].Key != "aw3" || got[0].CostUSD != 0.5 {
		t.Errorf("top key = %+v, want aw3/0.5", got[0])
	}
	// ragtag aggregates its two rows.
	for _, r := range got {
		if r.Key == "ragtag" && (r.Requests != 2 || r.DwellMS != 10) {
			t.Errorf("ragtag rollup = %+v", r)
		}
	}
}

// TestRollupSeriesQueueMetrics aggregates queue wait + 429 rejections per bucket.
func TestRollupSeriesQueueMetrics(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 1000, Served: "m", Key: "k", Status: 200, QueuedMS: 0},
		{TS: 1500, Served: "m", Key: "k", Status: 200, QueuedMS: 500},  // queued then served
		{TS: 1800, Served: "m", Key: "k", Status: 429, QueuedMS: 1000}, // queued then rejected
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// One wide bucket covering all three.
	got, err := st.RollupSeries(0, 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (bucket,key) row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Requests != 3 || r.Rejected != 1 || r.QueuedMS != 1500 {
		t.Errorf("got requests=%d rejected=%d queuedMs=%d, want 3/1/1500", r.Requests, r.Rejected, r.QueuedMS)
	}
}

// TestLaneSamples: samples aggregate to mean/peak per (bucket, group), and
// pruning drops old rows.
func TestLaneSamples(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Two samples in the same bucket for "interactive": waiting 2 then 6.
	if err := st.InsertLaneSamples(1000, []LaneSample{{Group: "interactive", Active: 1, Waiting: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertLaneSamples(2000, []LaneSample{{Group: "interactive", Active: 1, Waiting: 6}}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LaneDepthSeries(0, 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (bucket,group), got %d", len(got))
	}
	r := got[0]
	if r.Group != "interactive" || r.AvgWaiting != 4 || r.MaxWaiting != 6 {
		t.Errorf("got %+v, want interactive avgWaiting=4 maxWaiting=6", r)
	}

	// Prune everything before ts 5000 → empty.
	if err := st.PruneLaneSamples(5000); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LaneDepthSeries(0, 3_600_000); len(got) != 0 {
		t.Errorf("after prune want 0 rows, got %d", len(got))
	}
}

// TestPruneActivity deletes rows older than the cutoff, keeping recent ones.
func TestPruneActivity(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, ts := range []int64{100, 200, 5000, 6000} {
		if err := st.InsertActivity(Activity{TS: ts, Served: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.PruneActivity(1000) // drop ts < 1000 (the 100 and 200 rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	got, _ := st.RecentActivity(10)
	if len(got) != 2 {
		t.Errorf("remaining %d rows, want 2", len(got))
	}
}

// TestMigrationsIdempotent: Open applies the upgrade migrations and is safe to
// call repeatedly against the same database (duplicate-column errors swallowed).
func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	// A shared in-memory DB across connections via the file: DSN.
	dsn := "file:store_migrate_test?mode=memory&cache=shared"
	a, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = a.Close() }()
	if _, err := Open(ctx, dsn); err != nil {
		t.Fatalf("second open (migrations must be idempotent): %v", err)
	}
}
