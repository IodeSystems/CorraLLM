package config

import "testing"

func cfgFor(global SchedulerConfig, per map[string]*SchedulerConfig) *Config {
	models := map[string]Model{}
	for name, sc := range per {
		models[name] = Model{Scheduler: sc}
	}
	return &Config{Scheduler: global, Models: models}
}

// A model with no override is the common case and must be exactly the box-wide
// setting — not a zero value, which would silently unbound it.
func TestSchedulerForInheritsWhenUnset(t *testing.T) {
	c := cfgFor(SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8}, map[string]*SchedulerConfig{
		"plain": nil,
	})
	got := c.SchedulerFor("plain")
	if got.MaxWait != "15s" || got.MaxQueueDepth != 8 {
		t.Errorf("got %+v, want the box-wide bounds", got)
	}
	if got := c.SchedulerFor("never-heard-of-it"); got.MaxQueueDepth != 8 {
		t.Errorf("unknown model = %+v, want the box-wide bounds", got)
	}
}

// The whole point: one model's depth changes and no other model moves.
func TestSchedulerForOverridesOneFieldOnly(t *testing.T) {
	c := cfgFor(SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8}, map[string]*SchedulerConfig{
		"slow":  {MaxQueueDepth: 1},
		"plain": nil,
	})
	slow := c.SchedulerFor("slow")
	if slow.MaxQueueDepth != 1 {
		t.Errorf("slow depth = %d, want its own 1", slow.MaxQueueDepth)
	}
	// Setting a depth must not drop the wait — the trap a whole-struct override
	// would fall into.
	if slow.MaxWait != "15s" {
		t.Errorf("slow maxWait = %q, want the inherited 15s", slow.MaxWait)
	}
	if plain := c.SchedulerFor("plain"); plain.MaxQueueDepth != 8 {
		t.Errorf("an untouched model moved: %+v", plain)
	}
}

// Zero means INHERIT here, not unbounded. A model that wants no practical bound
// sets a number it cannot reach.
func TestSchedulerForTreatsZeroAsInherit(t *testing.T) {
	c := cfgFor(SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8}, map[string]*SchedulerConfig{
		"empty": {},
	})
	got := c.SchedulerFor("empty")
	if got.MaxQueueDepth != 8 || got.MaxWait != "15s" {
		t.Errorf("got %+v, want both inherited", got)
	}
}
