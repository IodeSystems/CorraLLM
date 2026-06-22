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

func TestAdmitUpToCapacity(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, err := s.Admit(ctx, "b", 2, "g", 1, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Admit(ctx, "b", 2, "g", 1, rejectStage)
	if err != nil {
		t.Fatal(err)
	}
	// Third over capacity with reject stage → BackpressureError now.
	_, err = s.Admit(ctx, "b", 2, "g", 1, rejectStage)
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
	r3, err := s.Admit(ctx, "b", 2, "g", 1, rejectStage)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
	r3()
}

func TestQueueWaitsThenAdmits(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, _ := s.Admit(ctx, "b", 1, "g", 1, queueStage)

	admitted := make(chan struct{})
	go func() {
		r2, err := s.Admit(ctx, "b", 1, "g", 1, queueStage)
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
	r1, _ := s.Admit(context.Background(), "b", 1, "g", 1, queueStage)
	defer r1()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.Admit(ctx, "b", 1, "g", 1, queueStage)
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
		bs.waiters = append(bs.waiters, &waiter{group: "hi", weight: 3, ready: make(chan struct{})})
	}
	for range 10 {
		bs.waiters = append(bs.waiters, &waiter{group: "lo", weight: 1, ready: make(chan struct{})})
	}
	bs.promote() // fills all 4 slots by min(active/weight)

	if bs.groupActive["hi"] != 3 || bs.groupActive["lo"] != 1 {
		t.Fatalf("weighted share: hi=%d lo=%d, want 3 and 1", bs.groupActive["hi"], bs.groupActive["lo"])
	}
	if bs.active != 4 || len(bs.waiters) != 16 {
		t.Errorf("active=%d waiters=%d, want 4 and 16", bs.active, len(bs.waiters))
	}
}
