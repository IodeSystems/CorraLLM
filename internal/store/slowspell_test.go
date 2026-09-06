package store

import (
	"context"
	"testing"
)

func spellStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func add(t *testing.T, st *Store, tsMS, dwellMS int64, status int) {
	t.Helper()
	if err := st.InsertActivity(Activity{
		TS: tsMS, Served: "m", Requested: "m", Key: "k", Path: "/v1/chat/completions",
		Status: status, DwellMS: dwellMS,
	}); err != nil {
		t.Fatal(err)
	}
}

// The hour that produced the rule: a true summary over a window that contained
// an unusable stretch. The mean stays low, so nothing in the aggregate moves —
// the spell has to be found separately or it is not found at all.
func TestSlowSpellFindsTheStretchTheMeanHides(t *testing.T) {
	st := spellStore(t)
	base := int64(1_000_000)
	for i := int64(0); i < 400; i++ {
		add(t, st, base+i*1000, 1_000, 200)
	}
	// Eight consecutive slow ones, as on the box.
	for i := int64(0); i < 8; i++ {
		add(t, st, base+500_000+i*1000, 36_000, 200)
	}

	got, err := st.SlowSpell(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 8 {
		t.Errorf("count = %d, want the 8 slow ones", got.Count)
	}
	if got.WorstMS != 36_000 {
		t.Errorf("worst = %d ms, want 36000", got.WorstMS)
	}
	if got.FirstMS != base+500_000 || got.LastMS != base+500_000+7_000 {
		t.Errorf("span = %d..%d, want the stretch's own bounds", got.FirstMS, got.LastMS)
	}
	if got.WorstAtMS == 0 {
		t.Error("worst has no timestamp; 'when' is the part a person can act on")
	}
	// The mean is exactly what made this invisible — it must stay low, or the
	// test is not reproducing the problem.
	if got.MeanMS > 2_000 {
		t.Errorf("mean = %d ms; the whole point is that the average did not move", got.MeanMS)
	}
}

// An ordinary window reports nothing. A rule that fires on normal traffic
// trains the reader to scroll past the one sentence written to stop them.
func TestSlowSpellIsSilentOnAnOrdinaryWindow(t *testing.T) {
	st := spellStore(t)
	base := int64(1_000_000)
	for i := int64(0); i < 100; i++ {
		// Ordinary spread, nothing past 3x the mean.
		add(t, st, base+i*1000, 1_000+(i%5)*300, 200)
	}
	got, err := st.SlowSpell(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 {
		t.Errorf("count = %d on an ordinary window, want 0", got.Count)
	}
}

// A rejection is refused in milliseconds. Letting 429s into the mean would drag
// it down and make the threshold it defines too easy to clear — the panel would
// then invent a spell out of ordinary requests on a busy, throttled box.
func TestSlowSpellIgnoresRejectionsWhenAveraging(t *testing.T) {
	st := spellStore(t)
	base := int64(1_000_000)
	for i := int64(0); i < 50; i++ {
		add(t, st, base+i*1000, 4_000, 200)
	}
	for i := int64(0); i < 500; i++ {
		add(t, st, base+100_000+i*100, 2, 429) // turned away instantly
	}
	got, err := st.SlowSpell(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MeanMS < 3_000 {
		t.Errorf("mean = %d ms; rejections dragged the average down", got.MeanMS)
	}
	if got.Count != 0 {
		t.Errorf("count = %d, want 0 — the served requests were all alike", got.Count)
	}
}

// An empty window is not an incident.
func TestSlowSpellOnNothing(t *testing.T) {
	st := spellStore(t)
	got, err := st.SlowSpell(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 || got.MeanMS != 0 {
		t.Errorf("got %+v on an empty log, want zeroes", got)
	}
}
