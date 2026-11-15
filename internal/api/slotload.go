package api

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/store"
)

// --- slot load: throughput, slots and queue for models with parallel slots ---
//
// Per-request decode speed (tg/s) answers "how fast does ONE answer come out".
// With several slots that number stays flat while the box does more work: three
// requests decoding at ~78 tok/s each is ~234 tok/s of output, and the old
// dashboard showed 78. This view reports what the box as a whole produced, how
// full its slots were, and how long arrivals waited for one.
//
// "Now" comes from the scheduler (slots, busy, waiting). The windows come from
// the activity log (finished requests) PLUS the proxy's in-flight registry, so
// a request still running counts toward the window it is running in instead of
// only once it finishes. Without the live half a 1-minute window misses most of
// the work: an agentic turn decodes for tens of seconds.

// slotLoadWindows are the rolling windows reported, in minutes.
var slotLoadWindows = []int{1, 15}

// SlotLoadInput has no parameters: the windows are fixed and end now.
type SlotLoadInput struct{}

// SlotLoadWindow is one rolling window ending now.
type SlotLoadWindow struct {
	Minutes int `json:"minutes" doc:"Window length in minutes, ending now."`
	// Aggregate output: what the box produced, all slots together.
	TokPerSecTotal float64 `json:"tokPerSecTotal" doc:"Completion tokens per second of wall-clock, summed across every request on this model (tok/s total). A finished request's tokens are placed on its decode span (end − tokens ÷ its reported decode rate); a running stream counts its chunks so far (≈ tokens) spread over its time streaming."`
	// Per request: the old tg/s, kept as the secondary number.
	TokPerSecPerRequest float64 `json:"tokPerSecPerRequest" doc:"Mean backend-reported decode speed of the requests that finished in the window (tok/s per request). 0 when none reported one."`
	PerRequestSamples   int     `json:"perRequestSamples" doc:"Requests behind tokPerSecPerRequest."`
	// Slot load.
	BusySlotsAvg float64 `json:"busySlotsAvg" doc:"Mean slots busy over the window (slot-seconds held ÷ window seconds). A slot counts as held from the end of queue wait and cold load to the last byte — the span ServiceStats measures."`
	OccupancyPct float64 `json:"occupancyPct" doc:"busySlotsAvg as a percent of the model's slots now (0–100; above 100 only if slots were lowered within the window)."`
	// Queue load.
	Arrivals       int     `json:"arrivals" doc:"Requests that ARRIVED in the window, answered, refused or still running."`
	ArrivalsPerMin float64 `json:"arrivalsPerMin" doc:"arrivals ÷ window minutes."`
	Queued         int     `json:"queued" doc:"Arrivals that waited in the queue at all (wait > 0)."`
	QueueWaitP50MS int64   `json:"queueWaitP50Ms" doc:"Median queue wait of the window's arrivals, milliseconds; a request still queued counts its wait so far. Includes callers who timed out of the queue."`
	QueueWaitP90MS int64   `json:"queueWaitP90Ms" doc:"90th-percentile queue wait of the window's arrivals, milliseconds."`
}

// SlotLoadRow is one local model, or the box total.
type SlotLoadRow struct {
	Model       string           `json:"model" doc:"Served model name; empty on the box total."`
	Slots       int              `json:"slots" doc:"Admission slots: the scheduler's live capacity once the model has been admitted to, else the config's maxConcurrent (default 1)."`
	SlotsSource string           `json:"slotsSource" doc:"scheduler | config — where slots came from. Empty on the box total."`
	Busy        int              `json:"busy" doc:"Slots in service right now."`
	Waiting     int              `json:"waiting" doc:"Requests queued for a slot right now."`
	Windows     []SlotLoadWindow `json:"windows" doc:"One entry per rolling window, shortest first."`
}

// SlotLoadOutput is the per-model slot load plus a box total.
type SlotLoadOutput struct {
	Body struct {
		AsOfMS int64         `json:"asOfMs" doc:"Unix millis the windows end at."`
		Models []SlotLoadRow `json:"models" doc:"Local models resident, busy, or with traffic in the longest window; busiest first."`
		Total  SlotLoadRow   `json:"total" doc:"Sum over the models: slots, busy, waiting and tok/s total add; occupancy is total slot-seconds held over total slot-seconds available; percentiles pool every arrival."`
	}
}

// liveSpan is an in-flight request reduced to the moments slotAgg reads. Zero
// means "not yet": QueueEndMS 0 is still queued, ServiceStartMS 0 is not yet
// holding a slot.
type liveSpan struct {
	Model          string
	ArrivalMS      int64
	QueueEndMS     int64
	ServiceStartMS int64
	Chunks         int64 // streamed writes so far (≈ tokens); 0 when not streaming
}

