package sched

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// slotQueue is a backend "b" (capacity 2, both held) and a queue the test fills
// one waiter at a time, in order.
type slotQueue struct {
	t       *testing.T
	s       *Scheduler
	held    []func(...Done)
	granted chan string
	hints   map[string]*SlotHint
	off     atomic.Int64 // clock offset, ns
}

func newSlotQueue(t *testing.T) *slotQueue {
	q := &slotQueue{t: t, s: New(), granted: make(chan string, 16), hints: map[string]*SlotHint{}}
	base := time.Now()
	q.s.now = func() time.Time { return base.Add(time.Duration(q.off.Load())) }
	for range 2 {
		r, _, err := q.s.Admit(context.Background(), "b", "local", 2, "g", 1, false, queueStage)
		if err != nil {
			t.Fatal(err)
		}
		q.held = append(q.held, r)
	}
	return q
}

// enqueue adds waiter id with a hint for slot (-1: none) on instance "i".
func (q *slotQueue) enqueue(id string, slot int, group string) {
	q.s.mu.Lock()
	n := len(q.s.backends["b"].waiters)
	q.s.mu.Unlock()
	ctx := context.Background()
	if slot >= 0 {
		h := &SlotHint{Instance: "i", Slot: slot}
		q.hints[id] = h
		ctx = WithSlotHint(ctx, h)
	}
	go func() {
		r, _, err := q.s.Admit(ctx, "b", "local", 2, group, 1, false, queueStage)
		if err != nil {
			q.t.Errorf("%s: %v", id, err)
			return
		}
		q.granted <- id
		_ = r // held until the test ends; the queue only moves on explicit release
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.s.mu.Lock()
		m := len(q.s.backends["b"].waiters)
		q.s.mu.Unlock()
		if m == n+1 {
			return
		}
		if time.Now().After(deadline) {
			q.t.Fatalf("%s never queued", id)
		}
		time.Sleep(time.Millisecond)
	}
}

// free releases one held slot, saying it freed slot (-1: not known), and
// returns who was granted.
func (q *slotQueue) free(slot int) string {
	q.t.Helper()
	r := q.held[0]
	q.held = q.held[1:]
	d := Done{}
	if slot >= 0 {
		d = Done{SlotInstance: "i", Slot: slot}
	}
	r(d)
	select {
	case id := <-q.granted:
		// The granted request now holds a slot; the next free releases one.
		q.held = append(q.held, q.releaseOf())
		return id
	case <-time.After(2 * time.Second):
		q.t.Fatal("nobody granted")
	}
	return ""
}

// releaseOf frees one active slot directly (the granted waiter's), as its
// release would.
func (q *slotQueue) releaseOf() func(...Done) {
	return func(d ...Done) {
		q.s.mu.Lock()
		bs := q.s.backends["b"]
		sl := bs.slots[0]
		q.s.mu.Unlock()
		q.s.releaser("b", sl)(d...)
	}
}

func TestSlotDequeuePicksFreedSlotsWaiter(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", 0, "g")
	q.enqueue("B", 1, "g")
	q.enqueue("C", -1, "g")
	if got := q.free(1); got != "B" {
		t.Fatalf("freed slot 1 granted %s, want B", got)
	}
	if !q.hints["B"].Reordered.Load() || q.hints["A"].Reordered.Load() {
		t.Error("Reordered not set on B alone")
	}
	if got := q.free(0); got != "A" {
		t.Fatalf("freed slot 0 granted %s, want A", got)
	}
	if q.hints["A"].Reordered.Load() {
		t.Error("A was the normal pick: not reordered")
	}
	if got := q.free(1); got != "C" {
		t.Fatalf("got %s, want C", got)
	}
}

// Flag off: no hints and no freed slot in Done. Either alone also keeps
// today's order exactly.
func TestSlotDequeueOffIsFIFO(t *testing.T) {
	q := newSlotQueue(t) // hints, but releases report no slot
	q.enqueue("A", -1, "g")
	q.enqueue("B", 0, "g")
	q.enqueue("C", 1, "g")
	for _, want := range []string{"A", "B", "C"} {
		if got := q.free(-1); got != want {
			t.Fatalf("no freed slot: granted %s, want %s", got, want)
		}
	}
	q2 := newSlotQueue(t) // a freed slot, but no hints
	q2.enqueue("A", -1, "g")
	q2.enqueue("B", -1, "g")
	for _, want := range []string{"A", "B"} {
		if got := q2.free(1); got != want {
			t.Fatalf("no hints: granted %s, want %s", got, want)
		}
	}
}

// The normal pick is passed over at most slotSkipMax times.
func TestSlotDequeueSkipLimit(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("M1", 1, "g")
	q.enqueue("M2", 1, "g")
	q.enqueue("M3", 1, "g")
	for i, want := range []string{"M1", "M2", "A", "M3"} {
		if got := q.free(1); got != want {
			t.Fatalf("release %d: granted %s, want %s", i, got, want)
		}
	}
}

// A waiter queued slotSkipWait or longer is never passed over.
func TestSlotDequeueNoStarvationByWait(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("M", 1, "g")
	q.off.Store(int64(slotSkipWait))
	if got := q.free(1); got != "A" {
		t.Fatalf("granted %s, want A (waited %s)", got, slotSkipWait)
	}
}

// Another group's matching waiter does not jump: that is fairshare's call.
func TestSlotDequeueSameGroupOnly(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("X", 1, "other")
	if got := q.free(1); got == "X" && q.hints["X"].Reordered.Load() {
		t.Fatal("a waiter of another group was reordered ahead")
	}
}
