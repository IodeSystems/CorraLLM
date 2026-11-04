package sched

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// One model's queue bound must not move any other model's.
//
// This is the whole reason the override exists: the depth a queue can actually
// reach is capacity × maxWait / meanService — one model's arithmetic — so a box
// with a slow model and fast ones has no single correct global depth. The
// dashboard could diagnose that clash and could only offer prose about it,
// because every fix it could name was box-wide.
func TestAModelsQueueBoundIsItsOwn(t *testing.T) {
	cfg := &config.Config{
		Scheduler: config.SchedulerConfig{MaxWait: "50ms", MaxQueueDepth: 8},
		Models: map[string]config.Model{
			"slow":  {Scheduler: &config.SchedulerConfig{MaxQueueDepth: 1}},
			"plain": {},
		},
	}
	s := NewWithConfig(cfg)

	if got, want := depthOf(s, "slow"), 1; got != want {
		t.Errorf("slow depth = %d, want %d", got, want)
	}
	if got, want := depthOf(s, "plain"), 8; got != want {
		t.Errorf("plain depth = %d, want the box-wide %d", got, want)
	}
	// Setting only the depth must leave the wait inherited.
	if w, _ := s.boundsFor("slow"); w != 50*time.Millisecond {
		t.Errorf("slow maxWait = %v, want the inherited 50ms", w)
	}
}

func depthOf(s *Scheduler, model string) int {
	_, d := s.boundsFor(model)
	return d
}

// And it has to BIND: a model whose depth is 1 rejects the second waiter while
// the box-wide bound would still have let seven more in.
func TestTheOverriddenDepthActuallyRejects(t *testing.T) {
	cfg := &config.Config{
		Scheduler: config.SchedulerConfig{MaxWait: "2s", MaxQueueDepth: 8},
		Models:    map[string]config.Model{"slow": {Scheduler: &config.SchedulerConfig{MaxQueueDepth: 1}}},
	}
	s := NewWithConfig(cfg)
	ctx := context.Background()

	// Fill the single slot.
	rel, _, err := s.Admit(ctx, "slow", "chat", 1, "default", 1, false, config.Stage{})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	defer rel()

	// One waiter is allowed (depth 1).
	queued := make(chan error, 1)
	go func() {
		_, _, err := s.Admit(ctx, "slow", "chat", 1, "default", 1, false, config.Stage{})
		queued <- err
	}()
	// Give the waiter time to enter the queue before the third arrives.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.backends["slow"].waiters)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// The next one must be refused rather than queued: depth is 1, not 8.
	if _, _, err := s.Admit(ctx, "slow", "chat", 1, "default", 1, false, config.Stage{}); err == nil {
		t.Error("third request was admitted or queued; the model's depth of 1 did not bind")
	}
	rel()
	<-queued
}

// A provider gate admits a request its model slot already admitted: it reads the
// provider's queue bounds and does not charge the group's budget a second time.
func TestProviderGateTakesItsBoundsAndChargesNothing(t *testing.T) {
	cfg := &config.Config{
		Scheduler: config.SchedulerConfig{MaxQueueDepth: 8},
		Extensions: map[string]config.Extension{"corp": {Providers: map[string]config.Provider{
			"gw": {MaxConcurrent: 1, Scheduler: &config.SchedulerConfig{MaxQueueDepth: 2, MaxWait: "40ms"}},
		}}},
		PriorityGroups: map[string]config.PriorityGroup{"g": {Weight: 1, Limits: map[string]string{"requests": "1/min"}}},
	}
	s := NewWithConfig(cfg)
	gate := config.ProviderGateKey("corp", "gw")
	if w, d := s.boundsFor(gate); d != 2 || w != 40*time.Millisecond {
		t.Errorf("gate bounds = %v/%d, want the provider's 40ms/2", w, d)
	}
	ctx := context.Background()
	rel, _, err := s.Admit(ctx, "m", "chat", 1, "g", 1, false, config.Stage{})
	if err != nil {
		t.Fatal(err)
	}
	// The group's one request a minute is spent on the model slot; the gate
	// must not refuse it as a second request.
	grel, _, err := s.Admit(ctx, gate, "chat", 1, "g", 1, false, config.Stage{})
	if err != nil {
		t.Fatalf("gate refused a request the model admitted: %v", err)
	}
	grel()
	rel()
}

// A passthrough model lives in the discovered overlay, not cfg.Models; its
// template's queue bound must still apply.
func TestADiscoveredModelsQueueBoundApplies(t *testing.T) {
	cfg := &config.Config{Scheduler: config.SchedulerConfig{MaxQueueDepth: 8}}
	cfg.SetDiscoveredFor("gw", config.DefaultCredentialName, map[string]config.Model{
		"gw-x": {Scheduler: &config.SchedulerConfig{MaxQueueDepth: 3}},
	})
	if got := depthOf(NewWithConfig(cfg), "gw-x"); got != 3 {
		t.Errorf("depth = %d, want the template's 3", got)
	}
}
