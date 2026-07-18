package proc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// TestCapacityTransientCarriesRetryAfter: a load blocked only by a resident
// sitting inside its activeUse window is TRANSIENT — the resident becomes a
// legal victim when the window expires, so the error must say when. The request
// edge turns this into 429 + Retry-After; reporting it as a bare 503 made every
// cold load inside the client's short 5xx retry budget unretryable.
func TestCapacityTransientCarriesRetryAfter(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := resConfig(t, "10", "6", portA, portB) // 6+6 > 10 → mutually exclusive
	mgr := NewManager(cfg)
	mgr.activeUse = 30 * time.Second // wide window so the block is unambiguous
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	_, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	doneA() // refs → 0, but lastUsed is NOW: protected by activeUse

	_, _, _, err = mgr.EnsureReady(ctx, "B", cfg.Models["B"], nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CapacityError, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("must still satisfy errors.Is(ErrNoCapacity) for existing callers")
	}
	if ce.Permanent {
		t.Errorf("A is evictable once its window expires — must be transient, not permanent")
	}
	// A was just used, so the wait is ~the full window. Allow slack for the
	// health-check time already burned.
	if ce.RetryAfter <= 0 || ce.RetryAfter > 30*time.Second {
		t.Errorf("RetryAfter = %s, want (0, 30s]", ce.RetryAfter)
	}
	if len(ce.Blocking) != 1 || ce.Blocking[0] != "A" {
		t.Errorf("Blocking = %v, want [A]", ce.Blocking)
	}
}

// TestCapacityPermanentWhenOversized: a model whose declared usage exceeds the
// pool budget outright can never fit, no matter what is evicted. That is an
// operator-visible fault, not contention — it must NOT be dressed up as
// retryable backpressure, or a client will spin on it until its budget runs out.
func TestCapacityPermanentWhenOversized(t *testing.T) {
	port := listenTCP(t)
	cfg := &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"big": resModel(t, "box", map[string]string{"gpu": "99"}, port), // > budget
		},
	}
	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	_, _, _, err := mgr.EnsureReady(context.Background(), "big", cfg.Models["big"], nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CapacityError, got %T: %v", err, err)
	}
	if !ce.Permanent {
		t.Errorf("99 > budget 10 with nothing else resident: must be permanent")
	}
	if ce.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 — waiting cannot help a permanent miss", ce.RetryAfter)
	}
}

// TestCapacityPermanentEvenIfEverythingEvicted: the permanent test above has an
// empty server. This one has a resident that COULD be evicted and still leaves
// the incoming model unable to fit — the classification must consider the
// fully-evicted state, not just the current one.
func TestCapacityPermanentEvenIfEverythingEvicted(t *testing.T) {
	portA, portB := listenTCP(t), listenTCP(t)
	cfg := &config.Config{
		Servers: map[string]config.Server{"box": {Pools: map[string]string{"gpu": "10"}}},
		Models: map[string]config.Model{
			"A":   resModel(t, "box", map[string]string{"gpu": "4"}, portA),
			"big": resModel(t, "box", map[string]string{"gpu": "20"}, portB), // > budget alone
		},
	}
	mgr := NewManager(cfg)
	mgr.activeUse = 0 // A is freely evictable — and it still won't be enough
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	_, doneA, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	doneA()

	_, _, _, err = mgr.EnsureReady(ctx, "big", cfg.Models["big"], nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CapacityError, got %T: %v", err, err)
	}
	if !ce.Permanent {
		t.Errorf("evicting A frees 4 of the 20 needed against a budget of 10: still permanent")
	}
}
