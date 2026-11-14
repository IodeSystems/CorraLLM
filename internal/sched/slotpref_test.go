package sched

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
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
	prefer  bool         // releases carry PreferUnsettled (slotAwareDequeue)
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
	q.enqueueWith(id, slot, group, false)
}

// enqueueWith is enqueue, as a priority request when prio is set.
func (q *slotQueue) enqueueWith(id string, slot int, group string, prio bool) {
	q.s.mu.Lock()
	n := len(q.s.backends["b"].waiters)
	q.s.mu.Unlock()
	ctx := context.Background()
	if prio {
		ctx = WithPriority(ctx)
	}
	if slot >= 0 {
		h := &SlotHint{Instance: "i", Slot: slot}
		q.hints[id] = h
		ctx = WithSlotHint(ctx, h)
	} else if h := q.hints[id]; h != nil {
		ctx = WithSlotHint(ctx, h) // preset by the test (another instance, or unsettled)
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
	return q.freeWith(slot, 0, 0)
}

// freeWith is free with the model's skip bound in Done (0: defaults).
func (q *slotQueue) freeWith(slot, maxSkips int, maxWait time.Duration) string {
	q.t.Helper()
	r := q.held[0]
	q.held = q.held[1:]
	d := Done{SlotSkipMax: maxSkips, SlotSkipWait: maxWait, PreferUnsettled: q.prefer}
	if slot >= 0 {
		d.SlotInstance, d.Slot = "i", slot
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

// The normal pick is passed over at most config.DefaultSlotSkipMax times.
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

// A waiter queued config.DefaultSlotSkipWait or longer is never passed over.
func TestSlotDequeueNoStarvationByWait(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("M", 1, "g")
	q.off.Store(int64(config.DefaultSlotSkipWait))
	if got := q.free(1); got != "A" {
		t.Fatalf("granted %s, want A (waited %s)", got, config.DefaultSlotSkipWait)
	}
}

// The model's own skip count (Done.SlotSkipMax) replaces the default.
func TestSlotDequeueConfiguredSkipMax(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("M1", 1, "g")
	q.enqueue("M2", 1, "g")
	for i, want := range []string{"M1", "A", "M2"} {
		if got := q.freeWith(1, 1, 0); got != want {
			t.Fatalf("release %d: granted %s, want %s", i, got, want)
		}
	}
}

// The model's own wait (Done.SlotSkipWait) replaces the default, both ways.
func TestSlotDequeueConfiguredSkipWait(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", -1, "g")
	q.enqueue("M", 1, "g")
	q.off.Store(int64(500 * time.Millisecond))
	if got := q.freeWith(1, 0, 400*time.Millisecond); got != "A" {
		t.Fatalf("waited 500ms of a 400ms bound: granted %s, want A", got)
	}
	q2 := newSlotQueue(t)
	q2.enqueue("A", -1, "g")
	q2.enqueue("M", 1, "g")
	q2.off.Store(int64(3 * time.Second)) // past the 2 s default
	if got := q2.freeWith(1, 0, 10*time.Second); got != "M" {
		t.Fatalf("waited 3s of a 10s bound: granted %s, want M", got)
	}
}

// A priority waiter is never passed over for a slot match, and a matching
// waiter does not jump ahead of one.
func TestSlotDequeueNeverPastPriority(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("M", 1, "g")
	q.enqueueWith("P", -1, "g", true)
	if got := q.free(1); got != "P" {
		t.Fatalf("granted %s, want the priority waiter P", got)
	}
	if q.hints["M"].Reordered.Load() {
		t.Error("M reordered past a priority waiter")
	}
}

// OnFreed: set when the grant follows the release of the waiter's own slot,
// reordered or not; never on a release of another slot or of an unknown one.
func TestSlotDequeueOnFreed(t *testing.T) {
	q := newSlotQueue(t)
	q.enqueue("A", 0, "g") // normal pick, its own slot freed
	q.enqueue("B", 1, "g") // granted on slot 0's release: not its slot
	q.enqueue("C", 1, "g") // granted on an unknown release
	if got := q.free(0); got != "A" {
		t.Fatalf("granted %s, want A", got)
	}
	if !q.hints["A"].OnFreed.Load() || q.hints["A"].Reordered.Load() {
		t.Error("A: want OnFreed, not Reordered")
	}
	if got := q.free(0); got != "B" {
		t.Fatalf("granted %s, want B", got)
	}
	if q.hints["B"].OnFreed.Load() {
		t.Error("B was granted on slot 0's release: OnFreed must stay false")
	}
	if got := q.free(-1); got != "C" {
		t.Fatalf("granted %s, want C", got)
	}
	if q.hints["C"].OnFreed.Load() {
		t.Error("C was granted on an unknown release: OnFreed must stay false")
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

// Fairshare across groups holds: the under-served group's normal pick wins
// over a slot match in the group that holds every slot.
func TestSlotDequeueKeepsFairshareAcrossGroups(t *testing.T) {
	q := newSlotQueue(t) // both held slots are group "g"
	q.enqueue("X", 1, "g")
	q.enqueue("A", -1, "other")
	if got := q.free(1); got != "A" {
		t.Fatalf("granted %s, want A (group other holds no slot)", got)
	}
	if q.hints["X"].Reordered.Load() {
		t.Error("X reordered across groups")
	}
}

// slotAwareDequeue order on a freed slot: (a) its own conversation, (b) an
// unsettled turn, (c) a turn homed on another slot.
func TestSlotDequeueUnsettledOrder(t *testing.T) {
	// (a) beats (b).
	q := newSlotQueue(t)
	q.prefer = true
	q.enqueue("H0", 0, "g")
	q.enqueue("U", -1, "g")
	q.enqueue("M", 1, "g")
	if got := q.free(1); got != "M" {
		t.Fatalf("(a): granted %s, want M (lives on slot 1)", got)
	}
	// (b) beats a normal pick homed on another slot. An unsettled turn is one
	// with no hint, a slotless hint, or a slot on another backend process.
	for _, u := range []struct {
		name string
		h    *SlotHint
	}{{"no hint", nil}, {"slotless hint", &SlotHint{Slot: -1}}, {"other instance", &SlotHint{Instance: "old", Slot: 1}}} {
		q := newSlotQueue(t)
		q.prefer = true
		q.enqueue("H0", 0, "g")
		if u.h != nil {
			q.hints["U"] = u.h
		}
		q.enqueue("U", -1, "g")
		if got := q.free(1); got != "U" {
			t.Fatalf("(b) %s: granted %s, want U", u.name, got)
		}
		if u.h != nil && !u.h.Reordered.Load() {
			t.Errorf("(b) %s: Reordered not set", u.name)
		}
		if u.h != nil && u.h.OnFreed.Load() {
			t.Errorf("(b) %s: OnFreed set, but slot 1 is not its home", u.name)
		}
	}
	// (c) after the skip bound: H0 is passed over twice, then wins.
	q = newSlotQueue(t)
	q.prefer = true
	q.enqueue("H0", 0, "g")
	q.enqueue("U1", -1, "g")
	q.enqueue("U2", -1, "g")
	q.enqueue("U3", -1, "g")
	for i, want := range []string{"U1", "U2", "H0", "U3"} {
		if got := q.free(1); got != want {
			t.Fatalf("(c) release %d: granted %s, want %s", i, got, want)
		}
	}
	// (c) with no unsettled waiter: the homed-elsewhere normal pick, at once.
	q = newSlotQueue(t)
	q.prefer = true
	q.enqueue("H0", 0, "g")
	if got := q.free(1); got != "H0" {
		t.Fatalf("(c) alone: granted %s, want H0", got)
	}
}

// An unsettled normal pick is not passed over for another unsettled turn, and
// without PreferUnsettled (slotAffinity) step (b) does not run.
func TestSlotDequeueUnsettledOnlyWithPrefer(t *testing.T) {
	q := newSlotQueue(t)
	q.prefer = true
	q.enqueue("U1", -1, "g")
	q.enqueue("U2", -1, "g")
	if got := q.free(1); got != "U1" {
		t.Fatalf("granted %s, want U1 (FIFO among unsettled)", got)
	}
	q2 := newSlotQueue(t) // slotAffinity: no step (b)
	q2.enqueue("H0", 0, "g")
	q2.enqueue("U", -1, "g")
	if got := q2.free(1); got != "H0" {
		t.Fatalf("without PreferUnsettled: granted %s, want H0", got)
	}
}
