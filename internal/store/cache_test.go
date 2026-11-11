package store

import (
	"context"
	"testing"
)

func cacheStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Events read back in order, narrowed by model and window, and age out with
// the activity they describe.
func TestBackendEventsRoundTrip(t *testing.T) {
	st := cacheStore(t)
	for _, e := range []BackendEvent{
		{TS: 100, Model: "swift", Instance: "pgid:1", Kind: "start", Reason: "cold-spawn", OK: true},
		{TS: 200, Model: "other", Instance: "pgid:2", Kind: "start", Reason: "cold-spawn", OK: true},
		{TS: 300, Model: "swift", Instance: "pgid:1", Kind: "stop", Reason: "crash:exit status 1", Err: "x"},
	} {
		if err := st.InsertBackendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.BackendEvents("swift", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "start" || !got[0].OK || got[1].Reason != "crash:exit status 1" || got[1].OK {
		t.Fatalf("got %+v", got)
	}
	if got, _ := st.BackendEvents("", 150, 1000); len(got) != 2 {
		t.Errorf("window: got %d events, want 2", len(got))
	}
	if _, err := st.PruneActivity(250); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.BackendEvents("", 0, 1000); len(got) != 1 {
		t.Errorf("after prune: got %d events, want 1", len(got))
	}
}

// A cache row rides its activity insert, reads back with the caller joined
// from activity, and is pruned with it; LastTurns gives each conversation's
// newest turn.
func TestCacheRowsRoundTrip(t *testing.T) {
	st := cacheStore(t)
	for i, ts := range []int64{100, 200, 300} {
		a := Activity{TS: ts, Served: "swift", Key: "sk-aw4", Path: "/v1/chat/completions", Status: 200,
			PromptTokens: 1000 * (i + 1), CompletionTokens: 10, Conversation: "conv", ConversationMessages: 1 + 2*i,
			Cache: &CacheRow{Served: "swift", Conversation: "conv", Instance: "pgid:1", Slot: i - 1,
				PromptTokens: 1000 * (i + 1), MatchChars: -1, Class: "hit"}}
		if _, err := st.InsertActivityID(a); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.CacheRows("swift", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Caller != "sk-aw4" || rows[2].Slot != 1 || rows[1].TS != 200 || rows[0].MatchChars != -1 {
		t.Fatalf("rows = %+v", rows)
	}
	turns, err := st.LastTurns(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].PromptTokens != 3000 || turns[0].ActivityID != rows[2].ActivityID {
		t.Fatalf("last turns = %+v", turns)
	}
	if _, err := st.PruneActivity(250); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.CacheRows("", 0, 1000); len(rows) != 1 {
		t.Errorf("after prune: %d rows, want 1", len(rows))
	}
}
