package store

import (
	"context"
	"testing"
)

// TestRetryPromises: a 429 row carries the backoff we promised, and the query
// correlates each promise with the caller's next request so "did they come back"
// is answerable without reading the whole log.
func TestRetryPromises(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const base = 1_000_000

	rows := []Activity{
		// alice: told 4s, came back at +5s → honored.
		{TS: base, Served: "m", Key: "alice", Status: 429, Error: "rejected", RetryAfterMS: 4000},
		{TS: base + 5000, Served: "m", Key: "alice", Status: 200},
		// bob: told 10s, came back at +2s → jumped the gun.
		{TS: base + 100, Served: "m", Key: "bob", Status: 429, Error: "queue-timeout", RetryAfterMS: 10_000},
		{TS: base + 2100, Served: "m", Key: "bob", Status: 200},
		// carol: told 3s and never returned. Now is far past due → gone.
		{TS: base + 200, Served: "m", Key: "carol", Status: 429, Error: "exhausted", RetryAfterMS: 3000},
		// A 200 is not a promise, and neither is a 503 (a pause is permanent).
		{TS: base + 300, Served: "m", Key: "dave", Status: 200},
		{TS: base + 400, Served: "m", Key: "dave", Status: 503, Error: "paused"},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RetryPromises(0, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 promises (429s only), got %d: %+v", len(got), got)
	}
	// Newest first: carol (+200), bob (+100), alice (+0).
	byKey := map[string]RetryPromise{}
	for _, p := range got {
		byKey[p.Key] = p
	}
	if p := byKey["alice"]; p.RetryAfterMS != 4000 || p.ReturnedMS != base+5000 {
		t.Errorf("alice: promised %d, returned %d (want 4000 / %d)", p.RetryAfterMS, p.ReturnedMS, base+5000)
	}
	if p := byKey["bob"]; p.ReturnedMS != base+2100 {
		t.Errorf("bob returned %d, want %d", p.ReturnedMS, base+2100)
	}
	if p := byKey["carol"]; p.ReturnedMS != 0 {
		t.Errorf("carol never came back; want 0, got %d", p.ReturnedMS)
	}
	if got[0].Key != "carol" || got[2].Key != "alice" {
		t.Errorf("want newest-first order, got %s…%s", got[0].Key, got[2].Key)
	}

	// The key filter narrows to one caller (the per-key page).
	one, err := st.RetryPromises(0, 50, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Key != "bob" {
		t.Fatalf("key filter = %+v", one)
	}

	// sinceMS bounds the window: carol's promise is the only one at/after +200.
	recent, err := st.RetryPromises(base+200, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Key != "carol" {
		t.Fatalf("since filter = %+v", recent)
	}
}

// TestRetryPromisesUnkeyed: unkeyed callers correlate by source IP, not by the
// empty key — otherwise the first unrelated anonymous request in the log would
// look like an instant return for every one of them.
func TestRetryPromisesUnkeyed(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const base = 2_000_000
	rows := []Activity{
		{TS: base, Served: "m", SourceIP: "10.0.0.1", Status: 429, Error: "rejected", RetryAfterMS: 5000},
		// A DIFFERENT anonymous host, one second later. Must not count as .1's return.
		{TS: base + 1000, Served: "m", SourceIP: "10.0.0.2", Status: 200},
		{TS: base + 6000, Served: "m", SourceIP: "10.0.0.1", Status: 200},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.RetryPromises(0, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 promise, got %d", len(got))
	}
	if got[0].ReturnedMS != base+6000 {
		t.Errorf("returned %d, want %d (the same IP's next request, not another host's)",
			got[0].ReturnedMS, base+6000)
	}
}

// The question the placement thread was built toward: not "how does this model
// behave" but "how does it behave ON THAT BOX". With one model served from two
// machines, a figure averaged across both describes neither.
func TestRecentActivityFiltersByPlacement(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	seedActivity(t, st, []Activity{
		{TS: 1, Served: "qwen", Placement: "box1", DwellMS: 100},
		{TS: 2, Served: "qwen", Placement: "mac1", DwellMS: 900},
		{TS: 3, Served: "qwen", Placement: "box1", DwellMS: 120},
		// Written before the column existed: belongs to no placement, and must
		// not be swept into either one's numbers.
		{TS: 4, Served: "qwen", Placement: "", DwellMS: 50},
	})

	box, err := st.RecentActivity(10, "qwen", "", "box1")
	if err != nil {
		t.Fatal(err)
	}
	if len(box) != 2 {
		t.Fatalf("box1 rows = %d, want 2", len(box))
	}
	for _, r := range box {
		if r.Placement != "box1" {
			t.Errorf("box1 filter returned a %q row", r.Placement)
		}
	}

	mac, err := st.RecentActivity(10, "qwen", "", "mac1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mac) != 1 || mac[0].DwellMS != 900 {
		t.Errorf("mac1 rows = %+v, want the single 900ms row", mac)
	}

	// Unfiltered still returns everything, including the pre-column row —
	// narrowing is opt-in, and a missing placement is not a reason to hide
	// history.
	all, err := st.RecentActivity(10, "qwen", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("unfiltered rows = %d, want all 4", len(all))
	}
}
