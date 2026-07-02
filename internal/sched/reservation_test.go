package sched

import (
	"context"
	"testing"
	"time"
)

// batch fills only the unreserved slots; the reserved slot stays free and the
// reserving (interactive) lane takes it without preempting.
func TestReservationGatesBatchFreesForLane(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.Reserve("b", "interactive", 1, time.Minute) // hold 1 of 2 for interactive

	relB, _, err := s.Admit(ctx, "b", "local", 2, "batch", 1, false, rejectStage)
	if err != nil {
		t.Fatalf("batch #1 should admit (effCap 1): %v", err)
	}
	if _, _, err := s.Admit(ctx, "b", "local", 2, "batch", 1, false, rejectStage); err == nil {
		t.Fatal("batch #2 should be rejected — the reserved slot is held free")
	}
	relI, _, err := s.Admit(ctx, "b", "local", 2, "interactive", 10, false, rejectStage)
	if err != nil {
		t.Fatalf("interactive should get the reserved slot immediately: %v", err)
	}
	relI()
	relB()
}

// a reservation frees on expiry; batch then fills the whole backend.
func TestReservationExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	s := fixedClock(nil, &now)
	ctx := context.Background()
	s.Reserve("b", "interactive", 1, 30*time.Second)

	relB, _, _ := s.Admit(ctx, "b", "local", 2, "batch", 1, false, rejectStage)
	if _, _, err := s.Admit(ctx, "b", "local", 2, "batch", 1, false, rejectStage); err == nil {
		t.Fatal("reserved slot should block batch #2 before expiry")
	}
	now = now.Add(31 * time.Second)
	s.reapExpired()

	relB2, _, err := s.Admit(ctx, "b", "local", 2, "batch", 1, false, rejectStage)
	if err != nil {
		t.Fatalf("after expiry batch should fill both slots: %v", err)
	}
	relB()
	relB2()
}

// a queued batch request is NOT promoted into a reserved slot when one frees, but
// IS admitted once the reservation expires.
func TestReservationPromoteGating(t *testing.T) {
	now := time.Unix(1000, 0)
	s := fixedClock(nil, &now)
	ctx := context.Background()
	s.Reserve("b", "interactive", 2, time.Minute) // reserve BOTH slots

	done := make(chan error, 1)
	go func() {
		_, _, err := s.Admit(ctx, "b", "local", 2, "batch", 1, false, queueStage)
		done <- err
	}()
	waitQueued(t, s, "b", 1) // batch is queued (effCap 0)

	relI, _, err := s.Admit(ctx, "b", "local", 2, "interactive", 10, false, rejectStage)
	if err != nil {
		t.Fatalf("interactive should admit under its own reservation: %v", err)
	}
	relI() // promote runs, but batch is still reserved out → stays queued

	select {
	case err := <-done:
		t.Fatalf("batch should NOT have been admitted while reserved (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	now = now.Add(2 * time.Minute)
	s.reapExpired() // reservation gone → promote → batch granted
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("batch should be admitted after expiry: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not admitted after the reservation expired")
	}
}

// renewal extends the lease; the ttl is capped at the max.
func TestReservationRenewAndCap(t *testing.T) {
	now := time.Unix(1000, 0)
	s := fixedClock(nil, &now)
	s.SetMaxReservationTTL(5 * time.Minute)

	exp1 := s.Reserve("b", "g", 1, time.Minute)
	now = now.Add(30 * time.Second)
	exp2 := s.Reserve("b", "g", 1, time.Minute) // heartbeat → later expiry
	if !exp2.After(exp1) {
		t.Errorf("renew should push expiry out: %v !> %v", exp2, exp1)
	}
	// ttl over the cap is clamped
	capped := s.Reserve("b", "g", 1, time.Hour)
	if want := now.Add(5 * time.Minute); !capped.Equal(want) {
		t.Errorf("ttl should cap at 5m: got %v want %v", capped, want)
	}
}

func waitQueued(t *testing.T, s *Scheduler, backend string, n int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		for _, bl := range s.Snapshot().Backends {
			if bl.Backend == backend && bl.Waiting >= n {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiter did not queue on %q", backend)
}
