package proc

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Resolution keeps a disabled model out of every ladder, so nothing ROUTED
// reaches the manager. What still reaches it are the callers that never went
// through resolution — an explicit /load, an extension load, boot preload — and
// EnsureReady is the one door all three come through.

func TestDisabledModelIsNotSpawned(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	m := cfg.Models["A"]
	m.Disabled = true
	cfg.Models["A"] = m

	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()
	ctx := context.Background()

	if _, err := mgr.LoadModel(ctx, "A"); err == nil {
		t.Error("an explicit load of a disabled model must be refused")
	}

	_, _, _, err := mgr.EnsureReady(ctx, "A", cfg.Models["A"], nil, nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("EnsureReady err = %v, want *CapacityError", err)
	}
	// Permanent, like a pause and unlike congestion: nothing about a disabled
	// model changes until somebody edits the config, so a walk must not retry
	// it and a caller must not be told to come back.
	if !ce.Permanent {
		t.Error("a disabled refusal must be permanent, not backpressure")
	}
	if !errors.Is(err, ErrNoCapacity) {
		t.Error("a disabled refusal should unwrap to ErrNoCapacity")
	}
	if got := mgr.Snapshot(); len(got.Models) != 0 {
		t.Errorf("a disabled model left something resident: %+v", got.Models)
	}
	// Its sibling is untouched: disabling is per config entry, not per box.
	if _, err := mgr.LoadModel(ctx, "B"); err != nil {
		t.Fatalf("disabling one model broke another: %v", err)
	}
}

// Disabled outranks pinned. A pin says WHERE this model should be when it is in
// the setup; disabling says it is not, so the pin is never reached — and boot
// stays quiet about it, because there is nothing to come back to.
func TestPreloadSkipsADisabledPinnedModel(t *testing.T) {
	cfg := resConfig(t, "10", "6", listenTCP(t), listenTCP(t))
	a := cfg.Models["A"]
	a.Persistent, a.Disabled = true, true
	cfg.Models["A"] = a
	b := cfg.Models["B"]
	b.Persistent = true
	cfg.Models["B"] = b

	mgr := NewManager(cfg)
	mgr.healthTimeout = 5 * time.Second
	defer mgr.Shutdown()

	mgr.Preload(context.Background())

	if isResident(mgr, "A") {
		t.Error("preload spawned a disabled model — a pin must not outrank being out of the setup")
	}
	if !isResident(mgr, "B") {
		t.Error("preload skipped the pinned model that is still in the setup")
	}
}

func isResident(mgr *Manager, name string) bool {
	for _, rm := range mgr.Snapshot().Models {
		if rm.Name == name || rm.ModelName == name {
			return true
		}
	}
	return false
}
