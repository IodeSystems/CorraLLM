package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustLoad(t *testing.T, src string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &c
}

const bothGroups = `
priorityGroups:
  interactive: {weight: 10}
  batch: {weight: 1}
  default: {weight: 1}
keys:
  yscr: default
  sk-aw4:
    default: batch
    interactive: true
`

// The string form is what every config in existence is written in. It must keep
// parsing, and must grant no escalation it did not ask for.
func TestBareStringKeyStillWorksAndGrantsNothing(t *testing.T) {
	c := mustLoad(t, bothGroups)
	cr := c.ResolveCaller("yscr")
	if cr.GroupName != "default" || cr.Denied || cr.Requested != "" {
		t.Fatalf("got %+v, want plain default", cr)
	}
	// A string-form key may not escalate, even to a group that exists.
	cr = c.ResolveCaller("yscr:interactive")
	if cr.GroupName != "default" {
		t.Errorf("GroupName = %q, want default — a bare key must not escalate", cr.GroupName)
	}
	if !cr.Denied {
		t.Error("Denied must be set so the refusal is visible")
	}
	if cr.Key != "yscr" {
		t.Errorf("Key = %q, want yscr — attribution follows the identity", cr.Key)
	}
}

func TestPermittedKeyEscalates(t *testing.T) {
	c := mustLoad(t, bothGroups)
	base := c.ResolveCaller("sk-aw4")
	if base.GroupName != "batch" {
		t.Fatalf("unasked GroupName = %q, want batch", base.GroupName)
	}
	up := c.ResolveCaller("sk-aw4:interactive")
	if up.GroupName != "interactive" || up.Denied {
		t.Fatalf("got %+v, want interactive granted", up)
	}
	if up.Group.EffectiveWeight() != 10 {
		t.Errorf("weight = %d, want 10 — the escalated group's config must apply",
			up.Group.EffectiveWeight())
	}
	// Same tenant either way: cost must not split across three rows.
	if base.Key != "sk-aw4" || up.Key != "sk-aw4" {
		t.Errorf("Key = %q / %q, want sk-aw4 both", base.Key, up.Key)
	}
}

// Asking for the group you already have is not an escalation and is not denied.
func TestAskingForYourOwnGroupIsNotDenied(t *testing.T) {
	c := mustLoad(t, bothGroups)
	cr := c.ResolveCaller("sk-aw4:batch")
	if cr.GroupName != "batch" || cr.Denied {
		t.Errorf("got %+v, want batch, not denied", cr)
	}
}

// A key that may escalate can still be refused a group it was not granted.
func TestUngrantedGroupIsRefused(t *testing.T) {
	c := mustLoad(t, bothGroups)
	cr := c.ResolveCaller("sk-aw4:default")
	if cr.GroupName != "batch" || !cr.Denied {
		t.Errorf("got %+v, want fallback to batch and Denied", cr)
	}
}

// The safety property: adding this feature must not change what any existing
// credential means, including one that contains the separator.
func TestKeyContainingSeparatorResolvesToItself(t *testing.T) {
	c := mustLoad(t, `
priorityGroups:
  interactive: {weight: 10}
  batch: {weight: 1}
keys:
  "weird:interactive": batch
`)
	cr := c.ResolveCaller("weird:interactive")
	if cr.GroupName != "batch" {
		t.Errorf("GroupName = %q, want batch — the whole credential is a key and wins",
			cr.GroupName)
	}
	if cr.Key != "weird:interactive" || cr.Requested != "" || cr.Denied {
		t.Errorf("got %+v, want the whole string as the key with nothing requested", cr)
	}
}

// An escalation naming a group that does not exist must not resolve to the
// synthesized weight-1 fallback — that is quietly WORSE than the group the
// caller already had.
func TestEscalationToUnknownGroupKeepsTheDefault(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"batch": {Weight: 1}, "interactive": {Weight: 10}},
		Keys:           map[string]KeyPolicy{"k": {Group: "batch", Allow: map[string]bool{"ghost": true}}},
	}
	cr := c.ResolveCaller("k:ghost")
	if cr.GroupName != "batch" || !cr.Denied {
		t.Errorf("got %+v, want batch and Denied", cr)
	}
}

// Round-trip through YAML is not optional: the revision history saves and
// reloads the whole config on every write.
func TestKeyPolicyRoundTripsThroughYAML(t *testing.T) {
	c := mustLoad(t, bothGroups)
	out, err := yaml.Marshal(c.Keys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("reload what we just wrote: %v\n%s", err, out)
	}
	if back["sk-aw4"].Group != "batch" || !back["sk-aw4"].Allow["interactive"] {
		t.Errorf("policy did not survive: %+v", back["sk-aw4"])
	}
	// A key with no escalations must come back as the bare string it was
	// written as, so existing configs do not show a diff on every save.
	if got := string(out); !strings.Contains(got, "yscr: default") {
		t.Errorf("plain key did not marshal back to a bare string:\n%s", got)
	}
}

// Validation catches the two ways this is misconfigured, at load rather than at
// request time.
func TestValidationRejectsBadEscalations(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"batch": {Weight: 1}},
		Keys:           map[string]KeyPolicy{"k": {Group: "batch", Allow: map[string]bool{"ghost": true}}},
	}
	if err := c.Validate(); err == nil {
		t.Error("want an error for an escalation to an unknown group")
	}
	c = &Config{
		PriorityGroups: map[string]PriorityGroup{"batch": {Weight: 1}, "interactive": {Weight: 10}},
		Keys: map[string]KeyPolicy{
			"has:colon": {Group: "batch", Allow: map[string]bool{"interactive": true}},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("want an error: a key containing the separator can never escalate")
	}
}
