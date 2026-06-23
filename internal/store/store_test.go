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
	if got[0] != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], in)
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
