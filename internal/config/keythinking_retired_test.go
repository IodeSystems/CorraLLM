package config

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// A stored key policy from before the per-key override was removed still has
// `thinking:`. It must load, warn once, keep the key's other fields, and not
// survive into what is written back.
func TestRetiredKeyThinkingIgnoredWithOneWarning(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	keyThinkingWarn = sync.Once{}

	var k KeyPolicy
	src := "default: batch\ninteractive: true\nthinking: false\n"
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	if !k.Allow["interactive"] {
		t.Error("the retired field ate a real group permission")
	}
	if k.Allow["thinking"] {
		t.Error("`thinking` was read as a group permission")
	}
	if n := strings.Count(buf.String(), "no longer used"); n != 1 {
		t.Errorf("warned %d times, want 1", n)
	}

	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "thinking") {
		t.Errorf("retired field written back:\n%s", out)
	}
}

// A key with nothing but a group marshals to the BARE STRING, so a save does
// not show up as a diff on every key in the history.
func TestAPlainKeyStaysABareString(t *testing.T) {
	out, err := yaml.Marshal(KeyPolicy{Group: "batch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "batch\n" {
		t.Errorf("got %q, want the bare string it was written as", got)
	}
}

// `thinking` stays reserved inside a key policy, so an old config never reads
// it as a group permission. A group by that name is refused.
func TestAGroupNamedThinkingIsRefused(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"thinking": {Weight: 1}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("a priority group named `thinking` was accepted")
	}
}
