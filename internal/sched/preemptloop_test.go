package sched

import "testing"

// The kick-loop question, answered structurally rather than by convention.
//
// pickVictim compares weights with a STRICT inequality, so preemption is a
// strict partial order on weight: a request can only ever be kicked by one of
// HIGHER priority. Equal-priority requests cannot kick each other, which is the
// cycle that would otherwise be possible (A kicks B, B retries, B kicks A,
// forever, with neither making progress and the backend thrashing).
//
// Determinism follows from the same property: the outcome depends only on the
// weights involved, never on arrival order or timing.
func TestPickVictim_StrictPriorityPreventsKickLoops(t *testing.T) {
	bs := &backendState{}
	// Three live requests at different priorities, all interruptible.
	lo := &slot{weight: 1, interruptible: true}
	mid := &slot{weight: 5, interruptible: true}
	hi := &slot{weight: 10, interruptible: true}
	bs.slots = []*slot{mid, hi, lo}

	// An EQUAL-weight caller must find no victim. This is the anti-loop rule.
	if v := bs.pickVictim(5); v != nil && v.weight == 5 {
		t.Error("equal weight kicked equal weight — two such callers would kick each other forever")
	}

	// The lowest weight below the preemptor is chosen, not merely any.
	bs.slots = []*slot{mid, hi, lo}
	lo.preempting, mid.preempting, hi.preempting = false, false, false
	if v := bs.pickVictim(10); v == nil || v.weight != 1 {
		t.Fatalf("victim = %v, want the lowest-weight (1) candidate", v)
	}

	// The very lowest priority can never kick anything, so the bottom of the
	// ladder cannot start a cascade.
	bs.slots = []*slot{mid, hi}
	mid.preempting, hi.preempting = false, false
	if v := bs.pickVictim(1); v != nil {
		t.Errorf("weight 1 kicked weight %d — the bottom rung must not preempt", v.weight)
	}
}

// A slot already being preempted must not be chosen again: two simultaneous
// high-priority arrivals would otherwise both "free" the same slot and both
// queue for it, and one would wait forever for capacity that was only ever
// released once.
func TestPickVictim_NoDoubleKick(t *testing.T) {
	bs := &backendState{}
	only := &slot{weight: 1, interruptible: true}
	bs.slots = []*slot{only}

	first := bs.pickVictim(10)
	if first == nil {
		t.Fatal("precondition: the first preemptor should find a victim")
	}
	if second := bs.pickVictim(10); second != nil {
		t.Error("the same slot was handed to a second preemptor")
	}
}

// Interruptibility is opt-in. A request that never volunteered — and whose
// group does not allow it — is not a candidate at any priority gap.
func TestPickVictim_NonInterruptibleIsNeverAVictim(t *testing.T) {
	bs := &backendState{}
	bs.slots = []*slot{{weight: 1, interruptible: false}}
	if v := bs.pickVictim(10); v != nil {
		t.Error("a non-interruptible request was preempted")
	}
}