// slotAgg accumulates one model's window.
type slotAgg struct {
	busyMS   float64
	tokens   float64
	tgSum    float64
	tgN      int
	arrivals int
	waits    []int64
}

// overlapMS is the length of [a,b) ∩ [from,to), never negative.
func overlapMS(a, b, from, to int64) float64 {
	lo, hi := max(a, from), min(b, to)
	if hi <= lo {
		return 0
	}
	return float64(hi - lo)
}

// aggregateSlotLoad folds finished and in-flight requests into per-model
// accumulators for the window [from, to). Pure, so the window arithmetic is
// testable without a scheduler or a clock.
func aggregateSlotLoad(spans []store.LoadSpan, live []liveSpan, from, to int64) map[string]*slotAgg {
	out := map[string]*slotAgg{}
	agg := func(m string) *slotAgg {
		a := out[m]
		if a == nil {
			a = &slotAgg{}
			out[m] = a
		}
		return a
	}
	for _, s := range spans {
		a := agg(s.Served)
		arrival := s.EndMS - s.DwellMS
		svcStart := min(arrival+s.QueuedMS+s.LoadMS, s.EndMS)
		a.busyMS += overlapMS(svcStart, s.EndMS, from, to)
		// Finished in the window: (from, to], so a request ending exactly at
		// "now" — the window's end — counts.
		finished := s.EndMS > from && s.EndMS <= to

		if s.CompletionTokens > 0 {
			decStart := svcStart
			if s.PredictedPerSec > 0 {
				decStart = max(svcStart, s.EndMS-int64(float64(s.CompletionTokens)/s.PredictedPerSec*1000))
			}
			if s.EndMS > decStart {
				a.tokens += float64(s.CompletionTokens) * overlapMS(decStart, s.EndMS, from, to) / float64(s.EndMS-decStart)
			} else if finished {
				a.tokens += float64(s.CompletionTokens) // no span to spread over: an instant
			}
		}
		if finished && s.PredictedPerSec > 0 && s.CompletionTokens > 0 {
			a.tgSum += s.PredictedPerSec
			a.tgN++
		}
		if arrival >= from && arrival < to {
			a.arrivals++
			a.waits = append(a.waits, s.QueuedMS)
		}
	}
	for _, l := range live {
		a := agg(l.Model)
		if l.ServiceStartMS > 0 {
			a.busyMS += overlapMS(l.ServiceStartMS, to, from, to)
			if l.Chunks > 0 && to > l.ServiceStartMS {
				a.tokens += float64(l.Chunks) * overlapMS(l.ServiceStartMS, to, from, to) / float64(to-l.ServiceStartMS)
			}
		}
		if l.ArrivalMS >= from && l.ArrivalMS < to {
			a.arrivals++
			end := l.QueueEndMS
			if end == 0 {
				end = to // still queued: its wait so far
			}
			a.waits = append(a.waits, max(end-l.ArrivalMS, 0))
		}
	}
	return out
}

// percentileMS is the nearest-rank percentile of xs (p in 0..1); 0 when empty.
func percentileMS(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[min(max(i, 0), len(s)-1)]
}

// windowOfAgg renders an accumulator as the wire shape for a model with slots.
func windowOfAgg(a *slotAgg, minutes, slots int) SlotLoadWindow {
	w := SlotLoadWindow{Minutes: minutes}
	if a == nil {
		return w
	}
	sec := float64(minutes) * 60
	w.TokPerSecTotal = a.tokens / sec
	if a.tgN > 0 {
		w.TokPerSecPerRequest = a.tgSum / float64(a.tgN)
	}
	w.PerRequestSamples = a.tgN
	w.BusySlotsAvg = a.busyMS / 1000 / sec
	if slots > 0 {
		w.OccupancyPct = 100 * w.BusySlotsAvg / float64(slots)
	}
	w.Arrivals = a.arrivals
	w.ArrivalsPerMin = float64(a.arrivals) / float64(minutes)
	for _, x := range a.waits {
		if x > 0 {
			w.Queued++
		}
	}
	w.QueueWaitP50MS = percentileMS(a.waits, 0.5)
	w.QueueWaitP90MS = percentileMS(a.waits, 0.9)
	return w
}

// liveSpansOf turns the proxy's in-flight registry into liveSpans. A request
// still queued has no backend yet, so it is attributed through the name it
// asked for (resolve), its first candidate — the rung it is waiting on first.
func liveSpansOf(in []proxy.InflightInfo, now int64, resolve func(string) string) []liveSpan {
	out := make([]liveSpan, 0, len(in))
	for _, r := range in {
		model := r.Backend
		if model == "" {
			model = resolve(r.Served)
		}
		if model == "" {
			continue
		}
		l := liveSpan{Model: model, ArrivalMS: now - r.ElapsedMS}
		stateStart := now - r.StateMS
		switch r.State {
		case "queued":
		case "loading":
			l.QueueEndMS = stateStart
		default: // streaming: holding a slot; its wait is everything before this state
			l.QueueEndMS = stateStart
			l.ServiceStartMS = stateStart
			if r.Streaming {
				l.Chunks = r.Chunks
			}
		}
		out = append(out, l)
	}
	return out
}

