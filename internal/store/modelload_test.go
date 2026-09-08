package store

import (
	"context"
	"testing"
)

func loadStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The two facts the activity log could never answer: who triggered the load,
// and what it displaced. Without the second, "this model keeps reloading" is
// half a story — which is why the 2026-09-05 slowdown had to be reconstructed
// from a database session instead of read off a screen.
func TestAModelLoadRecordsWhoAskedAndWhatItDisplaced(t *testing.T) {
	st := loadStore(t)
	if err := st.InsertModelLoad(ModelLoad{
		TS: 2000, Model: "local-Qwen3.8-27B", Server: "box1",
		Requester: "sk-aw4", Evicted: "local-chandra-ocr-2", MS: 31_000, OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ModelLoads(Window{}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Requester != "sk-aw4" {
		t.Errorf("requester = %q, want the caller that triggered it", r.Requester)
	}
	if r.Evicted != "local-chandra-ocr-2" {
		t.Errorf("evicted = %q, want what was unloaded to fit it", r.Evicted)
	}
	if !r.OK || r.MS != 31_000 {
		t.Errorf("ok/ms = %v/%d, want true/31000", r.OK, r.MS)
	}
}

// A FAILED load is the more interesting row of the two — it is the one somebody
// goes looking for — so it must be kept, with its reason.
func TestAFailedLoadIsKeptWithItsReason(t *testing.T) {
	st := loadStore(t)
	if err := st.InsertModelLoad(ModelLoad{
		TS: 1000, Model: "m", Server: "box1", MS: 900, OK: false, Err: "cudaMalloc failed",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ModelLoads(Window{}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].OK {
		t.Fatalf("a failed load was dropped or marked ok: %+v", rows)
	}
	if rows[0].Err != "cudaMalloc failed" {
		t.Errorf("err = %q, want the reason it failed", rows[0].Err)
	}
}

// A preload at boot has no requester, and an empty string must not be confused
// with a caller named "".
func TestAPreloadHasNoRequester(t *testing.T) {
	st := loadStore(t)
	if err := st.InsertModelLoad(ModelLoad{TS: 1, Model: "m", OK: true}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ModelLoads(Window{}, "", 10)
	if len(rows) != 1 || rows[0].Requester != "" {
		t.Fatalf("got %+v, want an empty requester", rows)
	}
}

// Newest first, and filterable to one model — the two ways this gets read.
func TestModelLoadsAreNewestFirstAndFilterable(t *testing.T) {
	st := loadStore(t)
	for _, l := range []ModelLoad{
		{TS: 1000, Model: "a", OK: true},
		{TS: 3000, Model: "b", OK: true},
		{TS: 2000, Model: "a", OK: true},
	} {
		if err := st.InsertModelLoad(l); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := st.ModelLoads(Window{}, "", 10)
	if len(all) != 3 || all[0].TS != 3000 {
		t.Fatalf("order = %+v, want newest first", all)
	}
	justA, _ := st.ModelLoads(Window{}, "a", 10)
	if len(justA) != 2 {
		t.Fatalf("filtered = %d rows, want a's 2", len(justA))
	}
	windowed, _ := st.ModelLoads(Between(1500, 2500), "", 10)
	if len(windowed) != 1 || windowed[0].TS != 2000 {
		t.Fatalf("windowed = %+v, want just the 2000 row", windowed)
	}
}
