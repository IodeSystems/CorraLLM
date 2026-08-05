package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// TestUtilization: the row set is what was ASKED FOR in the window (not the
// catalog), promise outcomes are counted per model, and the measured wait
// excludes both instant admissions and rejections.
func TestUtilization(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	rows := []store.Activity{
		// big: two queued-then-admitted requests (200ms, 800ms) → mean 500ms.
		{TS: now - 300_000, Served: "big", Backend: "big", Status: 200, QueuedMS: 200},
		{TS: now - 290_000, Served: "big", Backend: "big", Status: 200, QueuedMS: 800},
		// Admitted instantly: excluded, or a quiet hour drags the mean to zero.
		{TS: now - 280_000, Served: "big", Backend: "big", Status: 200, QueuedMS: 0},
		// A rejection's queued_ms measures waiting before being turned AWAY —
		// a different quantity, and huge. Must not pollute the mean.
		{TS: now - 270_000, Served: "big", Backend: "big", Status: 429,
			Error: "queue-timeout", QueuedMS: 60_000, RetryAfterMS: 5000},
		{TS: now - 265_000, Served: "big", Backend: "big", Status: 200}, // the return → honored
		// gone: promised 3s long ago, never returned.
		{TS: now - 200_000, Served: "big", Backend: "big", Key: "ghost", Status: 429,
			Error: "exhausted", RetryAfterMS: 3000},
		// small: called, never queued, never refused.
		{TS: now - 100_000, Served: "small", Backend: "small", Status: 200, QueuedMS: 0},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{Models: map[string]config.Model{"big": {}, "small": {}, "never-called": {}}}
	h := &Handlers{Store: st, Sched: sched.New(), Cfg: cfg}

	out, err := h.Utilization(context.Background(), &UtilizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]UtilizationRow{}
	for _, r := range out.Body.Rows {
		got[r.Served] = r
	}
	// A model nobody called is absent, not a zero row.
	if _, ok := got["never-called"]; ok {
		t.Errorf("never-called must not appear: %+v", out.Body.Rows)
	}
	if len(out.Body.Rows) != 2 {
		t.Fatalf("want rows for big+small only, got %d: %+v", len(out.Body.Rows), out.Body.Rows)
	}

	big := got["big"]
	if big.RealWaitMS != 500 || big.MaxWaitMS != 800 || big.QueuedSamples != 2 {
		t.Errorf("big wait = mean %d / max %d / n %d; want 500 / 800 / 2 (instant admits and the 429 excluded)",
			big.RealWaitMS, big.MaxWaitMS, big.QueuedSamples)
	}
	if big.Turned != 2 {
		t.Errorf("big turned away = %d, want 2", big.Turned)
	}
	if big.NotHonored != 1 {
		t.Errorf("big notHonored = %d, want 1 (the ghost)", big.NotHonored)
	}
	if big.Promised != 0 {
		t.Errorf("big promised = %d, want 0 (both promises are long past due)", big.Promised)
	}

	// small queued for nothing and was never refused: a real row, all zeros.
	small := got["small"]
	if small.QueuedSamples != 0 || small.Turned != 0 || small.RealWaitMS != 0 {
		t.Errorf("small = %+v; want an all-zero row", small)
	}

	// Busiest first. With no live load, the tiebreak is who was turned away most.
	if out.Body.Rows[0].Served != "big" {
		t.Errorf("want big first (2 turned away), got %s", out.Body.Rows[0].Served)
	}
	if out.Body.Minutes != 60 {
		t.Errorf("minutes = %d, want the 60 default", out.Body.Minutes)
	}
}
