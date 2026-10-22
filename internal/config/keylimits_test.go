package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// A key allocation parses from the mapping form, survives the save round trip,
// and does not eat a group permission (P56).
func TestKeyLimitsSurviveTheYAMLRoundTrip(t *testing.T) {
	var k KeyPolicy
	src := "default: batch\ninteractive: true\nlimits:\n  - {usd: 20, per: calendar-month, hard: true}\n"
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	if len(k.Limits) != 1 || k.Limits[0].USD != 20 || !k.Limits[0].Hard || k.Limits[0].Calendar() != "month" {
		t.Fatalf("limits = %+v", k.Limits)
	}
	if !k.Allow["interactive"] || k.Allow["limits"] {
		t.Errorf("allow = %v, want interactive only", k.Allow)
	}
	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	var back KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("what we wrote does not parse: %v\n%s", err, out)
	}
	if len(back.Limits) != 1 || back.Limits[0].USD != 20 || !back.Limits[0].Hard {
		t.Errorf("limits lost in the round trip: %+v\n%s", back.Limits, out)
	}
}

// Nothing charges dwell seconds to a caller, so a `sec` allocation is refused
// rather than accepted and never filled; a bad window is refused too.
func TestKeyLimitsValidate(t *testing.T) {
	for _, l := range []Limit{{Sec: 60, Per: "hour"}, {USD: 5, Per: "fortnight"}} {
		c := &Config{PriorityGroups: map[string]PriorityGroup{"g": {Weight: 1}},
			Keys: map[string]KeyPolicy{"k": {Group: "g", Limits: LimitSet{l}}}}
		if err := c.Validate(); err == nil {
			t.Errorf("limit %+v accepted", l)
		}
	}
	ok := &Config{PriorityGroups: map[string]PriorityGroup{"g": {Weight: 1}}, Keys: map[string]KeyPolicy{"k": {Group: "g", Limits: LimitSet{{USD: 5, Per: "day"}}}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("usd/day refused: %v", err)
	}
}
