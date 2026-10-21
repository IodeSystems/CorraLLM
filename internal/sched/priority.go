package sched

import "context"

// PRIORITY: a request marked priority goes to the head of its backend's
// admission queue.
//
// It exists for the running benchmark. A bench measures the model; queue time
// behind production callers measures the box instead, and on a 1-slot model a
// run that needed ~10 minutes of slot time took 2h28m and lost stages to it.
//
// What priority does, and nothing more:
//   - Among waiters, priority waiters are granted first, FIFO among themselves.
//     Preempt waiters still go before them: each already freed a slot.
//   - maxQueueDepth counts only the priority waiters ahead of a priority
//     arrival. Normal callers queued ahead do not cause a refusal, and are not
//     bumped to make room either.
//   - A normal arrival never bumps a priority waiter out of a full queue.
//
// What it does not do: it does not preempt a running request, does not change
// the group's weight, budgets, reservations or stage verbs. A free slot is still
// granted directly, and a reservation still keeps slots from other lanes.

type priorityKey struct{}

// WithPriority marks a request for the head of the admission queue. Carried on
// the context for the same reason as WithWaitingSince: optional per-request
// metadata, and an unmarked context behaves exactly as before.
func WithPriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, priorityKey{}, true)
}

func priorityFrom(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(priorityKey{}).(bool)
	return v
}

// queuedAhead is how many waiters count against maxQueueDepth for an arrival:
// all of them for a normal caller, only the priority ones for a priority
// caller. Caller holds s.mu.
func (bs *backendState) queuedAhead(sl *slot) int {
	if !sl.priority {
		return len(bs.waiters)
	}
	n := 0
	for _, w := range bs.waiters {
		if w.slot.priority {
			n++
		}
	}
	return n
}

// PriorityForTest exposes the mark to tests in other packages, as
// WaitingSinceForTest does: the proxy sets it and the scheduler reads it.
func PriorityForTest(ctx context.Context) bool { return priorityFrom(ctx) }
