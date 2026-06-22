// Package sched is corrallm's admission controller. Each backend has a fixed
// number of slots (its concurrency capacity); the scheduler arbitrates those
// slots across priority groups by weighted fairshare, queues or rejects when
// saturated, and always emits informative backoff.
//
// P2 scope: request-count fairshare over per-backend slots, queue + reject
// stages. Preempt (P5) and spill/fall-through (P3) are other exits the request
// pipeline applies around this controller; here a request either gets a slot,
// waits for one, or is rejected.
package sched

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// BackpressureError is returned when a request cannot be admitted. It carries
// the structured backoff the edge turns into 429 + Retry-After + headers.
type BackpressureError struct {
	Reason     string        // "rejected" | "queue-timeout"
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
	waiters     []*waiter      // queued, picked by weighted fairshare
}

type waiter struct {
	group  string
	weight int
	ready  chan struct{} // signaled when this waiter is granted a slot
}

// Admit acquires a slot on the named backend for a request in group (with
// weight), honoring stage when the backend is saturated. On success it returns a
// release func that MUST be called when the request finishes. On saturation it
// returns a *BackpressureError (after waiting, if the stage queues).
func (s *Scheduler) Admit(ctx context.Context, backend string, capacity int, group string, weight int, stage config.Stage) (release func(), err error) {
	if weight < 1 {
		weight = 1
	}
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

	if bs.active < bs.capacity {
		bs.grant(group)
		s.mu.Unlock()
		return s.releaser(backend, group), nil
	}

	// Saturated — the stage decides. Queue waits; spill/fallThrough advances to
	// the next backend (the caller treats Reason "spill" as non-terminal); reject
	// (and any undeclared stage) is terminal.
	switch {
	case stage.Queue:
		// fall through to the wait path below
	case stage.Spill || stage.FallThrough:
		be := bs.backpressure("spill")
		s.mu.Unlock()
		return nil, be
	default:
		be := bs.backpressure("rejected")
		s.mu.Unlock()
		return nil, be
	}

	w := &waiter{group: group, weight: weight, ready: make(chan struct{})}
	bs.waiters = append(bs.waiters, w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		return s.releaser(backend, group), nil
	case <-ctx.Done():
		// Drop out of the queue. If we were granted concurrently with the
		// cancel, hand the slot back so it isn't lost.
		s.mu.Lock()
		if bs.removeWaiter(w) {
			be := bs.backpressure("queue-timeout")
			s.mu.Unlock()
			return nil, be
		}
		// Already granted: release the slot we never used.
		s.mu.Unlock()
		s.releaser(backend, group)()
		return nil, &BackpressureError{Reason: "queue-timeout", RetryAfter: time.Second}
	}
}

// releaser returns a one-shot release for a held slot.
func (s *Scheduler) releaser(backend, group string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			bs := s.backends[backend]
			if bs == nil {
				return
			}
			bs.active--
			bs.groupActive[group]--
			if bs.groupActive[group] <= 0 {
				delete(bs.groupActive, group)
			}
			bs.promote()
		})
	}
}

// grant claims a slot for group (caller holds s.mu).
func (bs *backendState) grant(group string) {
	bs.active++
	bs.groupActive[group]++
}

// promote grants the freed slot to the most under-served waiting group, by
// min (active/weight). This is the weighted-fairshare pick: a weight-10 group
// holds ~10× the slots of a weight-1 group under sustained contention.
func (bs *backendState) promote() {
	for bs.active < bs.capacity && len(bs.waiters) > 0 {
		best, bestIdx := math.Inf(1), -1
		for i, w := range bs.waiters {
			ratio := float64(bs.groupActive[w.group]) / float64(w.weight)
			if ratio < best {
				best, bestIdx = ratio, i
			}
		}
		w := bs.waiters[bestIdx]
		bs.waiters = append(bs.waiters[:bestIdx], bs.waiters[bestIdx+1:]...)
		bs.grant(w.group)
		close(w.ready)
	}
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
