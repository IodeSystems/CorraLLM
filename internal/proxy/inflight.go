package proxy

import (
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/events"
)

// In-flight request registry.
//
// The activity log only exists once a request FINISHES — which is exactly when
// you stop caring. A long chat completion, a model cold-load, or a request stuck
// behind a saturated backend is invisible until it is over. This registry is the
// live half: every request in the proxy is registered before it can block on
// anything and removed on every exit path, so the dashboard can show what the
// box is doing RIGHT NOW.
//
// Lifecycle states mirror where the request is actually blocked:
//
//	queued    → waiting on a scheduler slot (admission)
//	loading   → holds a slot, waiting on the backend to become ready (cold load)
//	streaming → proxying to/from the backend
//
// Entries are pointers into a map guarded by one mutex; each mutation publishes
// a "changed" ping so SSE subscribers refetch. States change a handful of times
// per request, not per token, so the ping rate is bounded by request rate.

// inflightState values (see above).
const (
	inflightQueued    = "queued"
	inflightLoading   = "loading"
	inflightStreaming = "streaming"
)

// inflightEntry is one live request. Fields are written only through the Proxy's
// registry methods (which hold the lock); read only via Inflight().
type inflightEntry struct {
	id        int64
	served    string
	backend   string
	group     string
	key       string
	sourceIP  string
	path      string
	streaming bool
	state     string
	startedAt time.Time
}

// InflightInfo is a snapshot of one live request, safe to hand to the API layer.
type InflightInfo struct {
	ID        int64
	Served    string
	Backend   string // "" until a candidate is admitted
	Group     string
	Key       string
	SourceIP  string
	Path      string
	Streaming bool // client asked for a streamed response
	State     string
	StartedAt time.Time
	ElapsedMS int64
}

// beginInflight registers a request as live and returns its entry. The caller
// MUST defer endInflight. Registration happens before admission, so a request
// queued behind a saturated backend is visible while it waits.
func (p *Proxy) beginInflight(r *http.Request, served, group, key string, streaming bool) *inflightEntry {
	e := &inflightEntry{
		id:        atomic.AddInt64(&p.inflightSeq, 1),
		served:    served,
		group:     group,
		key:       key,
		sourceIP:  clientIP(r),
		path:      r.URL.Path,
		streaming: streaming,
		state:     inflightQueued,
		startedAt: time.Now(),
	}
	p.inflightMu.Lock()
	if p.inflight == nil {
		p.inflight = map[int64]*inflightEntry{}
	}
	p.inflight[e.id] = e
	p.inflightMu.Unlock()
	p.publish(events.Event{Type: "changed"})
	return e
}

// markInflight moves an entry to a new state on a named backend. An empty
// backend leaves the current one (a spill back to "queued" keeps the last
// backend tried out of the row rather than showing a stale one).
func (p *Proxy) markInflight(e *inflightEntry, state, backend string) {
	if e == nil {
		return
	}
	p.inflightMu.Lock()
	e.state = state
	e.backend = backend
	p.inflightMu.Unlock()
	p.publish(events.Event{Type: "changed"})
}

// endInflight removes a finished request. Safe to call more than once.
func (p *Proxy) endInflight(e *inflightEntry) {
	if e == nil {
		return
	}
	p.inflightMu.Lock()
	_, live := p.inflight[e.id]
	delete(p.inflight, e.id)
	p.inflightMu.Unlock()
	if live {
		p.publish(events.Event{Type: "changed"})
	}
}

// Inflight returns every live request, oldest first — the one that has been
// running longest is the one worth looking at.
func (p *Proxy) Inflight() []InflightInfo {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.inflightMu.Lock()
	out := make([]InflightInfo, 0, len(p.inflight))
	for _, e := range p.inflight {
		out = append(out, InflightInfo{
			ID: e.id, Served: e.served, Backend: e.backend, Group: e.group,
			Key: e.key, SourceIP: e.sourceIP, Path: e.path, Streaming: e.streaming,
			State: e.state, StartedAt: e.startedAt,
			ElapsedMS: now.Sub(e.startedAt).Milliseconds(),
		})
	}
	p.inflightMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}
