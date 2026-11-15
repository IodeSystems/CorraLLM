package sched

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// KV-POOL ADMISSION (P75, 11-15; a model's kvPool).
//
// A llama-server with --kv-unified shares one KV pool across its slots, and
// any slot may grow to the whole pool. When the pool is full llama-server
// clears idle slots, halves its batch down to 1, and then fails every request
// in the batch. So corrallm counts the expected KV tokens of the requests in
// service on the model (each request's KVAsk: estimated prompt + output
// reserve) and does not grant a request whose ask would take the sum past the
// pool. Idle llama.cpp slots do not count (llama-server clears them); only
// granted, unreleased admissions do.
//
//   - Lone request: when nothing tracked is in service, the request is
//     granted whatever its size, so an oversized request reaches llama-server
//     and gets llama-server's own error rather than waiting forever.
//   - Bypass: while a waiter is held by the pool, smaller requests behind it
//     (and new arrivals) that fit may go ahead, for at most the waiter's
//     KVAsk.Bypass (0: config.DefaultKVBypassFor) from the moment it was
//     first held. After that only the held waiter may be granted, when it
//     fits; everyone else waits.
//   - A request held by the pool while a slot is free queues whatever its
//     stage says (the slot is not the short resource), bounded by the
//     model's maxWait as any waiter is.
//
// Slot-aware dequeue (slotpref.go) still orders the queue; it only skips a
// waiter that does not fit.

// KVAsk is one request's expected KV need on a model with kvPool.
type KVAsk struct {
	Tokens int           // estimated prompt tokens + output reserve
	Pool   int           // the model's kvPool.tokens
	Bypass time.Duration // 0: config.DefaultKVBypassFor
	// Held is set once the pool held this request in the queue.
	Held atomic.Bool
}

type kvAskKey struct{}

// WithKVAsk attaches a to an admission context. nil, or a pool ≤ 0, attaches
// nothing.
func WithKVAsk(ctx context.Context, a *KVAsk) context.Context {
	if a == nil || a.Pool <= 0 {
		return ctx
	}
	return context.WithValue(ctx, kvAskKey{}, a)
}

func kvAskFrom(ctx context.Context) *KVAsk {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(kvAskKey{}).(*KVAsk)
	return a
}

func (a *KVAsk) bypass() time.Duration {
	if a == nil || a.Bypass <= 0 {
		return config.DefaultKVBypassFor
	}
	return a.Bypass
}

// kvLogEvery rate-limits the "kv pool hold" line per backend.
const kvLogEvery = 10 * time.Second

// kvFitsLocked reports whether sl's ask fits beside what is in service. An
// untracked slot always fits. Caller holds s.mu.
func (bs *backendState) kvFitsLocked(sl *slot) bool {
	if sl.kv == nil {
		return true
	}
	bs.kvPool = sl.kv.Pool
	return bs.kvInService == 0 || bs.kvInService+sl.kv.Tokens <= sl.kv.Pool
}

// kvStarvedLocked is the earliest-held waiter whose bypass window has run
// out, or nil. While it is non-nil only it may be granted. Caller holds s.mu.
func (bs *backendState) kvStarvedLocked(now time.Time) *waiter {
	var best *waiter
	for _, w := range bs.waiters {
		if w.kvHeldAt.IsZero() || now.Sub(w.kvHeldAt) < w.slot.kv.bypass() {
			continue
		}
		if best == nil || w.kvHeldAt.Before(best.kvHeldAt) {
			best = w
		}
	}
	return best
}

// kvAdmitsArrivalLocked decides a new arrival when a slot is free. Caller
// holds s.mu.
func (bs *backendState) kvAdmitsArrivalLocked(sl *slot, now time.Time) bool {
	return bs.kvStarvedLocked(now) == nil && bs.kvFitsLocked(sl)
}

// kvHoldLocked marks w held by the pool (first time only), counts it and
// logs, rate-limited. Caller holds s.mu.
func (bs *backendState) kvHoldLocked(backend string, w *waiter, now time.Time) {
	if w.kvHeldAt.IsZero() {
		w.kvHeldAt = now
	}
	a := w.slot.kv
	if a == nil || !a.Held.CompareAndSwap(false, true) {
		return
	}
	bs.kvHolds++
	bs.kvHoldsUnlogged++
	if now.Sub(bs.kvLogAt) < kvLogEvery {
		return
	}
	slog.Info("kv pool hold", "model", backend, "need", a.Tokens, "in_service", bs.kvInService,
		"pool", a.Pool, "holds", bs.kvHoldsUnlogged, "holds_total", bs.kvHolds)
	bs.kvLogAt, bs.kvHoldsUnlogged = now, 0
}

// kvGrantLocked / kvReleaseLocked move a slot's ask in and out of the
// in-service sum. Caller holds s.mu.
func (bs *backendState) kvGrantLocked(sl *slot) {
	if sl.kv == nil {
		return
	}
	bs.kvPool = sl.kv.Pool
	bs.kvInService += sl.kv.Tokens
	if bs.kvInService > bs.kvPeak {
		bs.kvPeak = bs.kvInService
	}
}

func (bs *backendState) kvReleaseLocked(sl *slot) {
	if sl.kv == nil {
		return
	}
	bs.kvInService -= sl.kv.Tokens
	if bs.kvInService < 0 {
		bs.kvInService = 0
	}
}

// kvPickLocked applies the pool to promote's pick idx: idx when it fits and
// nothing is starved; the starved waiter when it fits and may take a slot;
// else the first fitting grantable waiter in the normal order while no one
// is starved; else -1. Caller holds s.mu.
func (s *Scheduler) kvPickLocked(bs *backendState, backend string, now time.Time, idx int) int {
	if st := bs.kvStarvedLocked(now); st != nil {
		for i, w := range bs.waiters {
			if w != st {
				continue
			}
			room := bs.active < s.effCapLocked(bs, backend, w.slot.group, now)
			if w.preempt {
				room = bs.active < bs.capacity
			}
			if room && bs.kvFitsLocked(w.slot) {
				return i
			}
			return -1
		}
	}
	w := bs.waiters[idx]
	if bs.kvFitsLocked(w.slot) {
		return idx
	}
	bs.kvHoldLocked(backend, w, now)
	return s.pickGrantableWaiterIf(bs, backend, now, func(x *waiter) bool { return bs.kvFitsLocked(x.slot) })
}

// repromoteKVLocked fills free slots after a waiter left the queue of a
// backend with a pool: a held waiter that leaves may have been all that
// stopped the others, and no release is coming to promote them when nothing
// is in service. Caller holds s.mu.
func (s *Scheduler) repromoteKVLocked(bs *backendState, backend string) {
	if bs == nil || bs.kvPool == 0 {
		return
	}
	now := s.now()
	for _, ps := range s.promote(bs, backend, now) {
		if !isGate(backend) {
			s.recordAdmitLocked(now, ps.group, ps.backendType)
		}
	}
}
