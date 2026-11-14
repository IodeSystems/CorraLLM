package sched

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// SLOT-AWARE DEQUEUE (P75; a model's slotAwareDequeue flag, or slotAffinity).
//
// When a request releases its admission slot, the llama.cpp slot it ran on is
// now free. A queued turn of the conversation whose KV sits in THAT slot is
// the one worth granting: any other waiter would land on it and evict the
// conversation, or go elsewhere and re-prefill. So, among waiters of the same
// group as the normal pick (never across groups: that is fairshare's call,
// and never ahead of a preempt or priority waiter), the first waiter, in
// queue order, whose hint names the freed slot on the same backend instance
// goes first (a). With slotAwareDequeue (Done.PreferUnsettled), when the
// normal pick's conversation lives on ANOTHER slot of that instance, an
// unsettled waiter (no slot home there) goes before it (b). Otherwise the
// normal pick (c).
//
// Bounded: the waiter the normal pick chose is passed over at most
// Done.SlotSkipMax times (0: config.DefaultSlotSkipMax), and never once it
// has queued Done.SlotSkipWait (0: config.DefaultSlotSkipWait); then the
// normal order wins. Without a hint (no history, flag off) or without a freed
// slot in Done, nothing changes.

// SlotHint is a request's preferred llama.cpp slot: where its conversation's
// previous turn ran. Reordered is set when the queue granted it ahead of the
// normal pick because that slot had just been freed. OnFreed is set when the
// queue granted it on the release of that very slot (reordered or not): the
// slot is idle at that moment, so pinning it does not defer.
type SlotHint struct {
	Instance  string
	Slot      int
	Reordered atomic.Bool
	OnFreed   atomic.Bool
}

type slotHintKey struct{}

// WithSlotHint attaches h to an admission context. nil attaches nothing. A
// hint with no slot (Slot < 0) marks an unsettled turn: it never matches a
// freed slot, and carries Reordered when (b) below granted it early.
func WithSlotHint(ctx context.Context, h *SlotHint) context.Context {
	if h == nil {
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

// unsettled: no slot home on instance (no history, or its slot is on another
// backend process, so its KV is gone from this one).
func (h *SlotHint) unsettled(instance string) bool {
	return h == nil || h.Slot < 0 || h.Instance != instance
}

// homedElsewhere: its conversation lives on another slot of instance.
func (h *SlotHint) homedElsewhere(instance string, slot int) bool {
	return h != nil && h.Slot >= 0 && h.Instance == instance && h.Slot != slot
}

// preferFreedSlot returns the index to grant instead of idx, the normal pick,
// when a freed slot is known. Caller holds s.mu.
func (s *Scheduler) preferFreedSlot(bs *backendState, backend string, now time.Time, idx int, d Done) int {
	if d.SlotInstance == "" || d.Slot < 0 {
		return idx
	}
	x := bs.waiters[idx]
	if x.slot.hint.matches(d.SlotInstance, d.Slot) {
		x.slot.hint.OnFreed.Store(true)
		return idx
	}
	if x.preempt || x.slot.priority {
		return idx
	}
	maxSkips, maxWait := d.SlotSkipMax, d.SlotSkipWait
	if maxSkips <= 0 {
		maxSkips = config.DefaultSlotSkipMax
	}
	if maxWait <= 0 {
		maxWait = config.DefaultSlotSkipWait
	}
	if x.skipped >= maxSkips || now.Sub(x.queuedAt) >= maxWait {
		return idx
	}
	eligible := func(i int, w *waiter) bool {
		return i != idx && !w.preempt && !w.slot.priority && w.slot.group == x.slot.group &&
			bs.active < s.effCapLocked(bs, backend, w.slot.group, now)
	}
	// (a) the conversation that lives on the freed slot.
	for i, w := range bs.waiters {
		if !eligible(i, w) || !w.slot.hint.matches(d.SlotInstance, d.Slot) {
			continue
		}
		x.skipped++
		w.slot.hint.Reordered.Store(true)
		w.slot.hint.OnFreed.Store(true)
		return i
	}
	// (b) slotAwareDequeue: an unsettled turn before one that lives on another
	// slot of this instance, so the freed slot does not take (and later
	// evict) a conversation whose KV is elsewhere.
	if d.PreferUnsettled && x.slot.hint.homedElsewhere(d.SlotInstance, d.Slot) {
		for i, w := range bs.waiters {
			if !eligible(i, w) || !w.slot.hint.unsettled(d.SlotInstance) {
				continue
			}
			x.skipped++
			if w.slot.hint != nil {
				w.slot.hint.Reordered.Store(true)
			}
			return i
		}
	}
	return idx
}
