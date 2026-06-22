package config

import (
	"path/filepath"
	"testing"
)

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
