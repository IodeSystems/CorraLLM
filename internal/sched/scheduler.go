// Package sched is corrallm's admission controller. Each backend has a fixed
// number of slots (its concurrency capacity); the scheduler arbitrates those
// slots across priority groups by weighted fairshare, queues or rejects when
// saturated, and always emits informative backoff.
//
// P2 scope: request-count fairshare over per-backend slots, queue + reject
// stages. P5 adds preemption: a higher group whose stage allows `preempt` may
// cooperatively cancel an in-flight slot held by a lower, interruptible group.
// Spill/fall-through (P3) is another exit the request pipeline applies around
// this controller. Default ordering when a stage permits both preempt and spill:
// preempt first, spill (`then`) only if there is no eligible victim.
package sched

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// ErrPreempted is the cancellation cause set on a request context when its slot
// is reclaimed by a higher-priority group. The proxy distinguishes it from a
// client cancel for logging.
var ErrPreempted = errors.New("preempted by higher-priority group")

// BackpressureError is returned when a request cannot be admitted. It carries
// the structured backoff the edge turns into 429 + Retry-After + headers.
type BackpressureError struct {
	Reason     string        // "rejected" | "queue-timeout" | "spill" | "exhausted"
	RetryAfter time.Duration // suggested wait
	Capacity   int           // backend slots
	InFlight   int           // slots currently in use
	Waiting    int           // queued requests
}

func (e *BackpressureError) Error() string {
	return fmt.Sprintf("backpressure: %s (capacity=%d inflight=%d waiting=%d retry_after=%s)",
		e.Reason, e.Capacity, e.InFlight, e.Waiting, e.RetryAfter)
}

// Done reports a finished request's measured cost so the scheduler can charge it
// against the group's limit budgets (and, with a cost share-currency, its
// fairshare accumulator). Dwell is measured internally from the admit timestamp.
// It is passed to the release func; an unmetered release (e.g. a spill before
// serving) passes none.
type Done struct {
	CostUSD float64
}

// Scheduler owns the per-backend admission state. With a config it also enforces
// per-group / per-(group×type) limit budgets over a sliding window; without one
// it is pure request-count fairshare (the P2 behavior).
type Scheduler struct {
	mu       sync.Mutex
	backends map[string]*backendState
	cfg      *config.Config         // optional: drives limits + share currency
	budgets  map[string][]rateEvent // "scope\x00dim" → sliding-window events
	now      func() time.Time       // injectable clock (windows, dwell, decay)

	maxWait       time.Duration // queue wait before a 429 (0 = bounded only by req ctx)
	maxQueueDepth int           // reject once this many already wait on a backend (0 = unbounded)

	// reservations holds short, renewable leases that keep slots free for a lane —
	// backend → lane → lease. A request from lane G sees effective capacity
	// capacity − Σ(slots reserved by lanes ≠ G), so batch backs off and the reserving
	// (interactive) lane gets an already-free slot without preempting.
	reservations map[string]map[string]*reservation
	maxResTTL    time.Duration // cap on a reservation lease (0 = default 5m)
}

// ewmaAlpha weights the newest service-time sample in the per-backend dwell EWMA
// that drives Retry-After. Higher = more reactive; 0.3 tracks recent load while
// damping single-request spikes.
const ewmaAlpha = 0.3

// rateEvent is one consumption against a limit budget at a point in time.
type rateEvent struct {
	at  time.Time
	amt float64
}

// New constructs a Scheduler with no config: request-count fairshare, no limits.
func New() *Scheduler {
	return &Scheduler{
		backends:     map[string]*backendState{},
		budgets:      map[string][]rateEvent{},
		reservations: map[string]map[string]*reservation{},
		now:          time.Now,
	}
}