// SlotLoad reports, per local model, aggregate output, slot load and queue
// load: now, and over rolling windows ending now.
func (h *Handlers) SlotLoad(_ context.Context, _ *SlotLoadInput) (*SlotLoadOutput, error) {
	now := time.Now().UnixMilli()
	longest := slotLoadWindows[len(slotLoadWindows)-1]
	from := now - int64(longest)*60_000

	// The log first, the live registry second: a request finishing between the
	// two reads is then missed for a moment rather than counted twice.
	var spans []store.LoadSpan
	if h.Store != nil {
		var err error
		if spans, err = h.Store.LoadSpans(from); err != nil {
			return nil, err
		}
	}
	cfg := h.config()
	resolve := func(name string) string {
		if cands, ok := cfg.ResolveServed(name); ok && len(cands) > 0 {
			return cands[0].Name
		}
		return ""
	}
	var live []liveSpan
	if h.Proxy != nil {
		live = liveSpansOf(h.Proxy.Inflight(), now, resolve)
	}

	// Row set: local models that are resident, busy, or had traffic.
	include := map[string]bool{}
	local := func(name string) bool {
		m, ok := cfg.Models[name]
		return ok && m.LocalProcess()
	}
	for _, s := range spans {
		include[s.Served] = true
	}
	for _, l := range live {
		include[l.Model] = true
	}
	byBackend := map[string]struct{ capacity, active, waiting int }{}
	if h.Sched != nil {
		for _, b := range h.Sched.Snapshot().Backends {
			byBackend[b.Backend] = struct{ capacity, active, waiting int }{b.Capacity, b.Active, b.Waiting}
			if b.Active > 0 || b.Waiting > 0 {
				include[b.Backend] = true
			}
		}
	}
	if h.Mgr != nil {
		for _, m := range h.Mgr.Snapshot().Models {
			if !m.Remote && (m.State == "ready" || m.State == "loading") {
				include[m.ModelName] = true
			}
		}
	}

	aggs := make([]map[string]*slotAgg, len(slotLoadWindows))
	for i, mins := range slotLoadWindows {
		aggs[i] = aggregateSlotLoad(spans, live, now-int64(mins)*60_000, now)
	}

	out := &SlotLoadOutput{}
	out.Body.AsOfMS = now
	total := SlotLoadRow{}
	totalAggs := make([]*slotAgg, len(slotLoadWindows))
	for i := range totalAggs {
		totalAggs[i] = &slotAgg{}
	}
	for name := range include {
		if !local(name) {
			continue
		}
		r := SlotLoadRow{Model: name, Slots: cfg.Models[name].Slots(), SlotsSource: "config"}
		if b, ok := byBackend[name]; ok {
			if b.capacity > 0 {
				r.Slots, r.SlotsSource = b.capacity, "scheduler"
			}
			r.Busy, r.Waiting = b.active, b.waiting
		}
		for i, mins := range slotLoadWindows {
			a := aggs[i][name]
			r.Windows = append(r.Windows, windowOfAgg(a, mins, r.Slots))
			if a != nil {
				t := totalAggs[i]
				t.busyMS += a.busyMS
				t.tokens += a.tokens
				t.tgSum += a.tgSum
				t.tgN += a.tgN
				t.arrivals += a.arrivals
				t.waits = append(t.waits, a.waits...)
			}
		}
		total.Slots += r.Slots
		total.Busy += r.Busy
		total.Waiting += r.Waiting
		out.Body.Models = append(out.Body.Models, r)
	}
	for i, mins := range slotLoadWindows {
		total.Windows = append(total.Windows, windowOfAgg(totalAggs[i], mins, total.Slots))
	}
	out.Body.Total = total
	if out.Body.Models == nil {
		out.Body.Models = []SlotLoadRow{}
	}
	// Busiest first: queued now, busy now, then output over the longest window.
	sort.Slice(out.Body.Models, func(i, j int) bool {
		a, b := out.Body.Models[i], out.Body.Models[j]
		if a.Waiting != b.Waiting {
			return a.Waiting > b.Waiting
		}
		if a.Busy != b.Busy {
			return a.Busy > b.Busy
		}
		ta, tb := a.Windows[len(a.Windows)-1].TokPerSecTotal, b.Windows[len(b.Windows)-1].TokPerSecTotal
		if ta != tb {
			return ta > tb
		}
		return a.Model < b.Model
	})
	return out, nil
}
