package sched

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

var queueStage = config.Stage{Queue: true}
var rejectStage = config.Stage{Reject: true}

// TestMaxQueueDepth: once the queue is full, a further queued request is
// rejected fast (429) instead of enqueued.
func TestMaxQueueDepth(t *testing.T) {
	s := NewWithConfig(&config.Config{Scheduler: config.SchedulerConfig{MaxQueueDepth: 1}})
	ctx := context.Background()

	r1, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage) // active
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	go func() { _, _, _ = s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage) }() // queues (depth 1)
	time.Sleep(100 * time.Millisecond)

	// Queue is full (depth 1) → reject.
	_, _, err = s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("want rejected backpressure, got %v", err)
	}
}

// TestMaxWait: a queued request gives up with a 429 after maxWait rather than
// blocking on the (longer) request context.
func TestMaxWait(t *testing.T) {
	s := NewWithConfig(&config.Config{Scheduler: config.SchedulerConfig{MaxWait: "60ms"}})
	ctx := context.Background()

	r1, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage) // holds the slot
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	start := time.Now()
	_, _, err = s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage) // queues, then maxWait fires
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "queue-timeout" {
		t.Fatalf("want queue-timeout, got %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("maxWait not honored: waited %s", waited)
	}
}

// TestRetryAfterFromDwell: Retry-After reflects the measured per-request service
// time (dwell EWMA), not a flat 1s.
func TestRetryAfterFromDwell(t *testing.T) {
	s := New()
	clk := time.Unix(1000, 0)
	s.now = func() time.Time { return clk }
	ctx := context.Background()

	// One request that takes 10s of service time seeds the dwell EWMA.
	r1, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(10 * time.Second)
	r1(Done{})

	// Saturate, then a rejected request should suggest ~10s (1 round × 10s dwell).
	r2, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	_, _, err = s.Admit(ctx, "b", "local", 1, "g", 1, false, rejectStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) {
		t.Fatalf("want backpressure, got %v", err)
	}
	if bp.RetryAfter != 10*time.Second {
		t.Errorf("Retry-After = %s, want 10s (from dwell EWMA)", bp.RetryAfter)
	}
}

