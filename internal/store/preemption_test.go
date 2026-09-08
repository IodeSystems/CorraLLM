package store

import (
	"context"
	"testing"
)

// A 499 is two opposite stories: the box taking a slot back, and a caller
// hanging up. Counting the status would report every abandoned request as a
// preemption, which is the reassurance this exists to give running backwards.
func TestPreemptionsCountsTheReasonNotTheStatus(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	add := func(ts int64, status int, reason string) {
		if err := st.InsertActivity(Activity{
			TS: ts, Served: "m", Requested: "m", Key: "k", Path: "/v1/chat/completions",
			Status: status, Error: reason,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(1000, 499, "client canceled")
	add(2000, 499, "preempted")
	add(3000, 499, "client canceled")
	add(4000, 200, "")

	n, last, err := st.Preemptions()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — the cancellations are not preemptions", n)
	}
	if last != 2000 {
		t.Errorf("last = %d, want 2000", last)
	}
}

// Zero is the answer that matters most, and it must come back as a clean zero
// rather than an error from a NULL max.
func TestPreemptionsOnABoxThatHasNeverPreempted(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	n, last, err := st.Preemptions()
	if err != nil {
		t.Fatalf("errored on an empty log: %v", err)
	}
	if n != 0 || last != 0 {
		t.Errorf("got %d/%d, want zeroes", n, last)
	}
}
