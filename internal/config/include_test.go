package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `include:` is refused, not merged.
//
// It existed to protect a hand-written config from the daemon's rewrites: "a
// generated file included from here is machine-owned end to end, and the
// hand-written file stays hand-written". Config lives in the database now, so
// there is no rewrite to protect anything from — and honouring an include would
// pull in a file that the import then stores as though it had been written
// inline, leaving a second file on disk that looks live and is not.
//
// The refusal has to be USEFUL, because it lands on someone whose config
// stopped loading: it names the files to merge, and says why.
func TestIncludeIsRefusedWithGuidance(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "generated", "agents.yaml")
	if err := os.MkdirAll(filepath.Dir(inc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inc, []byte("servers:\n  box2:\n    pools: {gpu0: 8GB}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "corrallm.yaml")
	if err := os.WriteFile(main, []byte(
		"include:\n  - generated/agents.yaml\nservers:\n  box1:\n    pools: {gpu0: 24GB}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(main)
	if err == nil {
		t.Fatal("an include was honoured; it should be refused")
	}
	msg := err.Error()
	for _, want := range []string{"include", "database", "generated/agents.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it does not tell the operator what to do:\n%s", want, msg)
		}
	}
}

// A config with no include still loads. The check must not fire on the empty
// field, which every config has.
func TestNoIncludeStillLoads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "corrallm.yaml")
	if err := os.WriteFile(p, []byte("servers:\n  box1:\n    pools: {gpu0: 24GB}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := c.Servers["box1"]; !ok {
		t.Error("the config did not load")
	}
}
