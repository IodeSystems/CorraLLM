package lease_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/lease/leasetest"
)

func req() lease.Request {
	return lease.Request{Extension: "colmap", Job: "recon_1", Stage: "match", Owner: "o", Usage: map[string]string{"gpu1": "3GB"}, Yieldable: true}
}

func TestClientContract(t *testing.T) {
	s := leasetest.New("tok")
	defer s.Close()
	c := &lease.Client{Base: s.URL, Token: "tok"}
	ctx := context.Background()

	g, err := c.Acquire(ctx, req())
	if err != nil || g.ID == "" || g.ProtectedUntil <= g.GrantedAt {
		t.Fatalf("grant %+v %v", g, err)
	}
	st, err := c.Status(ctx, g.ID, 50*time.Millisecond)
	if err != nil || st.State != lease.StateHeld {
		t.Fatalf("status %+v %v", st, err)
	}
	s.RequestYield("", 30*time.Second)
	st, err = c.Status(ctx, g.ID, 5*time.Second)
	if err != nil || st.State != lease.StateYield || st.Deadline == 0 || st.WantedBy == "" {
		t.Fatalf("yield status %+v %v", st, err)
	}
	if err := c.Release(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx, g.ID, 0); !errors.Is(err, lease.ErrNotFound) {
		t.Fatalf("after release: %v", err)
	}
	if err := c.Release(ctx, g.ID); err != nil {
		t.Fatalf("release of an unknown lease is not an error: %v", err)
	}

	s.Busy(1)
	_, err = c.Acquire(ctx, req())
	var be *lease.BusyError
	if !errors.As(err, &be) || be.RetryAfter != time.Second || len(be.Blocking) != 1 {
		t.Fatalf("busy: %v", err)
	}
	s.Conflict(true)
	r := req()
	r.Held = true
	if _, err := c.Acquire(ctx, r); !errors.Is(err, lease.ErrConflict) {
		t.Fatalf("held conflict: %v", err)
	}

	bad := &lease.Client{Base: s.URL, Token: "wrong"}
	var fe *lease.FatalError
	if _, err := bad.Acquire(ctx, req()); !errors.As(err, &fe) || fe.Status != 401 {
		t.Fatalf("bad token: %v", err)
	}
}

func TestHoldRetriesBusyAndReportsWaiting(t *testing.T) {
	s := leasetest.New("tok")
	defer s.Close()
	s.Busy(2)
	var waits []bool
	h, err := lease.Hold(context.Background(), &lease.Client{Base: s.URL, Token: "tok"}, req(), lease.Options{
		Poll: 100 * time.Millisecond, OnWait: func(w bool, _ string) { waits = append(waits, w) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if len(waits) < 2 || !waits[0] || waits[len(waits)-1] {
		t.Fatalf("waiting reports %v: want true… then false", waits)
	}
	var busy int
	for _, e := range s.Events() {
		if e.Kind == "busy" {
			busy++
		}
	}
	if busy != 2 {
		t.Fatalf("busy answers %d", busy)
	}
}

func TestHoldGivesUp(t *testing.T) {
	s := leasetest.New("tok")
	defer s.Close()
	s.Busy(100)
	_, err := lease.Hold(context.Background(), &lease.Client{Base: s.URL, Token: "tok"}, req(), lease.Options{MaxWait: 500 * time.Millisecond})
	if !errors.Is(err, lease.ErrGaveUp) {
		t.Fatalf("want ErrGaveUp, got %v", err)
	}
}

func TestHoldYieldAndReregistration(t *testing.T) {
	s := leasetest.New("tok")
	defer s.Close()
	c := &lease.Client{Base: s.URL, Token: "tok"}
	h, err := lease.Hold(context.Background(), c, req(), lease.Options{Poll: 100 * time.Millisecond, Backoff: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	first := h.ID()
	// corrallm restart: the lease is forgotten; the watcher re-registers with held:true.
	s.Forget()
	deadline := time.Now().Add(3 * time.Second)
	for h.ID() == first && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.ID() == first {
		t.Fatal("no re-registration after the lease was forgotten")
	}
	held := false
	for _, e := range s.Events() {
		held = held || (e.Kind == "held-grant" && e.Held)
	}
	if !held {
		t.Fatalf("re-registration was not held:true: %+v", s.Events())
	}
	t0 := time.Now()
	s.RequestYield("", 30*time.Second)
	select {
	case y := <-h.Yield():
		if time.Since(t0) > time.Second || y.Deadline.Before(time.Now()) {
			t.Fatalf("yield seen after %v, deadline %v", time.Since(t0), y.Deadline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("yield not delivered")
	}
	h.Release()
	if len(s.Held()) != 0 {
		t.Fatalf("still held: %v", s.Held())
	}

	// A held re-registration that does not fit (409) is a yield at once.
	h, err = lease.Hold(context.Background(), c, req(), lease.Options{Poll: 100 * time.Millisecond, Backoff: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.Conflict(true)
	s.Forget()
	select {
	case y := <-h.Yield():
		if y.WantedBy == "" {
			t.Fatal("conflict yield has no reason")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("409 on re-registration did not yield")
	}
	h.Release()
}
