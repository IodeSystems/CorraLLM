package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/store"
)

// A request that started before the window and ended inside it counts only the
// part inside. The window is [60s, 120s); the request held its slot from 30s to
// 90s, so 30s of it is in the window.
func TestSlotLoadClipsToWindow(t *testing.T) {
	spans := []store.LoadSpan{{Served: "m", EndMS: 90_000, DwellMS: 60_000}}
	a := aggregateSlotLoad(spans, nil, 60_000, 120_000)["m"]
	near(t, "busyMS", a.busyMS, 30_000)
	if a.arrivals != 0 {
		t.Errorf("arrivals = %d, want 0: it arrived before the window", a.arrivals)
	}
	w := windowOfAgg(a, 1, 3)
	near(t, "busySlotsAvg", w.BusySlotsAvg, 0.5)
	near(t, "occupancyPct", w.OccupancyPct, 100*0.5/3)
}

// The case the view exists for: two requests decoding at once on a 3-slot
// model. Per-request speed stays at 78; total output is twice that, and two of
// three slots were busy the whole minute.
func TestSlotLoadOverlappingRequests(t *testing.T) {
	// Each decodes 78×60 tokens at 78 tok/s across exactly the window [0, 60s).
	spans := []store.LoadSpan{
		{Served: "m", EndMS: 60_000, DwellMS: 60_000, CompletionTokens: 78 * 60, PredictedPerSec: 78},
		{Served: "m", EndMS: 60_000, DwellMS: 60_000, CompletionTokens: 78 * 60, PredictedPerSec: 78},
	}
	w := windowOfAgg(aggregateSlotLoad(spans, nil, 0, 60_000)["m"], 1, 3)
	near(t, "tokPerSecTotal", w.TokPerSecTotal, 156)
	near(t, "tokPerSecPerRequest", w.TokPerSecPerRequest, 78)
	near(t, "busySlotsAvg", w.BusySlotsAvg, 2)
	near(t, "occupancyPct", w.OccupancyPct, 200.0/3)
	if w.Arrivals != 2 || w.PerRequestSamples != 2 {
		t.Errorf("arrivals=%d samples=%d, want 2 and 2", w.Arrivals, w.PerRequestSamples)
	}
}

// Tokens go on the DECODE span (end − tokens/rate), not the whole service
// span: 600 tokens at 60 tok/s decode over the last 10 s of a 40 s request.
// A window holding the last 5 s gets half of them; one holding only the
// prefill gets none.
func TestSlotLoadTokensOnDecodeSpan(t *testing.T) {
	spans := []store.LoadSpan{{Served: "m", EndMS: 100_000, DwellMS: 40_000, CompletionTokens: 600, PredictedPerSec: 60}}
	near(t, "last 5s", aggregateSlotLoad(spans, nil, 95_000, 160_000)["m"].tokens, 300)
	near(t, "prefill only", aggregateSlotLoad(spans, nil, 60_000, 89_000)["m"].tokens, 0)
	// No reported rate: spread over the service span (queue and load excluded).
	spans[0].PredictedPerSec, spans[0].QueuedMS = 0, 20_000 // service 80s→100s
	near(t, "no rate", aggregateSlotLoad(spans, nil, 90_000, 160_000)["m"].tokens, 300)
}

// Queue time is not slot time: a request that waited 20 s then served 10 s
// held its slot for 10 s.
func TestSlotLoadQueueIsNotBusy(t *testing.T) {
	spans := []store.LoadSpan{{Served: "m", EndMS: 30_000, DwellMS: 30_000, QueuedMS: 20_000}}
	a := aggregateSlotLoad(spans, nil, 0, 60_000)["m"]
	near(t, "busyMS", a.busyMS, 10_000)
	if len(a.waits) != 1 || a.waits[0] != 20_000 {
		t.Errorf("waits = %v, want [20000]", a.waits)
	}
}

func TestSlotLoadQueuePercentiles(t *testing.T) {
	// Ten arrivals: six never waited, the rest waited 1, 2, 4 and 8 s.
	var spans []store.LoadSpan
	for _, q := range []int64{0, 0, 0, 0, 0, 0, 1000, 2000, 4000, 8000} {
		spans = append(spans, store.LoadSpan{Served: "m", EndMS: 50_000, DwellMS: 10_000 + q, QueuedMS: q})
	}
	w := windowOfAgg(aggregateSlotLoad(spans, nil, 0, 60_000)["m"], 1, 1)
	if w.QueueWaitP50MS != 0 || w.QueueWaitP90MS != 4000 || w.Queued != 4 {
		t.Errorf("p50=%d p90=%d queued=%d, want 0, 4000, 4", w.QueueWaitP50MS, w.QueueWaitP90MS, w.Queued)
	}
	near(t, "arrivalsPerMin", w.ArrivalsPerMin, 10)
	if got := percentileMS(nil, 0.9); got != 0 {
		t.Errorf("empty percentile = %d, want 0", got)
	}
}

