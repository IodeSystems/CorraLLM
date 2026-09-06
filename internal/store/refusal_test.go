package store

import (
	"context"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The clock starts on the FIRST refusal and does not move. "How long has this
// been broken" is the question; "when did it last fail" is already in the log,
// and letting later refusals push `since` forward would silently answer the
// second question while looking like the first.
func TestRefusalKeepsTheFirstTimestampAndCounts(t *testing.T) {
	st := openTest(t)
	for _, at := range []int64{1_000, 2_000, 3_000} {
		if err := st.NoteBackendRefusal("cerebras-gpt-oss-120b", 402, at); err != nil {
			t.Fatalf("note: %v", err)
		}
	}
	rows, err := st.RefusingBackends()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.SinceMS != 1_000 {
		t.Errorf("since = %d, want the first refusal (1000)", r.SinceMS)
	}
	if r.LastMS != 3_000 {
		t.Errorf("last = %d, want the most recent (3000)", r.LastMS)
	}
	if r.Count != 3 {
		t.Errorf("count = %d, want 3", r.Count)
	}
	if r.Status != 402 {
		t.Errorf("status = %d, want 402", r.Status)
	}
}

// Serving again clears it. The table is a list of what is broken NOW, so a
// recovered backend leaves no row rather than a zeroed one.
func TestRefusalClearsWhenItServesAgain(t *testing.T) {
	st := openTest(t)
	if err := st.NoteBackendRefusal("groq-gpt-oss-120b", 401, 1_000); err != nil {
		t.Fatalf("note: %v", err)
	}
	if err := st.ClearBackendRefusal("groq-gpt-oss-120b"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	rows, err := st.RefusingBackends()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want no rows after recovery, got %d", len(rows))
	}
}

// Longest-broken first: a rung that died a week ago is a different problem from
// one that died a minute ago, and the reader should meet them in that order.
func TestRefusingBackendsAreOrderedLongestBrokenFirst(t *testing.T) {
	st := openTest(t)
	if err := st.NoteBackendRefusal("recent", 403, 9_000); err != nil {
		t.Fatalf("note: %v", err)
	}
	if err := st.NoteBackendRefusal("ancient", 402, 1_000); err != nil {
		t.Fatalf("note: %v", err)
	}
	rows, err := st.RefusingBackends()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].Backend != "ancient" {
		t.Fatalf("order = %v, want ancient first", rows)
	}
}
