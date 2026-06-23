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

// Scheduler owns the per-backend admission state.
type Scheduler struct {
	mu       sync.Mutex
	backends map[string]*backendState
}

// New constructs a Scheduler.
func New() *Scheduler {
	return &Scheduler{backends: map[string]*backendState{}}
}

type backendState struct {
	capacity    int
	active      int            // slots in use
	groupActive map[string]int // in-flight slots per group (fairness numerator)
	slots       []*slot        // active slots, for victim selection
	waiters     []*waiter      // queued, picked by preempt-priority then weighted fairshare
}

// slot is one in-flight admission. cancel reclaims the request when the slot is
// preempted; preempting marks a slot already chosen as a victim so concurrent
// preemptors don't double-target it.
type slot struct {
	group         string
	weight        int
	interruptible bool
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
func (s *Scheduler) Admit(ctx context.Context, backend string, capacity int, group string, weight int, interruptible bool, stage config.Stage) (release func(), reqCtx context.Context, err error) {
	if weight < 1 {
		weight = 1
	}
	reqCtx, cancel := context.WithCancelCause(ctx)

	s.mu.Lock()
	bs := s.backends[backend]
	if bs == nil {
		bs = &backendState{capacity: capacity, groupActive: map[string]int{}}
		s.backends[backend] = bs
	}
	// Capacity can only grow within a process (config reload may raise it).
	if capacity > bs.capacity {
		bs.capacity = capacity
	}

	sl := &slot{group: group, weight: weight, interruptible: interruptible, cancel: cancel}

	if bs.active < bs.capacity {
		bs.grant(sl)
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

	w := &waiter{slot: sl, ready: make(chan struct{})}
	bs.waiters = append(bs.waiters, w)
	s.mu.Unlock()
	return s.wait(ctx, backend, w, reqCtx)
}

// wait blocks until the waiter is granted a slot or ctx ends.
func (s *Scheduler) wait(ctx context.Context, backend string, w *waiter, reqCtx context.Context) (func(), context.Context, error) {
	select {
	case <-w.ready:
		return s.releaser(backend, w.slot), reqCtx, nil
	case <-ctx.Done():
		// Drop out of the queue. If we were granted concurrently with the cancel,
		// hand the slot back so it isn't lost.
		s.mu.Lock()
		bs := s.backends[backend]
		if bs != nil && bs.removeWaiter(w) {
			be := bs.backpressure("queue-timeout")
			s.mu.Unlock()
			w.slot.cancel(nil)
			return nil, nil, be
		}
		s.mu.Unlock()
		s.releaser(backend, w.slot)() // already granted: release the slot we never used
		return nil, nil, &BackpressureError{Reason: "queue-timeout", RetryAfter: time.Second}
	}
}

// releaser returns a one-shot release for a held slot.
func (s *Scheduler) releaser(backend string, sl *slot) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			bs := s.backends[backend]
			if bs == nil {
				s.mu.Unlock()
				return
			}
			bs.active--
			bs.groupActive[sl.group]--
			if bs.groupActive[sl.group] <= 0 {
				delete(bs.groupActive, sl.group)
			}
			bs.removeSlot(sl)
			bs.promote()
			s.mu.Unlock()
			sl.cancel(nil) // free the request context (no-op if already canceled)
		})
	}
}

// grant claims a slot (caller holds s.mu).
func (bs *backendState) grant(sl *slot) {
	bs.active++
	bs.groupActive[sl.group]++
	bs.slots = append(bs.slots, sl)
}

// promote fills free slots from the queue: preempt waiters first (they each
// freed a slot to get here), then the most under-served group by weighted
// fairshare (min active/weight) — a weight-10 group holds ~10× a weight-1 group's
// slots under sustained contention.
func (bs *backendState) promote() {
	for bs.active < bs.capacity && len(bs.waiters) > 0 {
		idx := bs.pickWaiter()
		w := bs.waiters[idx]
		bs.waiters = append(bs.waiters[:idx], bs.waiters[idx+1:]...)
		bs.grant(w.slot)
		close(w.ready)
	}
}

// pickWaiter chooses the next waiter to grant: preempt waiters in FIFO order
// jump ahead of fairshare waiters. Caller holds s.mu.
func (bs *backendState) pickWaiter() int {
	for i, w := range bs.waiters {
		if w.preempt {
			return i
		}
	}
	best, bestIdx := math.Inf(1), 0
	for i, w := range bs.waiters {
		ratio := float64(bs.groupActive[w.slot.group]) / float64(w.slot.weight)
		if ratio < best {
			best, bestIdx = ratio, i
		}
	}
	return bestIdx
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

// backpressure snapshots the current pressure into an error (caller holds s.mu).
func (bs *backendState) backpressure(reason string) *BackpressureError {
	waiting := len(bs.waiters)
	// Heuristic Retry-After: roughly how many "rounds" ahead this caller is,
	// floored at 1s. Refined once dwell metering lands (P6).
	retry := time.Duration(math.Ceil(float64(waiting+1)/float64(maxInt(bs.capacity, 1)))) * time.Second
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
