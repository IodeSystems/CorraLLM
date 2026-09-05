package store

import (
	"context"
	"testing"
)

// A Window with both ends set reads a fixed span — the thing "since N ago" could
// not express, and the reason the dashboard could answer "the last hour" but not
// "what happened at 09:00" (plan/p30-information-architecture.md §0).
//
// The property under test is TILING: the upper bound is exclusive, so two
// adjacent windows share no row and their counts sum to the whole. An inclusive
// bound double-counts every request that landed exactly on the seam — rare, real,
// and invisible in a chart.
func TestWindowTilesAndExcludesItsUpperBound(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// One row exactly on each boundary, and one strictly inside each half.
	for _, a := range []Activity{
		{TS: 1000, Served: "m", Status: 200, CostUSD: 1},
		{TS: 1500, Served: "m", Status: 200, CostUSD: 1},
		{TS: 2000, Served: "m", Status: 200, CostUSD: 1}, // the seam
		{TS: 2500, Served: "m", Status: 200, CostUSD: 1},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	count := func(w Window) int64 {
		rows, err := st.RollupByModel(w)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return 0
		}
		return rows[0].Requests
	}

	first := count(Between(1000, 2000))
	second := count(Between(2000, 3000))
	whole := count(Between(1000, 3000))

	// The row AT 2000 belongs to the second window and to neither twice.
	if first != 2 {
		t.Errorf("[1000,2000) = %d requests, want 2 (the row at 2000 is not in it)", first)
	}
	if second != 2 {
		t.Errorf("[2000,3000) = %d requests, want 2 (the row at 2000 is in it)", second)
	}
	if first+second != whole {
		t.Errorf("adjacent windows do not tile: %d + %d != %d", first, second, whole)
	}
}

// The zero value reads everything, and Since keeps the old open-ended behaviour.
// Both are relied on by every caller that has not been given a time control yet.
func TestWindowZeroValueAndSince(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, ts := range []int64{100, 500, 900} {
		if err := st.InsertActivity(Activity{TS: ts, Served: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.RollupByModel(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Requests != 3 {
		t.Fatalf("Window{} should read the whole log, got %+v", all)
	}

	recent, err := st.RollupByModel(Since(500))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Requests != 2 {
		t.Fatalf("Since(500) should include its own boundary and nothing older, got %+v", recent)
	}

	// An upper bound alone is legal: everything before a moment.
	older, err := st.RollupByModel(Window{ToMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].Requests != 1 {
		t.Fatalf("Window{ToMS: 500} should read only what precedes it, got %+v", older)
	}
}
