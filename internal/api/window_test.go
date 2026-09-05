package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// The absolute window is what makes "show me 09:00 this morning" answerable, and
// the property that matters is that it WINS over the relative one. Every panel
// used to carry its own "since N ago"; the dashboard's single time control sends
// from/to instead, and a panel that quietly kept using its own minutes would
// report a different span from the one on screen (P30 §0).
func TestWindowOfPrefersAbsoluteBounds(t *testing.T) {
	now := time.Now().UnixMilli()

	// Absolute wins, even with a relative window also set.
	got := windowOf(1000, 2000, 60)
	if got != (store.Window{FromMS: 1000, ToMS: 2000}) {
		t.Errorf("absolute bounds should win over minutes, got %+v", got)
	}

	// A start with no end means "since then, until now" — what an operator means
	// by "since 09:00".
	if got := windowOf(1000, 0, 60); got.FromMS != 1000 || got.ToMS != 0 {
		t.Errorf("from with no to should stay open-ended, got %+v", got)
	}

	// No bounds: the endpoint keeps the relative window it has always had.
	rel := windowOf(0, 0, 60)
	if rel.ToMS != 0 || rel.FromMS > now-59*60_000 || rel.FromMS < now-61*60_000 {
		t.Errorf("60 minutes should resolve to roughly now-1h, got %+v (now=%d)", rel, now)
	}

	// No bounds and no relative window: the whole log.
	if got := windowOf(0, 0, 0); got != (store.Window{}) {
		t.Errorf("no window at all should read everything, got %+v", got)
	}
}

// The activity log itself honours the window, which is the one that answers "what
// was happening at 09:00" rather than "the newest N rows, whenever they were".
func TestRecentActivityHonoursTheWindow(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, ts := range []int64{1_000, 5_000, 9_000} {
		if err := st.InsertActivity(store.Activity{TS: ts, Served: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{Store: st}

	out, err := h.RecentActivity(context.Background(), &RecentActivityInput{FromMS: 2_000, ToMS: 9_000})
	if err != nil {
		t.Fatal(err)
	}
	// [2000, 9000): the row at 5000 only. 1000 is before it; 9000 is the
	// exclusive upper bound, so it belongs to the NEXT window, not this one.
	if len(out.Body.Records) != 1 || out.Body.Records[0].TS != 5_000 {
		t.Fatalf("want only the row at ts=5000, got %+v", out.Body.Records)
	}

	// Unbounded still reads everything, so no existing caller changes behaviour.
	all, err := h.RecentActivity(context.Background(), &RecentActivityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Body.Records) != 3 {
		t.Fatalf("an unbounded request should read the whole log, got %d rows", len(all.Body.Records))
	}
}
