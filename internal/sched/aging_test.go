package sched

import (
	"context"
	"testing"
	"time"
)

// waiterFor builds a queued waiter directly, which is how the priority logic is
// exercised without racing real goroutines through Admit.
func waiterFor(group string, weight int, since time.Time) *waiter {
	return &waiter{slot: &slot{group: group, weight: weight, currency: "requests", since: since}}
}

// The case aging exists for: two callers of equal standing, one of whom has
// been trying since before the other arrived. Without age the pick falls to
// list order, which is how a caller starves on a one-slot backend — every time
// it comes back, somebody's fresh request is exactly as deserving.
func TestAnOlderAttemptWinsAgainstAnEqualNewcomer(t *testing.T) {
	now := time.Now()
	bs := &backendState{capacity: 1, groupActive: map[string]int{}, share: map[string]float64{}}
	// The newcomer is FIRST in the list, so a win here cannot be arrival order.
	bs.waiters = []*waiter{
		waiterFor("g", 1, time.Time{}),
		waiterFor("g", 1, now.Add(-45*time.Second)),
	}

	if idx := bs.pickWaiter(now); idx != 1 {
		t.Errorf("picked waiter %d; the 45s-old attempt should have won", idx)
	}
}

// Age is a discount on the ratio, not a priority tier. A weight the operator
// chose must survive a patient caller in a lighter group, or aging simply
// trades one starvation for another.
func TestAgingCannotOverturnALargeWeightDifference(t *testing.T) {
	now := time.Now()
	bs := &backendState{capacity: 1, groupActive: map[string]int{"heavy": 1, "light": 1},
		share: map[string]float64{}}
	// Same consumption, very different weights: the heavy group is meant to win
	// by 10x. The light group's attempt has been going for an hour.
	bs.waiters = []*waiter{
		waiterFor("light", 1, now.Add(-time.Hour)),
		waiterFor("heavy", 10, time.Time{}),
	}
	if idx := bs.pickWaiter(now); idx != 1 {
		t.Errorf("picked the light group's waiter; a 10x weight must outlast any wait")
	}

	// ...but a close contest is exactly what age is for.
	bs.waiters = []*waiter{
		waiterFor("light", 1, now.Add(-time.Hour)),
		waiterFor("heavy", 2, time.Time{}),
	}
	if idx := bs.pickWaiter(now); idx != 0 {
		t.Errorf("picked the fresh waiter; a 2x weight is within reach of the %v cap", maxAgingBoost)
	}
}

func TestAgingBoostShape(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		age  time.Duration
		want float64
	}{
		{"a fresh request is not aged", 0, 1},
		{"one unit is twice as deserving", agingUnit, 2},
		{"three units", 3 * agingUnit, 4},
		{"capped", 100 * agingUnit, maxAgingBoost},
	}
	for _, c := range cases {
		got := agingBoost(now, now.Add(-c.age))
		if got != c.want {
			t.Errorf("%s: boost(%s) = %v, want %v", c.name, c.age, got, c.want)
		}
	}
	if b := agingBoost(now, time.Time{}); b != 1 {
		t.Errorf("an unmarked request must be unaged, got %v", b)
	}
	// A clock that runs backwards, or a claim of the future, is not credit.
	if b := agingBoost(now, now.Add(time.Hour)); b != 1 {
		t.Errorf("a future start time bought %v", b)
	}
}

// The mark travels on the context, so every existing caller and test keeps its
// exact behaviour by simply not setting it.
func TestWaitingSinceRoundTrip(t *testing.T) {
	ctx := context.Background()
	if !waitingSince(ctx).IsZero() {
		t.Error("an unmarked context reported an attempt start")
	}
	since := time.Now().Add(-time.Minute)
	if got := waitingSince(WithWaitingSince(ctx, since)); !got.Equal(since) {
		t.Errorf("waitingSince = %v, want %v", got, since)
	}
	if !waitingSince(WithWaitingSince(ctx, time.Time{})).IsZero() {
		t.Error("a zero mark should leave the context unmarked rather than store a zero time")
	}
}

// Queue admission and slot admission must rank the same pair identically. If
// only the grant path aged, an aged waiter could be bumped OUT of the queue by
// an arrival it would have beaten to the next free slot.
func TestBumpingUsesTheSameAgedRatioAsGranting(t *testing.T) {
	now := time.Now()
	bs := &backendState{capacity: 1, groupActive: map[string]int{}, share: map[string]float64{}}
	aged := waiterFor("g", 1, now.Add(-45*time.Second))
	bs.waiters = []*waiter{aged}

	// A fresh arrival of equal standing must not displace the aged waiter.
	fresh := &slot{group: "g", weight: 1, currency: "requests"}
	if v := bs.pickBumpable(now, fresh); v != nil {
		t.Error("a fresh arrival bumped an older attempt out of the queue")
	}

	// An arrival that IS more deserving still bumps, aged waiter or not.
	strong := &slot{group: "heavy", weight: 20, currency: "requests"}
	bs.groupActive["g"] = 1
	if v := bs.pickBumpable(now, strong); v != aged {
		t.Error("a decisively more deserving arrival failed to bump")
	}
}
