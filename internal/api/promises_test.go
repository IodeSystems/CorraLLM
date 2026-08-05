package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// TestRetryPromises classifies each promise by what the caller did with it —
// the verdict a bare 429 count cannot give.
func TestRetryPromises(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	rows := []store.Activity{
		// honored: told 4s, returned at 5s.
		{TS: now - 20_000, Served: "m", Key: "honored", Status: 429, Error: "rejected", RetryAfterMS: 4000},
		{TS: now - 15_000, Served: "m", Key: "honored", Status: 200},
		// early: told 10s, returned after 2s.
		{TS: now - 20_000, Served: "m", Key: "early", Status: 429, Error: "rejected", RetryAfterMS: 10_000},
		{TS: now - 18_000, Served: "m", Key: "early", Status: 200},
		// gone: told 3s twenty seconds ago, never came back.
		{TS: now - 20_000, Served: "m", Key: "gone", Status: 429, Error: "exhausted", RetryAfterMS: 3000},
		// waiting: told 60s one second ago — still owed a slot.
		{TS: now - 1000, Served: "m", Key: "waiting", Status: 429, Error: "rejected", RetryAfterMS: 60_000},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handlers{Store: st}
	out, err := h.RetryPromises(context.Background(), &RetryPromisesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Promises) != 4 {
		t.Fatalf("want 4 promises, got %d", len(out.Body.Promises))
	}
	got := map[string]RetryPromiseRecord{}
	for _, p := range out.Body.Promises {
		got[p.Key] = p
	}
	for key, want := range map[string]string{
		"honored": "honored", "early": "early", "gone": "gone", "waiting": "waiting",
	} {
		if got[key].State != want {
			t.Errorf("%s: state = %q, want %q", key, got[key].State, want)
		}
	}
	// Only the outstanding one counts as still owed a slot.
	if out.Body.Waiting != 1 {
		t.Errorf("waiting = %d, want 1", out.Body.Waiting)
	}
	// Due time is derived, and the actual wait is reported for the returners so
	// promised-vs-actual is a direct comparison.
	if p := got["honored"]; p.DueMS != p.TS+4000 || p.WaitedMS != 5000 {
		t.Errorf("honored: due %d (want %d), waited %d (want 5000)", p.DueMS, p.TS+4000, p.WaitedMS)
	}
	if p := got["early"]; p.WaitedMS != 2000 {
		t.Errorf("early: waited %d, want 2000", p.WaitedMS)
	}
	// Never returned → no wait to report, rather than a misleading zero-that-means-instant.
	if p := got["gone"]; p.ReturnedMS != 0 || p.WaitedMS != 0 {
		t.Errorf("gone: returned %d, waited %d; want 0/0", p.ReturnedMS, p.WaitedMS)
	}

	// The window excludes older promises.
	narrow, err := h.RetryPromises(context.Background(), &RetryPromisesInput{Minutes: 10_080, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Body.Promises) != 4 {
		t.Fatalf("wide window: want 4, got %d", len(narrow.Body.Promises))
	}
}