func TestAdmitUpToCapacity(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, _, err := s.Admit(ctx, "b", "local", 2, "g", 1, false, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := s.Admit(ctx, "b", "local", 2, "g", 1, false, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	// Third over capacity with reject stage → BackpressureError now.
	_, _, err = s.Admit(ctx, "b", "local", 2, "g", 1, false, rejectStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) {
		t.Fatalf("want BackpressureError, got %v", err)
	}
	if bp.Reason != "rejected" || bp.Capacity != 2 || bp.InFlight != 2 {
		t.Errorf("backpressure = %+v", bp)
	}
	if bp.RetryAfter < time.Second {
		t.Errorf("retry-after should be >= 1s, got %s", bp.RetryAfter)
	}
	r1()
	// A freed slot admits again.
	r3, _, err := s.Admit(ctx, "b", "local", 2, "g", 1, false, rejectStage)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
	r3()
}

// TestSnapshot reports per-backend load with a per-group active/waiting
// breakdown: two groups fill capacity, a third request queues.
func TestSnapshot(t *testing.T) {
	s := New()
	ctx := context.Background()
	// Capacity 2: admit one slot each for g1 and g2 → backend full.
	r1, _, err := s.Admit(ctx, "b", "local", 2, "g1", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	r2, _, err := s.Admit(ctx, "b", "local", 2, "g2", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	defer r2()

	// A third request in g1 queues (capacity reached).
	go func() { _, _, _ = s.Admit(ctx, "b", "local", 2, "g1", 1, false, queueStage) }()
	time.Sleep(100 * time.Millisecond) // let it enqueue

	snap := s.Snapshot()
	if len(snap.Backends) != 1 {
		t.Fatalf("want 1 backend, got %d", len(snap.Backends))
	}
	b := snap.Backends[0]
	if b.Backend != "b" || b.Capacity != 2 || b.Active != 2 || b.Waiting != 1 {
		t.Fatalf("backend load = %+v", b)
	}
	got := map[string]GroupLoad{}
	for _, g := range b.Groups {
		got[g.Group] = g
	}
	if got["g1"] != (GroupLoad{Group: "g1", Active: 1, Waiting: 1}) {
		t.Errorf("g1 load = %+v", got["g1"])
	}
	if got["g2"] != (GroupLoad{Group: "g2", Active: 1, Waiting: 0}) {
		t.Errorf("g2 load = %+v", got["g2"])
	}
}

func TestQueueWaitsThenAdmits(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, _, _ := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)

	admitted := make(chan struct{})
	go func() {
		r2, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
		if err != nil {
			t.Errorf("queued admit: %v", err)
			return
		}
		close(admitted)
		r2()
	}()

	select {
	case <-admitted:
		t.Fatal("queued request admitted before slot freed")
	case <-time.After(100 * time.Millisecond):
	}

	r1() // free the slot → the waiter is promoted
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request never admitted after release")
	}
}

func TestQueueTimeout(t *testing.T) {
	s := New()
	r1, _, _ := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, queueStage)
	defer r1()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "queue-timeout" {
		t.Fatalf("want queue-timeout backpressure, got %v", err)
	}
}

// TestWeightedFairnessPromotion: under saturation with both groups continuously
// queued, slots are allocated proportional to weight. Weight 3 vs 1 over 4 slots
// → 3 slots to "hi", 1 to "lo". Deterministic: drives promote() directly.
func TestWeightedFairnessPromotion(t *testing.T) {
	bs := &backendState{capacity: 4, groupActive: map[string]int{}}
	for range 10 {
		bs.waiters = append(bs.waiters, &waiter{slot: &slot{group: "hi", weight: 3}, ready: make(chan struct{})})
	}
	for range 10 {
		bs.waiters = append(bs.waiters, &waiter{slot: &slot{group: "lo", weight: 1}, ready: make(chan struct{})})
	}
	New().promote(bs, "b", time.Now()) // fills all 4 slots by min(active/weight)

	if bs.groupActive["hi"] != 3 || bs.groupActive["lo"] != 1 {
		t.Fatalf("weighted share: hi=%d lo=%d, want 3 and 1", bs.groupActive["hi"], bs.groupActive["lo"])
	}
	if bs.active != 4 || len(bs.waiters) != 16 {
		t.Errorf("active=%d waiters=%d, want 4 and 16", bs.active, len(bs.waiters))
	}
}

var preemptStage = config.Stage{Preempt: true}

// TestPreemptCancelsLowerInterruptible: a low, interruptible group holds the only
// slot; a higher group with a preempt stage cancels it (cause ErrPreempted) and
// takes the slot once the victim releases.
func TestPreemptCancelsLowerInterruptible(t *testing.T) {
	s := New()
	relLow, lowCtx, err := s.Admit(context.Background(), "b", "local", 1, "low", 1, true, queueStage)
	if err != nil {
		t.Fatal(err)
	}

	admitted := make(chan func(...Done), 1)
	go func() {
		rel, _, err := s.Admit(context.Background(), "b", "local", 1, "high", 10, false, preemptStage)
		if err != nil {
			t.Errorf("preempt admit: %v", err)
			return
		}
		admitted <- rel
	}()

	select {
	case <-lowCtx.Done():
		if !errors.Is(context.Cause(lowCtx), ErrPreempted) {
			t.Errorf("cause = %v, want ErrPreempted", context.Cause(lowCtx))
		}
	case <-time.After(time.Second):
		t.Fatal("low request was not preempted")
	}

	relLow() // victim finishes after the cooperative cancel → frees the slot
	select {
	case rel := <-admitted:
		rel()
	case <-time.After(2 * time.Second):
		t.Fatal("high request never admitted after preemption")
	}
}

// TestPreemptNoVictimSpills: nothing interruptible to take → honor `then: fallThrough`.
func TestPreemptNoVictimSpills(t *testing.T) {
	s := New()
	rel, _, _ := s.Admit(context.Background(), "b", "local", 1, "low", 1, false, queueStage) // non-interruptible
	defer rel()
	_, _, err := s.Admit(context.Background(), "b", "local", 1, "high", 10, false,
		config.Stage{Preempt: true, Then: "fallThrough"})
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "spill" {
		t.Fatalf("want spill, got %v", err)
	}
}

// TestPreemptNoVictimRejects: no victim and no follow-up verb → terminal reject.
func TestPreemptNoVictimRejects(t *testing.T) {
	s := New()
	rel, _, _ := s.Admit(context.Background(), "b", "local", 1, "low", 1, false, queueStage)
	defer rel()
	_, _, err := s.Admit(context.Background(), "b", "local", 1, "high", 10, false, preemptStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("want rejected, got %v", err)
	}
}

// fixedClock returns a scheduler whose clock the test controls.
func fixedClock(cfg *config.Config, t *time.Time) *Scheduler {
	s := NewWithConfig(cfg)
	s.now = func() time.Time { return *t }
	return s
}

// TestLimitRequestsBudget: a per-group requests budget admits up to the cap
// within the window, then rejects "over-budget", and frees once the window rolls.
func TestLimitRequestsBudget(t *testing.T) {
	cfg := &config.Config{PriorityGroups: map[string]config.PriorityGroup{
		"g": {Weight: 1, Limits: map[string]string{"requests": "2/min"},
			OnSaturated: map[string]config.Stage{"default": {Reject: true}}},
	}}
	clk := time.Unix(1000, 0)
	s := fixedClock(cfg, &clk)
	stage := config.Stage{Reject: true}

	for i := 0; i < 2; i++ {
		r, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, stage)
		if err != nil {
			t.Fatalf("admit %d under budget: %v", i, err)
		}
		r() // release; the requests budget is charged at admit regardless
	}
	_, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, stage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "over-budget" {
		t.Fatalf("3rd request: want over-budget, got %v", err)
	}
	if bp.RetryAfter <= 0 {
		t.Errorf("over-budget retry-after = %s, want > 0", bp.RetryAfter)
	}

	clk = clk.Add(61 * time.Second) // window rolls
	if r, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, stage); err != nil {
		t.Fatalf("after window: %v", err)
	} else {
		r()
	}
}