// NewWithConfig constructs a Scheduler that also enforces the config's limit
// budgets, honors each group's share currency, and applies the queue bounds
// (maxWait / maxQueueDepth) from the scheduler config.
func NewWithConfig(cfg *config.Config) *Scheduler {
	s := New()
	s.cfg = cfg
	if cfg != nil {
		if d, err := time.ParseDuration(cfg.Scheduler.MaxWait); err == nil && d > 0 {
			s.maxWait = d
		}
		if cfg.Scheduler.MaxQueueDepth > 0 {
			s.maxQueueDepth = cfg.Scheduler.MaxQueueDepth
		}
	}
	return s
}

type backendState struct {
	capacity    int
	active      int            // slots in use
	groupActive map[string]int // in-flight slots per group (request-count numerator)
	slots       []*slot        // active slots, for victim selection
	waiters     []*waiter      // queued, picked by preempt-priority then weighted fairshare
	// share is a per-group decaying accumulator of the group's recent dwell/cost
	// consumption — the fairshare numerator under a dwell|cost share currency.
	share   map[string]float64
	shareAt map[string]time.Time // last update, for exponential decay
	// ewmaDwell is the EWMA of measured service time (seconds) on this backend —
	// the basis for an honest Retry-After. 0 until the first request completes.
	ewmaDwell float64
}

// slot is one in-flight admission. cancel reclaims the request when the slot is
// preempted; preempting marks a slot already chosen as a victim so concurrent
// preemptors don't double-target it.
type slot struct {
	group         string
	backendType   string
	weight        int
	interruptible bool
	currency      string // fairshare currency: requests (default) | dwell | cost
	admitAt       time.Time
	cancel        context.CancelCauseFunc
	preempting    bool
}

type waiter struct {
	slot    *slot
	preempt bool          // jumps the queue (it freed a slot by preempting)
	ready   chan struct{} // signaled when this waiter is granted a slot
}

