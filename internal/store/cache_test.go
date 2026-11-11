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