// TestLimitCostBudget: a cost budget is charged at release from the reported
// Done, and trips once cumulative $ within the window reaches the cap.
func TestLimitCostBudget(t *testing.T) {
	cfg := &config.Config{PriorityGroups: map[string]config.PriorityGroup{
		"g": {Weight: 1, Limits: map[string]string{"cost": "$1/min"},
			OnSaturated: map[string]config.Stage{"default": {Reject: true}}},
	}}
	clk := time.Unix(1000, 0)
	s := fixedClock(cfg, &clk)
	stage := config.Stage{Reject: true}

	for i := 0; i < 2; i++ {
		r, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, stage)
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
		r(Done{CostUSD: 0.6}) // 2 × $0.6 = $1.2 ≥ $1 budget
	}
	_, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, stage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "over-budget" {
		t.Fatalf("want over-budget after cost cap, got %v", err)
	}
}

// TestLimitSpillsWhenStageAllows: an over-budget request whose stage spills
// advances to the next backend rather than backing off.
func TestLimitSpillsWhenStageAllows(t *testing.T) {
	cfg := &config.Config{PriorityGroups: map[string]config.PriorityGroup{
		"g": {Weight: 1, Limits: map[string]string{"requests": "1/min"}},
	}}
	clk := time.Unix(1000, 0)
	s := fixedClock(cfg, &clk)

	r, _, err := s.Admit(context.Background(), "b", "local", 10, "g", 1, false, config.Stage{Spill: true})
	if err != nil {
		t.Fatal(err)
	}
	r()
	_, _, err = s.Admit(context.Background(), "b", "local", 10, "g", 1, false, config.Stage{Spill: true})
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "spill" {
		t.Fatalf("over-budget with spill stage: want spill, got %v", err)
	}
}

// TestLimitRequestsBudgetViaQueue: a request admitted through the queue (promote
// path) is still charged against the requests budget, so the cap holds under
// capacity contention.
func TestLimitRequestsBudgetViaQueue(t *testing.T) {
	cfg := &config.Config{PriorityGroups: map[string]config.PriorityGroup{
		"g": {Weight: 1, Limits: map[string]string{"requests": "2/min"}},
	}}
	clk := time.Unix(1000, 0)
	s := fixedClock(cfg, &clk)
	stage := config.Stage{Queue: true}

	r1, _, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, stage) // direct grant, charged → 1
	if err != nil {
		t.Fatal(err)
	}

	admitted := make(chan func(...Done), 1)
	go func() {
		r2, _, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, stage) // queues (cap full)
		if err != nil {
			t.Errorf("queued admit: %v", err)
			return
		}
		admitted <- r2
	}()
	time.Sleep(100 * time.Millisecond) // let it enter the queue
	r1()                               // promote grants the queued request → charged → 2
	r2 := <-admitted

	// Budget is now spent (2/2) — a 3rd request trips over-budget even though the
	// freed slot would otherwise admit it.
	_, _, err = s.Admit(context.Background(), "b", "local", 1, "g", 1, false, stage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "over-budget" {
		t.Fatalf("queued admission not charged: want over-budget, got %v", err)
	}
	r2()
}

