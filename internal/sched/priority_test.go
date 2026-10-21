package sched

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// queueLen reads a backend's queue length under the lock.
func queueLen(s *Scheduler, backend string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bs := s.backends[backend]; bs != nil {
		return len(bs.waiters)
	}
	return 0
}

type admitted struct {
	name    string
	release func(...Done)
}

// enqueue admits in the background and reports the grant on out.
func enqueue(s *Scheduler, ctx context.Context, name string, out chan<- admitted) {
	go func() {
		rel, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
		if err != nil {
			out <- admitted{name: name + ":" + err.Error()}
			return
		}
		out <- admitted{name: name, release: rel}
	}()
}

// TestPriorityHeadsTheQueue: priority waiters are granted before normal waiters
// that arrived earlier, FIFO among themselves; normal waiters keep their order.
func TestPriorityHeadsTheQueue(t *testing.T) {
	s := New()
	hold, _, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan admitted, 4)
	pri := WithPriority(context.Background())
	enqueue(s, context.Background(), "n1", out)
	waitQueued(t, s, "b", 1)
	enqueue(s, context.Background(), "n2", out)
	waitQueued(t, s, "b", 2)
	enqueue(s, pri, "p1", out)
	waitQueued(t, s, "b", 3)
	enqueue(s, pri, "p2", out)
	waitQueued(t, s, "b", 4)

	hold()
	var got []string
	for range 4 {
		select {
		case a := <-out:
			got = append(got, a.name)
			if a.release != nil {
				a.release()
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("stalled after %v", got)
		}
	}
	want := []string{"p1", "p2", "n1", "n2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grant order = %v, want %v", got, want)
		}
	}
}

// TestPriorityIgnoresNormalWaitersForDepth: a full queue of normal callers does
// not refuse a priority arrival and loses none of its members to it; only
// priority waiters count against the depth for a priority arrival.
func TestPriorityIgnoresNormalWaitersForDepth(t *testing.T) {
	s := NewWithConfig(&config.Config{Scheduler: config.SchedulerConfig{MaxQueueDepth: 2}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	out := make(chan admitted, 8)
	enqueue(s, ctx, "n1", out)
	enqueue(s, ctx, "n2", out)
	waitQueued(t, s, "b", 2)

	pri := WithPriority(ctx)
	enqueue(s, pri, "p1", out)
	enqueue(s, pri, "p2", out)
	waitQueued(t, s, "b", 4) // both priority arrivals queued past a full normal queue

	// Priority depth is now full: a third priority arrival is refused.
	_, _, err = s.Admit(pri, "b", "local", 1, "g", 1, false, queueStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("third priority arrival: want rejected, got %v", err)
	}
	// A normal arrival still sees the full queue.
	_, _, err = s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("normal arrival: want rejected, got %v", err)
	}
	select {
	case a := <-out:
		t.Fatalf("a waiter left the queue: %s", a.name)
	case <-time.After(50 * time.Millisecond):
	}
	if n := queueLen(s, "b"); n != 4 {
		t.Fatalf("queue length = %d, want 4", n)
	}
}

// TestNormalNeverBumpsPriority: an arrival that WOULD bump a normal waiter (an
// aged ticket in an idle group) is refused rather than bump a priority waiter.
func TestNormalNeverBumpsPriority(t *testing.T) {
	s := NewWithConfig(&config.Config{Scheduler: config.SchedulerConfig{MaxQueueDepth: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	out := make(chan admitted, 2)
	enqueue(s, WithPriority(ctx), "p1", out)
	waitQueued(t, s, "b", 1)

	aged := WithWaitingSince(ctx, time.Now().Add(-10*time.Minute))
	_, _, err = s.Admit(aged, "b", "local", 1, "other", 1, false, queueStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "rejected" {
		t.Fatalf("want rejected, got %v", err)
	}
	select {
	case a := <-out:
		t.Fatalf("priority waiter left the queue: %s", a.name)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPriorityDoesNotPreempt: a priority arrival under a preempting stage waits
// for the running request instead of canceling it. Priority is queue order only.
func TestPriorityDoesNotPreempt(t *testing.T) {
	s := New()
	_, holderCtx, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, true, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(WithPriority(context.Background()), 100*time.Millisecond)
	defer cancel()
	_, _, err = s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	var bp *BackpressureError
	if !errors.As(err, &bp) || bp.Reason != "queue-timeout" {
		t.Fatalf("want queue-timeout, got %v", err)
	}
	if holderCtx.Err() != nil {
		t.Fatal("running request was canceled by a priority arrival")
	}
}

// TestCallerCancelWhileQueuedIsNotBackpressure: a caller that hangs up while
// queued gets its own cancellation back, not a 429 queue-timeout. The live box
// logged bench requests the watchdog abandoned as 429 queue-timeout.
func TestCallerCancelWhileQueuedIsNotBackpressure(t *testing.T) {
	s := New()
	hold, _, _ := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, queueStage)
	defer hold()
	gone := errors.New("client went away")
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel(gone)
	}()
	_, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	var bp *BackpressureError
	if errors.As(err, &bp) {
		t.Fatalf("caller cancel reported as backpressure %q", bp.Reason)
	}
	if !errors.Is(err, gone) {
		t.Fatalf("want the cancel cause, got %v", err)
	}
	if n := queueLen(s, "b"); n != 0 {
		t.Fatalf("canceled waiter left in queue (%d)", n)
	}
}
