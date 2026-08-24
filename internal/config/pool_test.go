package config

import (
	"reflect"
	"strings"
	"testing"
)

// box1's real shape: two cards, bound to pools by UUID, plus host RAM.
func twoCardBox() *Config {
	return &Config{Servers: map[string]Server{
		"box1": {
			Pools: map[string]string{
				"gpu0":   "31654MiB",
				"gpu1":   "9877MiB",
				"system": "128GB",
			},
			Devices: map[string]string{
				"gpu0": "GPU-ee90af07-0882-d325-182e-87137ec6d47b",
				"gpu1": "GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544",
			},
		},
	}}
}

// THE REGRESSION THE SPLIT EXISTS FOR.
//
// Size became measurable, so models stopped declaring ramUsage. But ramUsage's
// KEYS were the only statement of WHICH card a model was on, so dropping it also
// dropped the placement — and the model silently fell back to the server-wide
// default pool. On box1 that charges a model actually running on gpu1 to gpu0:
// one budget inflates, the other looks free, and the scheduler places a second
// model onto memory that is already spoken for.
func TestPoolDeclaresPlacementWithoutAnySizeHint(t *testing.T) {
	c := twoCardBox()
	got := c.DevicePoolsNamedBy("box1", "gpu1", nil)
	if !reflect.DeepEqual(got, []string{"gpu1"}) {
		t.Fatalf("got %v, want [gpu1] — `pool:` alone must place a model", got)
	}
}

// Without a declared pool AND without ramUsage there is nothing to go on. The
// caller falls back to the server default; this function must not invent one.
func TestNoPoolAndNoRAMUsageNamesNothing(t *testing.T) {
	c := twoCardBox()
	if got := c.DevicePoolsNamedBy("box1", "", nil); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

// Back-compat: every config written before `pool:` existed keeps its meaning.
func TestRAMUsageKeysStillPlaceAModel(t *testing.T) {
	c := twoCardBox()
	got := c.DevicePoolsNamedBy("box1", "", map[string]string{"gpu1": "8GB"})
	if !reflect.DeepEqual(got, []string{"gpu1"}) {
		t.Fatalf("got %v, want [gpu1]", got)
	}
}

// An explicit declaration beats an inference drawn from the shape of a size
// hint. Otherwise `pool:` could not correct a stale ramUsage, which is the one
// thing an operator would reach for it to do.
func TestExplicitPoolWinsOverRAMUsageKeys(t *testing.T) {
	c := twoCardBox()
	got := c.DevicePoolsNamedBy("box1", "gpu1", map[string]string{"gpu0": "20GB"})
	if !reflect.DeepEqual(got, []string{"gpu1"}) {
		t.Fatalf("got %v, want [gpu1] — the declaration must win", got)
	}
}

// Host RAM is not a card. A `system` key in ramUsage was never a placement and
// must not become one now that a second field can name pools.
func TestSystemPoolIsNotAPlacement(t *testing.T) {
	c := twoCardBox()
	if got := c.DevicePoolsNamedBy("box1", "", map[string]string{"system": "40GB"}); len(got) != 0 {
		t.Fatalf("got %v, want none — `system` is host RAM", got)
	}
}

// A multi-GPU split has no single pool, and saying so is the point: one measured
// total cannot be divided back across cards, so the caller must decline to
// attribute it rather than charge the whole thing to either.
func TestSplitAcrossCardsStillNamesBoth(t *testing.T) {
	c := twoCardBox()
	got := c.DevicePoolsNamedBy("box1", "", map[string]string{"gpu0": "29GB", "gpu1": "6GB"})
	if !reflect.DeepEqual(got, []string{"gpu0", "gpu1"}) {
		t.Fatalf("got %v, want [gpu0 gpu1]", got)
	}
}

// A pool the server does not declare is ignored rather than returned. Validation
// rejects it at load, so reaching here means the config changed under a running
// daemon — and inventing a pool the ledger has no budget for is worse than
// falling back to what ramUsage says.
func TestUndeclaredPoolFallsBackInsteadOfInventing(t *testing.T) {
	c := twoCardBox()
	got := c.DevicePoolsNamedBy("box1", "gpu7", map[string]string{"gpu0": "20GB"})
	if !reflect.DeepEqual(got, []string{"gpu0"}) {
		t.Fatalf("got %v, want [gpu0]", got)
	}
}

func TestValidateRejectsAnUndeclaredPlacementPool(t *testing.T) {
	c := twoCardBox()
	err := c.validatePool("qwen", "", "box1", "gpu7")
	if err == nil || !strings.Contains(err.Error(), "not declared on server") {
		t.Fatalf("err = %v, want an undeclared-pool error naming the server", err)
	}
}

// A pool that EXISTS but holds host RAM is a category error, and it fails
// differently from a typo: it would file a VRAM measurement against a pool that
// holds no VRAM.
func TestValidateRejectsANonDevicePool(t *testing.T) {
	c := twoCardBox()
	err := c.validatePool("qwen", "", "box1", "system")
	if err == nil || !strings.Contains(err.Error(), "not a device pool") {
		t.Fatalf("err = %v, want a not-a-device-pool error", err)
	}
}

// Empty is the single-GPU answer, not missing data.
func TestValidateAcceptsAnEmptyPool(t *testing.T) {
	c := twoCardBox()
	if err := c.validatePool("qwen", "", "box1", ""); err != nil {
		t.Fatalf("empty pool must be valid, got %v", err)
	}
}

// The legacy single-server shape normalises into one placement, and `pool:` has
// to survive that trip or an old-style config would lose its placement the
// moment anything read it through PlacementList.
func TestLegacyModelCarriesPoolIntoItsPlacement(t *testing.T) {
	m := Model{Server: "box1", Cmd: "llama-server", Pool: "gpu1"}
	ps := m.PlacementList()
	if len(ps) != 1 {
		t.Fatalf("got %d placements, want 1", len(ps))
	}
	if ps[0].Pool != "gpu1" {
		t.Errorf("placement pool = %q, want gpu1", ps[0].Pool)
	}
}

// ForPlacement is where a chosen placement becomes "the model" for three dozen
// call sites that read Model.Server/Cmd. A placement's pool must override the
// model's, or two placements on two cards would both resolve to whichever one
// the model-level field happened to name.
func TestForPlacementAppliesThePlacementsPool(t *testing.T) {
	m := Model{Server: "box1", Pool: "gpu0", Placements: []Placement{
		{Name: "a", Server: "box1", Cmd: "x", Pool: "gpu1"},
	}}
	got := m.ForPlacement(m.PlacementList()[0])
	if got.Pool != "gpu1" {
		t.Errorf("pool = %q, want gpu1 — the placement decides", got.Pool)
	}
}

// A placement that names no pool leaves the model's own answer alone, so a
// model-level `pool:` still covers every placement that does not override it.
func TestForPlacementKeepsTheModelPoolWhenUnset(t *testing.T) {
	m := Model{Server: "box1", Pool: "gpu1", Placements: []Placement{
		{Name: "a", Server: "box1", Cmd: "x"},
	}}
	got := m.ForPlacement(m.PlacementList()[0])
	if got.Pool != "gpu1" {
		t.Errorf("pool = %q, want gpu1", got.Pool)
	}
}
