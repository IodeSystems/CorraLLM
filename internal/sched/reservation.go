package sched

import (
	"context"
	"sort"
	"time"
)

// defaultMaxReservationTTL bounds how long a single reservation lease lasts before
// it must be renewed — short so a dead client can't starve batch. Renewal (a
// heartbeat re-POST) resets it.
const defaultMaxReservationTTL = 5 * time.Minute

// reservation is one lane's lease on `slots` of a backend until expiresAt.
type reservation struct {
	slots     int
	expiresAt time.Time
}

// ReservationInfo is a public view of a live reservation (for the API/UI).
type ReservationInfo struct {
	Backend   string
	Lane      string
	Slots     int
	ExpiresAt time.Time
}

// SetMaxReservationTTL overrides the lease cap (0 keeps the 5m default).
func (s *Scheduler) SetMaxReservationTTL(d time.Duration) { s.maxResTTL = d }

// MaxReservationTTL is the effective lease cap.
func (s *Scheduler) MaxReservationTTL() time.Duration {
	if s.maxResTTL > 0 {
		return s.maxResTTL
	}
	return defaultMaxReservationTTL
}

// Reserve creates or renews (heartbeat) `lane`'s reservation of `slots` on a
// backend for up to `ttl`, capped at the max. Returns the effective expiry.
func (s *Scheduler) Reserve(backend, lane string, slots int, ttl time.Duration) time.Time {
	if slots < 1 {
		slots = 1
	}
	if max := s.MaxReservationTTL(); ttl <= 0 || ttl > max {
		ttl = max
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.reservations[backend]
	if m == nil {
		m = map[string]*reservation{}
		s.reservations[backend] = m
	}
	exp := s.now().Add(ttl)
	m[lane] = &reservation{slots: slots, expiresAt: exp}
	return exp
}

// Release drops `lane`'s reservation on a backend and wakes any batch waiters the
// freed capacity now admits.
func (s *Scheduler) Release(backend, lane string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.reservations[backend]
	if m == nil {
		return
	}
	delete(m, lane)
	if len(m) == 0 {
		delete(s.reservations, backend)
	}
	if bs := s.backends[backend]; bs != nil {
		s.promote(bs, backend, s.now())
	}
}

// reservedByOthersLocked sums live reservations on a backend held by lanes other
// than `lane` (expired ones ignored). Caller holds s.mu.
func (s *Scheduler) reservedByOthersLocked(backend, lane string, now time.Time) int {
	total := 0
	for l, r := range s.reservations[backend] {
		if l == lane || !r.expiresAt.After(now) {
			continue
		}
		total += r.slots
	}
	return total
}

// effCapLocked is the capacity available to `lane` on a backend: physical capacity
// minus what OTHER lanes have reserved. Caller holds s.mu.
func (s *Scheduler) effCapLocked(bs *backendState, backend, lane string, now time.Time) int {
	eff := bs.capacity - s.reservedByOthersLocked(backend, lane, now)
	if eff < 0 {
		eff = 0
	}
	return eff
}

// reapExpired prunes expired reservations and wakes waiters on affected backends
// (the freed capacity may now admit batch).
func (s *Scheduler) reapExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for backend, m := range s.reservations {
		changed := false
		for lane, r := range m {
			if !r.expiresAt.After(now) {
				delete(m, lane)
				changed = true
			}
		}
		if len(m) == 0 {
			delete(s.reservations, backend)
		}
		if changed {
			if bs := s.backends[backend]; bs != nil {
				s.promote(bs, backend, now)
			}
		}
	}
}

// StartReaper runs a background loop that expires stale reservations until ctx
// ends. Started by the server; tests drive reapExpired directly via the clock.
func (s *Scheduler) StartReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reapExpired()
			}
		}
	}()
}

// Reservations returns a sorted snapshot of live reservations (for the API/UI).
func (s *Scheduler) Reservations() []ReservationInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []ReservationInfo
	for backend, m := range s.reservations {
		for lane, r := range m {
			if !r.expiresAt.After(now) {
				continue
			}
			out = append(out, ReservationInfo{Backend: backend, Lane: lane, Slots: r.slots, ExpiresAt: r.expiresAt})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Backend != out[j].Backend {
			return out[i].Backend < out[j].Backend
		}
		return out[i].Lane < out[j].Lane
	})
	return out
}