// Admit acquires a slot on the named backend for a request in group (with
// weight, interruptible), honoring stage when the backend is saturated. On
// success it returns a release func that MUST be called when the request
// finishes, plus a request context that is canceled (cause ErrPreempted) if the
// slot is later preempted — the caller proxies under it so a preemption aborts
// the upstream stream. On saturation it returns a *BackpressureError.
func (s *Scheduler) Admit(ctx context.Context, backend, backendType string, capacity int, group string, weight int, interruptible bool, stage config.Stage) (release func(...Done), reqCtx context.Context, err error) {
	if weight < 1 {
		weight = 1
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	now := s.now()

	s.mu.Lock()
	bs := s.backends[backend]
	if bs == nil {
		bs = &backendState{capacity: capacity, groupActive: map[string]int{},
			share: map[string]float64{}, shareAt: map[string]time.Time{}}
		s.backends[backend] = bs
	}
	// Capacity can only grow within a process (config reload may raise it).
	if capacity > bs.capacity {
		bs.capacity = capacity
	}

	sl := &slot{group: group, backendType: backendType, weight: weight,
		interruptible: interruptible, currency: s.shareCurrency(group), cancel: cancel}

	// Over a per-group / per-(group×type) limit budget? Preemption can't free a
	// budget, so an over-budget request advances (spill) if the stage allows,
	// else backs off with the time until the window frees. (Capacity saturation
	// below keeps the full preempt/queue/reject sequence.)
	if over, retry := s.overBudgetLocked(now, group, backendType); over {
		if spills(stage) {
			be := bs.backpressure("spill")
			s.mu.Unlock()
			cancel(nil)
			return nil, nil, be
		}
		be := bs.backpressure("over-budget")
		be.RetryAfter = retry
		s.mu.Unlock()
		cancel(nil)
		return nil, nil, be
	}

	// Effective capacity for this lane = physical slots minus what OTHER lanes have
	// reserved. The reserving lane sees full capacity (its own reservation doesn't
	// count against it), so it gets an already-free slot; other lanes saturate early.
	if bs.active < s.effCapLocked(bs, backend, group, now) {
		bs.grant(sl, now)
		s.recordAdmitLocked(now, group, backendType)
		s.mu.Unlock()
		return s.releaser(backend, sl), reqCtx, nil
	}

	// Saturated. Preempt takes precedence over spill: if the stage allows it and
	// an eligible victim exists, cancel the victim and queue ahead of fairshare to
	// receive the slot it frees. With no victim, fall back to `then`/queue/reject.
	if stage.Preempt {
		if victim := bs.pickVictim(weight); victim != nil {
			victim.cancel(ErrPreempted)
			w := &waiter{slot: sl, preempt: true, ready: make(chan struct{})}
			bs.waiters = append(bs.waiters, w)
			s.mu.Unlock()
			return s.wait(ctx, backend, w, reqCtx)
		}
		// No victim — honor the follow-up verb, else queue/reject below.
		switch {
		case spills(stage):
			be := bs.backpressure("spill")
			s.mu.Unlock()
			cancel(nil)
			return nil, nil, be
		case stage.Queue || stage.Then == "queue":
			// fall through to the wait path below
		default:
			be := bs.backpressure("rejected")
			s.mu.Unlock()
			cancel(nil)
			return nil, nil, be
		}
	} else {
		// No preempt: the existing P2/P3 exits.
		switch {
		case stage.Queue:
			// fall through to the wait path below
		case stage.Spill || stage.FallThrough:
			be := bs.backpressure("spill")
			s.mu.Unlock()
			cancel(nil)
			return nil, nil, be
		default:
			be := bs.backpressure("rejected")
			s.mu.Unlock()
			cancel(nil)
			return nil, nil, be
		}
	}

	// Bound the queue: once maxQueueDepth callers already wait, reject fast with an
	// informative 429 so the caller can shape, rather than block (the fork's
	// maxQueueDepth contract). Preempt waiters above bypass this — they freed a slot.
	if s.maxQueueDepth > 0 && len(bs.waiters) >= s.maxQueueDepth {
		be := bs.backpressure("rejected")
		s.mu.Unlock()
		cancel(nil)
		return nil, nil, be
	}

	w := &waiter{slot: sl, ready: make(chan struct{})}
	bs.waiters = append(bs.waiters, w)
	s.mu.Unlock()
	return s.wait(ctx, backend, w, reqCtx)
}

// wait blocks until the waiter is granted a slot, maxWait elapses, or ctx ends.
func (s *Scheduler) wait(ctx context.Context, backend string, w *waiter, reqCtx context.Context) (func(...Done), context.Context, error) {
	// maxWait caps the queue wait so the caller gets a 429 to shape against rather
	// than blocking up to the request deadline (the fork's maxWait contract).
	var maxWaitC <-chan time.Time
	if s.maxWait > 0 {
		t := time.NewTimer(s.maxWait)
		defer t.Stop()
		maxWaitC = t.C
	}
	select {
	case <-w.ready:
		return s.releaser(backend, w.slot), reqCtx, nil
	case <-maxWaitC:
		return s.giveUp(backend, w, "queue-timeout")
	case <-ctx.Done():
		return s.giveUp(backend, w, "queue-timeout")
	}
}

// giveUp drops a waiter from the queue and returns informative backpressure. If
// the waiter was granted a slot concurrently with the give-up, the slot is
// handed back so it isn't lost.
func (s *Scheduler) giveUp(backend string, w *waiter, reason string) (func(...Done), context.Context, error) {
	s.mu.Lock()
	bs := s.backends[backend]
	if bs != nil && bs.removeWaiter(w) {
		be := bs.backpressure(reason)
		s.mu.Unlock()
		w.slot.cancel(nil)
		return nil, nil, be
	}
	s.mu.Unlock()
	s.releaser(backend, w.slot)() // already granted: release the slot we never used
	return nil, nil, &BackpressureError{Reason: reason, RetryAfter: time.Second}
}

// releaser returns a one-shot release for a held slot. The optional Done carries
// the request's measured cost, charged against limit budgets (and the cost share
// accumulator); dwell is measured from the slot's admit timestamp.
// --- live-load introspection (P8) ---

// GroupLoad is one group's in-flight + queued count on a backend.
type GroupLoad struct {
	Group   string
	Active  int
	Waiting int
}

// BackendLoad is a backend's live admission load with a per-group breakdown.
type BackendLoad struct {
	Backend  string
	Capacity int
	Active   int
	Waiting  int
	Groups   []GroupLoad
}

// SchedSnapshot is the live admission state across all touched backends.
type SchedSnapshot struct {
	Backends []BackendLoad
}

// Snapshot returns a stable (sorted) view of per-backend admission load and the
// per-group active/waiting breakdown — the read surface behind the P8 lanes view.
// Only backends that have seen traffic appear (state is created lazily on Admit).
func (s *Scheduler) Snapshot() SchedSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap SchedSnapshot
	for name, bs := range s.backends {
		bl := BackendLoad{Backend: name, Capacity: bs.capacity, Active: bs.active, Waiting: len(bs.waiters)}
		waitByGroup := map[string]int{}
		for _, w := range bs.waiters {
			waitByGroup[w.slot.group]++
		}
		groups := map[string]struct{}{}
		for g := range bs.groupActive {
			groups[g] = struct{}{}
		}
		for g := range waitByGroup {
			groups[g] = struct{}{}
		}
		for g := range groups {
			bl.Groups = append(bl.Groups, GroupLoad{Group: g, Active: bs.groupActive[g], Waiting: waitByGroup[g]})
		}
		sort.Slice(bl.Groups, func(i, j int) bool { return bl.Groups[i].Group < bl.Groups[j].Group })
		snap.Backends = append(snap.Backends, bl)
	}
	sort.Slice(snap.Backends, func(i, j int) bool { return snap.Backends[i].Backend < snap.Backends[j].Backend })
	return snap
}

