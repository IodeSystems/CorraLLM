package config

import (
	"path/filepath"
	"testing"
)

// TestMaxQuality returns the top quality (0 when none set).
func TestMaxQuality(t *testing.T) {
	if got := MaxQuality([]Backend{{Quality: 60}, {Quality: 100}, {Quality: 40}}); got != 100 {
		t.Errorf("MaxQuality = %d, want 100", got)
	}
	if got := MaxQuality([]Backend{{}, {}}); got != 0 {
		t.Errorf("MaxQuality (unset) = %d, want 0", got)
	}
}

// TestAcceptsQuality: a non-degrading group accepts only the top tier; a
// degrading group accepts down to its floor (P7).
func TestAcceptsQuality(t *testing.T) {
	const top = 100
	noDegrade := PriorityGroup{}
	if !noDegrade.AcceptsQuality(100, top) {
		t.Error("non-degrade group must accept the top tier")
	}
	if noDegrade.AcceptsQuality(60, top) {
		t.Error("non-degrade group must reject below the top tier")
	}

	floor60 := PriorityGroup{AcceptDegrade: true, QualityFloor: 60}
	if !floor60.AcceptsQuality(60, top) {
		t.Error("degrade group must accept at its floor")
	}
	if floor60.AcceptsQuality(40, top) {
		t.Error("degrade group must reject below its floor")
	}

	anyQ := PriorityGroup{AcceptDegrade: true}
	if !anyQ.AcceptsQuality(0, top) {
		t.Error("degrade group with no floor must accept anything")
	}

	// Regression: a group with a floor must still accept a model whose whole
	// ladder is below that floor (audio backends default to quality 0). The
	// model's top tier is always acceptable — the floor only gates degrading
	// below the best when a better tier exists.
	floored := PriorityGroup{AcceptDegrade: true, QualityFloor: 1}
	if !floored.AcceptsQuality(0, 0) {
		t.Error("floor must not reject a single-tier quality-0 model (audio)")
	}
	if !floored.AcceptsQuality(1, 1) {
		t.Error("floor must accept a model whose top tier equals the floor")
	}
}

// TestLoadSampleConfig parses the committed corrallm.yaml and checks the shape
// the scheduler will rely on — and that Validate accepts it.
func TestLoadSampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "corrallm.yaml"))
	if err != nil {
		t.Fatalf("load sample config: %v", err)
	}
	if _, ok := c.Servers["box1"]; !ok {
		t.Errorf("expected server box1, got %v", keysOf(c.Servers))
	}
	m, ok := c.Models["qwen3-coder"]
	if !ok {
		t.Fatalf("expected model qwen3-coder, got %v", keysOf(c.Models))
	}
	if len(m.Backends) != 2 {
		t.Errorf("qwen3-coder: want 2 backends, got %d", len(m.Backends))
	}
	if c.Keys["aw3"] != "interactive" {
		t.Errorf("key aw3: want interactive, got %q", c.Keys["aw3"])
	}
}

// TestLoadMissingIsEmpty: a missing config file yields an empty, valid config.
func TestLoadMissingIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if len(c.Models) != 0 {
		t.Errorf("missing config should be empty, got %d models", len(c.Models))
	}
}

// TestValidateRejectsUnknownServer: a backend referencing an undeclared server
// must fail validation.
func TestValidateRejectsUnknownServer(t *testing.T) {
	c := &Config{
		Models: map[string]Model{
			"m": {Backends: []Backend{{Cmd: "x", Server: "ghost"}}},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for unknown server, got nil")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