// TestShareCurrencyMixedFallsBackToRequests: when a backend's queued groups
// disagree on currency, comparison falls back to request-count so no group is
// starved (a requests group holding more slots is correctly deprioritized).
func TestShareCurrencyMixedFallsBackToRequests(t *testing.T) {
	now := time.Unix(1000, 0)
	bs := &backendState{capacity: 1,
		groupActive: map[string]int{"req": 3}, // requests group already holds 3 slots
		share:       map[string]float64{"dwl": 100},
		shareAt:     map[string]time.Time{"dwl": now}}
	bs.waiters = []*waiter{
		{slot: &slot{group: "req", weight: 1, currency: "requests"}, ready: make(chan struct{})},
		{slot: &slot{group: "dwl", weight: 1, currency: "dwell"}, ready: make(chan struct{})},
	}
	// Mixed currencies → request-count: req=3 in-flight vs dwl=0 → pick dwl.
	// (Under a naive cross-currency min, dwl's share of 100 would lose to req's 3.)
	if idx := bs.pickWaiter(now); bs.waiters[idx].slot.group != "dwl" {
		t.Fatalf("mixed currency starved the dwell group; picked %q, want dwl",
			bs.waiters[idx].slot.group)
	}
}

// TestShareCurrencyDwellPrefersLighter: under a dwell share currency, the group
// with less recent accumulated dwell is promoted first, even at equal weight.
func TestShareCurrencyDwellPrefersLighter(t *testing.T) {
	now := time.Unix(1000, 0)
	bs := &backendState{capacity: 1, groupActive: map[string]int{},
		share: map[string]float64{"heavy": 50}, shareAt: map[string]time.Time{"heavy": now}}
	bs.waiters = []*waiter{
		{slot: &slot{group: "heavy", weight: 1, currency: "dwell"}, ready: make(chan struct{})},
		{slot: &slot{group: "light", weight: 1, currency: "dwell"}, ready: make(chan struct{})},
	}
	if idx := bs.pickWaiter(now); bs.waiters[idx].slot.group != "light" {
		t.Fatalf("picked %q, want light (lower dwell share)", bs.waiters[idx].slot.group)
	}
}

// TestShareDecay: the accumulator halves after one half-life.
func TestShareDecay(t *testing.T) {
	now := time.Unix(1000, 0)
	bs := &backendState{share: map[string]float64{"g": 80}, shareAt: map[string]time.Time{"g": now}}
	got := bs.decayedShare(now.Add(shareHalfLife), "g")
	if got < 39.9 || got > 40.1 {
		t.Errorf("decayed share = %v, want ~40", got)
	}
}

// TestShareCurrencyFromConfig: a dwell-currency group's release folds measured
// dwell into the accumulator.
func TestShareCurrencyFromConfig(t *testing.T) {
	cfg := &config.Config{PriorityGroups: map[string]config.PriorityGroup{
		"g": {Weight: 1, ShareCurrency: "dwell"},
	}}
	clk := time.Unix(1000, 0)
	s := fixedClock(cfg, &clk)

	r, _, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, config.Stage{Queue: true})
	if err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(5 * time.Second) // 5s of dwell
	r()
	bs := s.backends["b"]
	if got := bs.decayedShare(clk, "g"); got < 4.9 || got > 5.1 {
		t.Errorf("dwell share = %v, want ~5s", got)
	}
}

// TestPreemptSkipsEqualWeight: preemption only targets strictly lower-weight
// groups, even when the holder is interruptible.
func TestPreemptSkipsEqualWeight(t *testing.T) {
	s := New()
	rel, _, _ := s.Admit(context.Background(), "b", "local", 1, "a", 5, true, queueStage)
	defer rel()
	_, _, err := s.Admit(context.Background(), "b", "local", 1, "z", 5, false, preemptStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("equal-weight must not be preempted; want rejected, got %v", err)
	}
}