// In-flight requests count: a streaming one holds a slot and contributes its
// chunks, a queued one is an arrival whose wait so far counts, and a queued
// one with no backend is attributed through the name it asked for.
func TestSlotLoadLiveRequests(t *testing.T) {
	now := int64(600_000)
	in := []proxy.InflightInfo{
		{Served: "chat", Backend: "m", State: "streaming", Streaming: true, Chunks: 1200, ElapsedMS: 40_000, StateMS: 30_000},
		{Served: "chat", State: "queued", ElapsedMS: 5_000, StateMS: 5_000},
	}
	live := liveSpansOf(in, now, func(n string) string {
		if n == "chat" {
			return "m"
		}
		return ""
	})
	if len(live) != 2 || live[1].Model != "m" {
		t.Fatalf("live = %+v, want both on m", live)
	}
	a := aggregateSlotLoad(nil, live, now-60_000, now)["m"]
	near(t, "busyMS", a.busyMS, 30_000)
	near(t, "tokens", a.tokens, 1200)
	if a.arrivals != 2 {
		t.Errorf("arrivals = %d, want 2", a.arrivals)
	}
	// Streaming one waited 10 s (40 elapsed − 30 streaming), queued one 5 s so far.
	if got := percentileMS(a.waits, 0.9); got != 10_000 {
		t.Errorf("p90 wait = %d, want 10000", got)
	}
	// Half of its streaming time is in a window covering the last 15 s.
	near(t, "tokens last 15s", aggregateSlotLoad(nil, live, now-15_000, now)["m"].tokens, 600)
}

// End to end over the store: only local models get rows, config slots stand in
// when the scheduler has never admitted to the model, and the total adds up.
func TestSlotLoadHandler(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UnixMilli()
	for _, a := range []store.Activity{
		{TS: now - 1_000, Served: "local-a", Path: "/v1/chat/completions", Status: 200, DwellMS: 30_000, CompletionTokens: 780, PredictedPerSec: 78},
		{TS: now - 2_000, Served: "local-b", Path: "/v1/chat/completions", Status: 200, DwellMS: 10_000, QueuedMS: 3_000},
		{TS: now - 1_000, Served: "remote-c", Path: "/v1/chat/completions", Status: 200, DwellMS: 5_000},
		{TS: now - 20*60_000, Served: "local-a", Path: "/v1/chat/completions", Status: 200, DwellMS: 5_000},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{Store: st, Cfg: &config.Config{Models: map[string]config.Model{
		"local-a":  {Cmd: "llama-server --parallel 3", MaxConcurrent: 3},
		"local-b":  {Cmd: "llama-server"},
		"remote-c": {}, // no process here: not a local model
	}}}
	out, err := h.SlotLoad(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := out.Body
	if len(b.Models) != 2 {
		t.Fatalf("models = %+v, want local-a and local-b only", b.Models)
	}
	byName := map[string]SlotLoadRow{}
	for _, r := range b.Models {
		byName[r.Model] = r
	}
	a := byName["local-a"]
	if a.Slots != 3 || a.SlotsSource != "config" || len(a.Windows) != 2 {
		t.Errorf("local-a = %+v, want 3 config slots and two windows", a)
	}
	// 780 tokens at 78 tok/s: 10 s of decode ending 1 s ago, all inside 1 min.
	near(t, "local-a 1m tok/s", a.Windows[0].TokPerSecTotal, 13)
	near(t, "local-a 15m tok/s", a.Windows[1].TokPerSecTotal, 780.0/900)
	if b.Total.Slots != 4 {
		t.Errorf("total slots = %d, want 3 + 1", b.Total.Slots)
	}
	if got := b.Total.Windows[1].QueueWaitP90MS; got != 3_000 {
		t.Errorf("total p90 wait = %d, want 3000 (pooled across models)", got)
	}
	// Total occupancy over 15 min: (30 s + 7 s) slot-seconds / (4 slots × 900 s).
	near(t, "total 15m occupancy", b.Total.Windows[1].OccupancyPct, 100*37.0/(4*900))
}