// ShareCurrency reports the configured fairshare currency for a group
// (requests|dwell|cost), defaulting to requests. Exposed for the UI.
func (s *Scheduler) ShareCurrency(group string) string { return s.shareCurrency(group) }

func (s *Scheduler) releaser(backend string, sl *slot) func(...Done) {
	var once sync.Once
	return func(d ...Done) {
		once.Do(func() {
			var cost float64
			if len(d) > 0 {
				cost = d[0].CostUSD
			}
			s.mu.Lock()
			bs := s.backends[backend]
			if bs == nil {
				s.mu.Unlock()
				return
			}
			now := s.now()
			dwell := now.Sub(sl.admitAt).Seconds()
			bs.observeDwell(dwell)
			bs.active--
			bs.groupActive[sl.group]--
			if bs.groupActive[sl.group] <= 0 {
				delete(bs.groupActive, sl.group)
			}
			bs.removeSlot(sl)
			s.recordReleaseLocked(now, sl.group, sl.backendType, dwell, cost)
			bs.addShare(now, sl, dwell, cost)
			// Charge each promoted (queued) request against its requests budget —
			// the direct-grant path charges at admit, the queue path here.
			for _, ps := range s.promote(bs, backend, now) {
				s.recordAdmitLocked(now, ps.group, ps.backendType)
			}
			s.mu.Unlock()
			sl.cancel(nil) // free the request context (no-op if already canceled)
		})
	}
}

// grant claims a slot (caller holds s.mu).
func (bs *backendState) grant(sl *slot, now time.Time) {
	sl.admitAt = now
	bs.active++
	bs.groupActive[sl.group]++
	bs.slots = append(bs.slots, sl)
}

