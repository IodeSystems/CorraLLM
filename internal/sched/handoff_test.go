package sched

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A draining scheduler admits nothing new, sends its queue away, and lets a
// granted slot finish — and its release does not hand the slot to anyone.
func TestDrain_RefusesQueuedAndNew(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() {
		_, _, err := s.Admit(ctx, "b", "local", 1, "g", 1, false, queueStage)
		queued <- err
	}()
	time.Sleep(50 * time.Millisecond)

	s.Drain()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrDraining) {
			t.Fatalf("queued request got %v, want ErrDraining", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request still waiting after Drain")
	}
	if _, _, err := s.Admit(ctx, "other", "local", 4, "g", 1, false, queueStage); !errors.Is(err, ErrDraining) {
		t.Fatalf("new request got %v, want ErrDraining", err)
	}
	if got := s.OwnActive()["b"]; got != 1 {
		t.Fatalf("own active = %d, want the granted slot", got)
	}
	r1()
	if got := s.OwnActive()["b"]; got != 0 {
		t.Fatalf("after release own active = %d, want 0 (nothing promoted)", got)
	}
}

// Foreign slots count against capacity: with the predecessor holding the only
// slot of a --parallel 1 backend, the successor queues until it is reported
// free, then gets it.
func TestForeign_HoldsCapacityUntilReleased(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.SetForeign(map[string]int{"swift": 1})
	got := make(chan func(...Done), 1)
	go func() {
		r, _, err := s.Admit(ctx, "swift", "local", 1, "g", 1, false, queueStage)
		if err != nil {
			t.Error(err)
			return
		}
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("admitted while the predecessor holds the only slot")
	case <-time.After(100 * time.Millisecond):
	}
	if snap := s.Snapshot(); snap.Backends[0].Active != 1 || snap.Backends[0].Waiting != 1 {
		t.Errorf("snapshot %+v: want the foreign slot active and ours waiting", snap.Backends[0])
	}
	s.SetForeign(nil) // predecessor finished
	select {
	case r := <-got:
		if a := s.OwnActive()["swift"]; a != 1 {
			t.Errorf("own active = %d", a)
		}
		r()
	case <-time.After(time.Second):
		t.Fatal("not admitted after the foreign slot was released")
	}
}

// A capacity-8 backend with 3 foreign slots admits 5 here, not 0 and not 8.
func TestForeign_PartialCapacity(t *testing.T) {
	s := New()
	s.SetForeign(map[string]int{"fw": 3})
	var rel []func(...Done)
	for i := 0; i < 5; i++ {
		r, _, err := s.Admit(context.Background(), "fw", "remote", 8, "g", 1, false, rejectStage)
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		rel = append(rel, r)
	}
	if _, _, err := s.Admit(context.Background(), "fw", "remote", 8, "g", 1, false, rejectStage); err == nil {
		t.Fatal("sixth admitted: 5 own + 3 foreign would overbook 8")
	}
	for _, r := range rel {
		r()
	}
}

// Under a hold nothing is admitted, so the successor cannot grant against an
// empty view before the predecessor's first report.
func TestHoldAdmission(t *testing.T) {
	s := New()
	s.HoldAdmission()
	done := make(chan struct{})
	go func() {
		r, _, err := s.Admit(context.Background(), "b", "local", 1, "g", 1, false, queueStage)
		if err == nil {
			r()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("admitted during the hold")
	case <-time.After(50 * time.Millisecond):
	}
	s.ReleaseAdmission()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still held after ReleaseAdmission")
	}
}

// The whole overlap in one process pair: a predecessor with capacity 1 and a
// successor that mirrors its occupancy. At no moment do the two hold more
// than one slot between them.
func TestHandoff_NeverOverbooks(t *testing.T) {
	old, nu := New(), New()
	ctx := context.Background()
	var held atomic.Int32
	var peak atomic.Int32
	run := func(r func(...Done)) {
		if n := held.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(5 * time.Millisecond)
		held.Add(-1)
		r()
	}
	// The predecessor is mid-request.
	r0, _, err := old.Admit(ctx, "m", "local", 1, "g", 1, false, queueStage)
	if err != nil {
		t.Fatal(err)
	}
	held.Add(1)
	peak.Store(1)
	nu.HoldAdmission()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := nu.Admit(ctx, "m", "local", 1, "g", 1, false, queueStage)
			if err != nil {
				t.Error(err)
				return
			}
			run(r)
		}()
	}
	// Handover: drain, report, release — the order serve() uses.
	old.Drain()
	nu.SetForeign(old.OwnActive())
	nu.ReleaseAdmission()
	time.Sleep(20 * time.Millisecond)
	held.Add(-1)
	r0()
	nu.SetForeign(old.OwnActive())
	wg.Wait()
	if p := peak.Load(); p > 1 {
		t.Errorf("peak concurrent = %d on a 1-slot backend", p)
	}
}
