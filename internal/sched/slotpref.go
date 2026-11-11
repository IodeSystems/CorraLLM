package sched

import (
	"context"
	"sync/atomic"
	"time"
)

// SLOT-AWARE DEQUEUE (P75 N9, under a model's slotAffinity flag).
//
// When a request releases its admission slot, the llama.cpp slot it ran on is
// now free. A queued turn of the conversation whose KV sits in THAT slot is
// the one worth granting: any other waiter would land on it and evict the
// conversation, or go elsewhere and re-prefill. So, among waiters of the same
// group as the normal pick (never across groups: that is fairshare's call,
// and never ahead of a preempt or priority waiter), the first waiter, in
// queue order, whose hint names the freed slot on the same backend instance
// goes first.
//
// Bounded: the waiter the normal pick chose is passed over at most
// slotSkipMax times, and never once it has queued slotSkipWait; then the
// normal order wins. Without a hint (no history, flag off) or without a freed
// slot in Done, nothing changes.

const (
	slotSkipMax  = 2
	slotSkipWait = 2 * time.Second
)

// SlotHint is a request's preferred llama.cpp slot: where its conversation's
// previous turn ran. Reordered is set when the queue granted it ahead of the
// normal pick because that slot had just been freed.
type SlotHint struct {
	Instance  string
	Slot      int
	Reordered atomic.Bool
}

type slotHintKey struct{}

// WithSlotHint attaches h to an admission context. nil or a negative slot
// attaches nothing.
func WithSlotHint(ctx context.Context, h *SlotHint) context.Context {
	if h == nil || h.Slot < 0 || h.Instance == "" {
		return ctx
	}
	return context.WithValue(ctx, slotHintKey{}, h)
}

func slotHintFrom(ctx context.Context) *SlotHint {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(slotHintKey{}).(*SlotHint)
	return h
}

func (h *SlotHint) matches(instance string, slot int) bool {
	return h != nil && instance != "" && h.Instance == instance && h.Slot == slot
}

// preferFreedSlot returns the index to grant instead of idx, the normal pick,
// when a freed slot is known. Caller holds s.mu.
func (s *Scheduler) preferFreedSlot(bs *backendState, backend string, now time.Time, idx int, d Done) int {
	if d.SlotInstance == "" || d.Slot < 0 {
		return idx
	}
	x := bs.waiters[idx]
	if x.preempt || x.slot.priority || x.slot.hint.matches(d.SlotInstance, d.Slot) {
		return idx
	}
	if x.skipped >= slotSkipMax || now.Sub(x.queuedAt) >= slotSkipWait {
		return idx
	}
	for i, w := range bs.waiters {
		if i == idx || w.preempt || w.slot.priority || w.slot.group != x.slot.group ||
			!w.slot.hint.matches(d.SlotInstance, d.Slot) {
			continue
		}
		if bs.active >= s.effCapLocked(bs, backend, w.slot.group, now) {
			continue
		}
		x.skipped++
		w.slot.hint.Reordered.Store(true)
		return i
	}
	return idx
}