// promote fills free slots from the queue: preempt waiters first (they each
// freed a slot to get here), then the most under-served group by weighted
// fairshare (min active/weight) — a weight-10 group holds ~10× a weight-1 group's
// slots under sustained contention. Reservation-aware: a non-preempt waiter is
// only granted a slot its lane may use (not one reserved by another lane), so
// freed batch slots don't refill a reserved segment.
func (s *Scheduler) promote(bs *backendState, backend string, now time.Time) []*slot {
	var granted []*slot
	for len(bs.waiters) > 0 {
		idx := s.pickGrantableWaiter(bs, backend, now)
		if idx < 0 {
			break // remaining waiters are all reserved out of the free slots
		}
		w := bs.waiters[idx]
		bs.waiters = append(bs.waiters[:idx], bs.waiters[idx+1:]...)
		bs.grant(w.slot, now)
		granted = append(granted, w.slot)
		close(w.ready)
	}
	return granted
}

// pickGrantableWaiter chooses the next waiter that can actually take a free slot,
// or -1 if none. Preempt waiters go first in FIFO order — each already freed a
// slot by preempting, so it REPLACES (net active unchanged) and is bounded by
// physical capacity, not the reservation. Otherwise the most under-served group by
// min(numerator/weight) wins, but only among lanes with room under their effective
// capacity (a lane can't fill slots reserved by another lane). Caller holds s.mu.
func (s *Scheduler) pickGrantableWaiter(bs *backendState, backend string, now time.Time) int {
	for i, w := range bs.waiters {
		if w.preempt && bs.active < bs.capacity {
			return i
		}
	}
	currency := bs.queueCurrency()
	best, bestIdx := math.Inf(1), -1
	for i, w := range bs.waiters {
		if w.preempt || bs.active >= s.effCapLocked(bs, backend, w.slot.group, now) {
			continue
		}
		ratio := bs.numerator(now, currency, w.slot.group) / float64(w.slot.weight)
		if ratio < best {
			best, bestIdx = ratio, i
		}
	}
	return bestIdx
}

// pickWaiter is the reservation-agnostic fairshare pick (preempt FIFO, then min
// numerator/weight) over ALL waiters — the pure priority logic, exercised
// directly by tests. promote uses pickGrantableWaiter, which layers the
// per-lane effective-capacity filter on top. Caller holds s.mu.
func (bs *backendState) pickWaiter(now time.Time) int {
	for i, w := range bs.waiters {
		if w.preempt {
			return i
		}
	}
	currency := bs.queueCurrency()
	best, bestIdx := math.Inf(1), 0
	for i, w := range bs.waiters {
		ratio := bs.numerator(now, currency, w.slot.group) / float64(w.slot.weight)
		if ratio < best {
			best, bestIdx = ratio, i
		}
	}
	return bestIdx
}

// queueCurrency is the currency to compare waiters in: the shared dwell|cost
// currency if every waiter agrees, else requests. Caller holds s.mu.
func (bs *backendState) queueCurrency() string {
	c := ""
	for _, w := range bs.waiters {
		switch w.slot.currency {
		case "dwell", "cost":
			if c == "" {
				c = w.slot.currency
			} else if c != w.slot.currency {
				return "requests"
			}
		default:
			return "requests"
		}
	}
	if c == "" {
		return "requests"
	}
	return c
}

// numerator is a group's fairshare load in the given (queue-wide) currency.
// Caller holds s.mu.
func (bs *backendState) numerator(now time.Time, currency, group string) float64 {
	switch currency {
	case "dwell", "cost":
		return bs.decayedShare(now, group)
	default: // requests
		return float64(bs.groupActive[group])
	}
}

// pickVictim selects an in-flight slot to preempt for a preemptor of the given
// weight: an interruptible slot of a strictly lower-weight group, preferring the
// lowest weight, skipping slots already targeted. It marks the chosen slot so a
// concurrent preemptor won't reuse it. Caller holds s.mu.
func (bs *backendState) pickVictim(preemptorWeight int) *slot {
	var best *slot
	for _, sl := range bs.slots {
		if sl.preempting || !sl.interruptible || sl.weight >= preemptorWeight {
			continue
		}
		if best == nil || sl.weight < best.weight {
			best = sl
		}
	}
	if best != nil {
		best.preempting = true
	}
	return best
}

