package store

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Overlapping spans count once; a span across a bucket edge splits; the
// current bucket is measured over its elapsed part.
func TestBusyFractions(t *testing.T) {
	spans := []ServiceSpan{
		{StartMS: 0, EndMS: 400},
		{StartMS: 200, EndMS: 600}, // overlaps the first: union 0..600
		{StartMS: 900, EndMS: 1300},
		{StartMS: 2100, EndMS: 2200}, // in the third bucket, which ends at now=2500
	}
	frac, busy := BusyFractions(spans, 0, 1000, 3, 2500)
	want := []float64{0.7, 0.3, 0.2} // 600+100 of 1000; 300 of 1000; 100 of 500 elapsed
	for i := range want {
		if !near(frac[i], want[i]) {
			t.Errorf("bucket %d: %v, want %v", i, frac[i], want[i])
		}
	}
	if busy != 1100 {
		t.Errorf("busy %d ms, want 1100", busy)
	}
}

// Spans outside the axis are clipped, not counted.
func TestBusyFractionsClips(t *testing.T) {
	frac, busy := BusyFractions([]ServiceSpan{{StartMS: -500, EndMS: 500}, {StartMS: 1800, EndMS: 2600}}, 0, 1000, 2, 5000)
	if !near(frac[0], 0.5) || !near(frac[1], 0.2) || busy != 700 {
		t.Errorf("got %v busy %d, want [0.5 0.2] busy 700", frac, busy)
	}
}

// ts is the END stamp: the span runs back from it by dwell - queued - load,
// and active TTFT drops the queue and load. A non-streamed reply (first byte
// at the end) has no first-token figure.
func TestServiceSpans(t *testing.T) {
	st := payloadStore(t)
	for _, a := range []Activity{
		{TS: 10_000, Served: "m", Path: "/v1/chat/completions", Status: 200, DwellMS: 5000, QueuedMS: 1000, LoadMS: 500, TTFBMs: 2000},
		{TS: 20_000, Served: "m", Path: "/v1/chat/completions", Status: 200, DwellMS: 3000, TTFBMs: 2950},
		{TS: 30_000, Served: "m", Path: "/v1/chat/completions", Status: 429, DwellMS: 10},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ServiceSpans(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d spans, want 2 (the 429 is not served)", len(got))
	}
	a := got[0]
	if a.StartMS != 6500 || a.EndMS != 10_000 || a.ActiveTTFTMS != 500 || a.FirstByteMS != 7000 {
		t.Errorf("streamed span: %+v", a)
	}
	if got[1].ActiveTTFTMS != -1 {
		t.Errorf("non-streamed reply got a first-token figure: %+v", got[1])
	}
}
