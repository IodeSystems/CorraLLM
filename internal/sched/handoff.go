package sched

import (
	"context"
	"errors"
	"time"
)

// Blue-green handoff. During one, two corrallm processes forward to the same
// backends: the old one finishing what it admitted, the new one taking
// everything else. A backend's slots are a property of the BACKEND, not of
// either process — a llama-server started with --parallel 1 answers one
// request at a time no matter who sent it — so for the overlap the two
// schedulers must behave as one:
//
//   - the old process admits nothing more (Drain). Its queued requests are
//     handed to the new process instead of waiting for a slot here, so from
//     the moment it drains its occupancy only shrinks;
//   - the new process counts the old one's occupancy as slots already taken
//     (SetForeign), refreshed as the old one reports releases;
//   - and admits nothing until it has the old one's first post-drain report
//     (HoldAdmission/ReleaseAdmission), so there is no window in which both
//     admit against an empty view of the other.

// ErrDraining refuses admission in a process that has handed over to its
// successor. The caller forwards the request there rather than failing it.
var ErrDraining = errors.New("sched: draining; this process has handed over to its successor")

// Drain stops all further admission: every later Admit, and every request
// still queued, gets ErrDraining. Slots already granted run to completion.
// Idempotent.
func (s *Scheduler) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return
	}
	s.draining = true
	close(s.drainCh)
}

// Draining reports whether Drain has been called.
func (s *Scheduler) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// OwnActive is this process's granted slots per backend — what a draining
// process reports to its successor. Foreign slots are not included.
func (s *Scheduler) OwnActive() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for name, bs := range s.backends {
		if n := bs.active - bs.foreign; n > 0 {
			out[name] = n
		}
	}
	return out
}

// SetForeign records how many slots ANOTHER process holds on each backend, as
// that process last reported. They count against capacity exactly like this
// process's own (and show as active in Snapshot), but are not this process's
// to preempt or release. A backend missing from m holds none; nil clears all.
// A drop admits waiters into the freed slots.
func (s *Scheduler) SetForeign(m map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	names := map[string]bool{}
	for name, bs := range s.backends {
		if bs.foreign > 0 {
			names[name] = true
		}
	}
	for name := range m {
		names[name] = true
	}
	for name := range names {
		n := m[name]
		if n < 0 {
			n = 0
		}
		bs := s.backends[name]
		if bs == nil {
			if n == 0 {
				continue
			}
			// Capacity is learned at this process's first Admit; until then the
			// slots are simply occupied.
			bs = &backendState{groupActive: map[string]int{},
				share: map[string]float64{}, shareAt: map[string]time.Time{}}
			s.backends[name] = bs
		}
		delta := n - bs.foreign
		bs.foreign = n
		bs.active += delta
		if delta < 0 {
			for _, ps := range s.promote(bs, name, now) {
				s.recordAdmitLocked(now, ps.group, ps.backendType)
			}
		}
	}
}

// HoldAdmission makes Admit wait until ReleaseAdmission. For a successor
// between taking the listener and receiving its predecessor's first
// occupancy report.
func (s *Scheduler) HoldAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold == nil {
		s.hold = make(chan struct{})
	}
}

// ReleaseAdmission ends a HoldAdmission. Idempotent.
func (s *Scheduler) ReleaseAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold = nil
	}
}

// awaitHold blocks while a HoldAdmission is in place.
func (s *Scheduler) awaitHold(ctx context.Context) error {
	s.mu.Lock()
	h := s.hold
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	select {
	case <-h:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
