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
