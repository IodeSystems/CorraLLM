package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// A key policy round-trips through YAML on every save (the revision history is
// why MarshalYAML exists at all), so a field that parses but does not marshal
// would vanish at the next config write.
func TestThinkingSurvivesTheYAMLRoundTrip(t *testing.T) {
	var k KeyPolicy
	if err := yaml.Unmarshal([]byte("default: batch\ninteractive: true\nthinking: false\n"), &k); err != nil {
		t.Fatal(err)
	}
	if think, set := k.ThinkingPreference(); !set || think {
		t.Fatalf("parsed thinking = %v/%v, want an explicit false", think, set)
	}
	if !k.Allow["interactive"] {
		t.Error("the reserved field ate a real group permission")
	}

	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	var back KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("what we wrote does not parse: %v\n%s", err, out)
	}
	if think, set := back.ThinkingPreference(); !set || think {
		t.Errorf("thinking lost in the round trip: %v/%v\n%s", think, set, out)
	}
}

// A key with nothing but a group still marshals to the BARE STRING, so adding
// this feature does not show up as a diff on every key in the history.
func TestAPlainKeyStaysABareString(t *testing.T) {
	out, err := yaml.Marshal(KeyPolicy{Group: "batch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "batch\n" {
		t.Errorf("got %q, want the bare string it was written as", got)
	}
}

// Unset is not false. A key that says nothing follows the model, and collapsing
// the two would silently pin every caller to instruct.
func TestUnsetThinkingIsNotFalse(t *testing.T) {
	var k KeyPolicy
	if err := yaml.Unmarshal([]byte("batch\n"), &k); err != nil {
		t.Fatal(err)
	}
	if _, set := k.ThinkingPreference(); set {
		t.Error("a key with no opinion reported one")
	}
}

// `thinking` is reserved inside a key policy, where every other name permits a
// group. A group by that name would make the word mean two things at once, and
// the reader of such a config could not tell which applied.
func TestAGroupNamedThinkingIsRefused(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"thinking": {Weight: 1}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("a priority group named `thinking` was accepted")
	}
}