// removeWaiter drops w from the queue; returns false if it was already granted
// (no longer in the queue). Caller holds s.mu.
func (bs *backendState) removeWaiter(w *waiter) bool {
	for i, x := range bs.waiters {
		if x == w {
			bs.waiters = append(bs.waiters[:i], bs.waiters[i+1:]...)
			return true
		}
	}
	return false
}

// removeSlot drops sl from the active set. Caller holds s.mu.
func (bs *backendState) removeSlot(sl *slot) {
	for i, x := range bs.slots {
		if x == sl {
			bs.slots = append(bs.slots[:i], bs.slots[i+1:]...)
			return
		}
	}
}

// observeDwell folds a finished request's service time into the backend's dwell
// EWMA (caller holds s.mu).
func (bs *backendState) observeDwell(dwell float64) {
	if dwell < 0 {
		dwell = 0
	}
	if bs.ewmaDwell == 0 {
		bs.ewmaDwell = dwell // seed with the first sample
		return
	}
	bs.ewmaDwell = ewmaAlpha*dwell + (1-ewmaAlpha)*bs.ewmaDwell
}

// backpressure snapshots the current pressure into an error (caller holds s.mu).
// Retry-After estimates the wait as the caller's queue position in "rounds"
// (ceil((waiting+1)/capacity)) times the measured per-request service time
// (dwell EWMA) — an honest hint. Before any request completes (no EWMA yet) it
// falls back to ~1s per round.
func (bs *backendState) backpressure(reason string) *BackpressureError {
	waiting := len(bs.waiters)
	rounds := math.Ceil(float64(waiting+1) / float64(maxInt(bs.capacity, 1)))
	per := bs.ewmaDwell
	if per <= 0 {
		per = 1 // no service-time sample yet
	}
	retry := time.Duration(rounds * per * float64(time.Second))
	if retry < time.Second {
		retry = time.Second
	}
	return &BackpressureError{
		Reason:     reason,
		RetryAfter: retry,
		Capacity:   bs.capacity,
		InFlight:   bs.active,
		Waiting:    waiting,
	}
}

// spills reports whether a stage's follow-up advances to the next backend.
func spills(stage config.Stage) bool {
	return stage.Spill || stage.FallThrough || stage.Then == "fallThrough" || stage.Then == "spill"
}

// shareHalfLife is the decay constant for the dwell/cost fairshare accumulator:
// a group's recent consumption halves every interval, so it is forgiven over
// time rather than penalized forever.
const shareHalfLife = 30 * time.Second

// shareCurrency returns a group's fairshare currency (requests | dwell | cost),
// defaulting to requests.
func (s *Scheduler) shareCurrency(group string) string {
	if s.cfg == nil {
		return "requests"
	}
	switch c := s.cfg.PriorityGroups[group].ShareCurrency; c {
	case "dwell", "cost":
		return c
	default:
		return "requests"
	}
}

// decayedShare returns a group's share accumulator decayed to now. Caller holds s.mu.
func (bs *backendState) decayedShare(now time.Time, group string) float64 {
	v := bs.share[group]
	if v == 0 {
		return 0
	}
	elapsed := now.Sub(bs.shareAt[group]).Seconds()
	return v * math.Pow(0.5, elapsed/shareHalfLife.Seconds())
}

// addShare folds a finished request's dwell/cost into its group's accumulator
// (no-op under the requests currency). Caller holds s.mu.
func (bs *backendState) addShare(now time.Time, sl *slot, dwell, cost float64) {
	var amt float64
	switch sl.currency {
	case "dwell":
		amt = dwell
	case "cost":
		amt = cost
	default:
		return
	}
	bs.share[sl.group] = bs.decayedShare(now, sl.group) + amt
	bs.shareAt[sl.group] = now
}

