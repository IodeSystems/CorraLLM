package store

import (
	"context"
	"testing"
)

func pressureStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A full memory bar is not a fault — a device pool at 96% normally means a
// model is RESIDENT, which is the box working. It becomes a problem only when
// something else wants the space, and that is an EVENT, not a level. This is
// the event.
func TestEvictionPressureNamesTheMostRecentTrade(t *testing.T) {
	st := pressureStore(t)
	for _, l := range []ModelLoad{
		{TS: 1000, Model: "a", Server: "box1", Evicted: "old-one", OK: true},
		{TS: 3000, Model: "b", Server: "box1", Evicted: "chandra-ocr-2", OK: true},
		{TS: 2000, Model: "c", Server: "box1", OK: true}, // fit without evicting
		{TS: 2500, Model: "d", Server: "mac", Evicted: "x", OK: true},
	} {
		if err := st.InsertModelLoad(l); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.EvictionPressureSince(Window{})
	if err != nil {
		t.Fatal(err)
	}
	byServer := map[string]EvictionPressure{}
	for _, p := range got {
		byServer[p.Server] = p
	}
	b := byServer["box1"]
	if b.Count != 2 {
		t.Errorf("box1 count = %d, want 2 — the load that simply fit is not pressure", b.Count)
	}
	if b.LastEvicted != "chandra-ocr-2" || b.LastFor != "b" {
		t.Errorf("last trade = %q for %q, want chandra-ocr-2 for b", b.LastEvicted, b.LastFor)
	}
	if byServer["mac"].Count != 1 {
		t.Errorf("mac count = %d, want 1", byServer["mac"].Count)
	}
}

// The normal case is silence. A box whose models simply fit reports nothing,
// so the panel says nothing — which is what stops a full bar reading as a
// fault the reader cannot act on (OB-6).
func TestNoPressureWhenEverythingFits(t *testing.T) {
	st := pressureStore(t)
	for _, l := range []ModelLoad{
		{TS: 1000, Model: "a", Server: "box1", OK: true},
		{TS: 2000, Model: "b", Server: "box1", OK: true},
	} {
		if err := st.InsertModelLoad(l); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.EvictionPressureSince(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing to report", got)
	}
}

// Windowed, because "three times last week" and "three times in the last hour"
// are different news about the same box.
func TestEvictionPressureRespectsTheWindow(t *testing.T) {
	st := pressureStore(t)
	for _, l := range []ModelLoad{
		{TS: 1000, Model: "a", Server: "box1", Evicted: "x", OK: true},
		{TS: 9000, Model: "b", Server: "box1", Evicted: "y", OK: true},
	} {
		if err := st.InsertModelLoad(l); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.EvictionPressureSince(Between(5000, 10000))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Count != 1 || got[0].LastEvicted != "y" {
		t.Fatalf("got %+v, want just the in-window trade", got)
	}
}
