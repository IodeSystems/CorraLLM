package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A per-model queue bound has to survive the database, or it vanishes at the
// next save and the model quietly goes back to the box-wide depth — a config
// that reverts itself being strictly worse than one that was never accepted.
//
// The round trip IS the port (see roundtrip_test.go): a normalized schema is a
// hand-written mapping, and a mapping with a missing field loses data silently.
func TestPerModelSchedulerSurvivesTheDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	in := &config.Config{
		Scheduler: config.SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8},
		Models: map[string]config.Model{
			"slow":  {MaxConcurrent: 1, Scheduler: &config.SchedulerConfig{MaxQueueDepth: 1}},
			"plain": {MaxConcurrent: 4},
		},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
	}
	if err := Write(ctx, db, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	slow, ok := out.Models["slow"]
	if !ok {
		t.Fatal("the model itself did not survive")
	}
	if slow.Scheduler == nil {
		t.Fatal("per-model scheduler was dropped by the round trip")
	}
	if slow.Scheduler.MaxQueueDepth != 1 {
		t.Errorf("depth = %d, want 1", slow.Scheduler.MaxQueueDepth)
	}
	// A model without an override must come back WITHOUT one, not with a zeroed
	// struct: nil and &{} resolve the same today, but a materialized empty
	// override is a lie about what the operator wrote.
	if plain := out.Models["plain"]; plain.Scheduler != nil {
		t.Errorf("plain gained an override it never had: %+v", plain.Scheduler)
	}
	// And the box-wide setting is untouched by any of it.
	if out.Scheduler.MaxQueueDepth != 8 || out.Scheduler.MaxWait != "15s" {
		t.Errorf("box-wide scheduler = %+v, want it unchanged", out.Scheduler)
	}
	// The resolver has to agree after the trip, which is the property callers use.
	if got := out.SchedulerFor("slow"); got.MaxQueueDepth != 1 || got.MaxWait != "15s" {
		t.Errorf("resolved slow = %+v, want depth 1 with the inherited wait", got)
	}
	if got := out.SchedulerFor("plain"); got.MaxQueueDepth != 8 {
		t.Errorf("resolved plain = %+v, want the box-wide depth", got)
	}
}