// limitsFor returns the per-group and per-(group×type) limit specs. Caller holds s.mu.
func (s *Scheduler) limitsFor(group, backendType string) (groupLimits, stageLimits map[string]string) {
	if s.cfg == nil {
		return nil, nil
	}
	g := s.cfg.PriorityGroups[group]
	return g.Limits, g.StageFor(backendType).Limits
}

// overBudgetLocked reports whether group is over any per-group or per-(group×type)
// limit budget, and the suggested wait until the binding window frees. Caller
// holds s.mu.
func (s *Scheduler) overBudgetLocked(now time.Time, group, backendType string) (bool, time.Duration) {
	gl, sl := s.limitsFor(group, backendType)
	if over, retry := s.overScopeLocked(now, group+"\x00", gl); over {
		return true, retry
	}
	return s.overScopeLocked(now, group+"\x00"+backendType+"\x00", sl)
}

func (s *Scheduler) overScopeLocked(now time.Time, prefix string, limits map[string]string) (bool, time.Duration) {
	over := false
	var retry time.Duration
	// Check every dimension (map order is nondeterministic) and report the
	// longest wait — the request can't proceed until the bindingest window frees.
	for dim, spec := range limits {
		r, err := config.ParseRate(dim, spec)
		if err != nil {
			continue // malformed specs are ignored, not fatal
		}
		sum, oldest := s.windowLocked(now, prefix+dim, r.Window)
		if sum >= r.Amount {
			over = true
			if d := r.Window - now.Sub(oldest); d > retry {
				retry = d
			}
		}
	}
	if over && retry < time.Second {
		retry = time.Second
	}
	return over, retry
}

// recordAdmitLocked charges one request against any configured requests budget.
func (s *Scheduler) recordAdmitLocked(now time.Time, group, backendType string) {
	gl, sl := s.limitsFor(group, backendType)
	if _, ok := gl["requests"]; ok {
		s.chargeLocked(now, group+"\x00requests", 1)
	}
	if _, ok := sl["requests"]; ok {
		s.chargeLocked(now, group+"\x00"+backendType+"\x00requests", 1)
	}
}

// recordReleaseLocked charges a finished request's dwell + cost against any
// configured dwell/cost budgets.
func (s *Scheduler) recordReleaseLocked(now time.Time, group, backendType string, dwell, cost float64) {
	gl, sl := s.limitsFor(group, backendType)
	for _, e := range []struct {
		dim string
		amt float64
	}{{"dwell", dwell}, {"cost", cost}} {
		if _, ok := gl[e.dim]; ok {
			s.chargeLocked(now, group+"\x00"+e.dim, e.amt)
		}
		if _, ok := sl[e.dim]; ok {
			s.chargeLocked(now, group+"\x00"+backendType+"\x00"+e.dim, e.amt)
		}
	}
}

// chargeLocked appends a consumption event. Caller holds s.mu.
func (s *Scheduler) chargeLocked(now time.Time, key string, amt float64) {
	s.budgets[key] = append(s.budgets[key], rateEvent{at: now, amt: amt})
}

// windowLocked prunes events outside the trailing window and returns the
// surviving sum and the oldest surviving timestamp. Caller holds s.mu.
func (s *Scheduler) windowLocked(now time.Time, key string, window time.Duration) (sum float64, oldest time.Time) {
	cutoff := now.Add(-window)
	ev := s.budgets[key]
	kept := ev[:0]
	for _, e := range ev {
		if e.at.After(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(s.budgets, key)
		return 0, now
	}
	s.budgets[key] = kept
	oldest = kept[0].at
	for _, e := range kept {
		sum += e.amt
		if e.at.Before(oldest) {
			oldest = e.at
		}
	}
	return sum, oldest
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
