package config

import (
	"net"
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

// A model filter is a caller-allocation feature: refused on any other limit,
// allowed (and split out) on a key's.
func TestLimitModelOnlyOnKeys(t *testing.T) {
	l := Limit{USD: 1, Per: "day", Model: "m"}
	if l.Validate() == nil {
		t.Error("a model filter outside a key allocation was accepted")
	}
	ls := LimitSet{l, {USD: 5, Per: "day"}}
	if err := ls.ValidateKey(); err != nil {
		t.Fatalf("key allocation with a model share: %v", err)
	}
	all, byModel := ls.ByModel()
	if len(all) != 1 || len(byModel["m"]) != 1 {
		t.Errorf("ByModel = %v, %v", all, byModel)
	}
	if (LimitSet{l, l}).ValidateKey() == nil {
		t.Error("two usd/day limits on one model were accepted")
	}
}

// A child key resolves exactly as its parent, :group suffix included, and is
// carried as itself.
func TestResolveCallerWithChild(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"batch": {Weight: 1}, "interactive": {Weight: 4}},
		Keys:           map[string]KeyPolicy{"sk-aw4": {Group: "batch", Allow: map[string]bool{"interactive": true}}},
	}
	parentOf := func(k string) (IssuedKey, bool) { return IssuedKey{Parent: "sk-aw4"}, k == "ck-1" }
	cr := c.ResolveCallerWith("ck-1:interactive", parentOf)
	if cr.Key != "ck-1" || cr.Parent != "sk-aw4" || cr.PolicyKey() != "sk-aw4" || !cr.Recognized || cr.GroupName != "interactive" {
		t.Errorf("child = %+v", cr)
	}
	if cr := c.ResolveCallerWith("sk-aw4", parentOf); cr.Parent != "" || cr.Key != "sk-aw4" || cr.GroupName != "batch" {
		t.Errorf("parent = %+v", cr)
	}
}

// An admin-issued key runs under its own policy; its child runs under that.
func TestResolveCallerWithIssued(t *testing.T) {
	c := &Config{PriorityGroups: map[string]PriorityGroup{"batch": {Weight: 1}, "interactive": {Weight: 4}}}
	issued := func(k string) (IssuedKey, bool) {
		switch k {
		case "ck-top":
			return IssuedKey{Policy: KeyPolicy{Group: "interactive"}}, true
		case "ck-kid":
			return IssuedKey{Parent: "ck-top"}, true
		}
		return IssuedKey{}, false
	}
	if cr := c.ResolveCallerWith("ck-top", issued); cr.Key != "ck-top" || cr.Parent != "" || !cr.Recognized || cr.GroupName != "interactive" {
		t.Errorf("issued = %+v", cr)
	}
	if cr := c.ResolveCallerWith("ck-kid", issued); cr.Key != "ck-kid" || cr.Parent != "ck-top" || !cr.Recognized || cr.GroupName != "interactive" {
		t.Errorf("child of issued = %+v", cr)
	}
	if cr := c.ResolveCallerWith("ck-top:batch", issued); cr.GroupName != "interactive" || !cr.Denied {
		t.Errorf("escalation it was not given = %+v", cr)
	}
}

// Filing rules: any rule may admit; every field a rule sets must match.
func TestAccessRequestRules(t *testing.T) {
	lan := net.ParseIP("192.168.1.9")
	var none *AccessRequests
	if _, ok := none.Allows(Filer{Peer: lan}); !ok {
		t.Error("default refused a direct LAN peer")
	}
	if _, ok := none.Allows(Filer{Peer: lan, Forwarded: true}); ok {
		t.Error("default admitted traffic through the front proxy")
	}
	if _, ok := none.Allows(Filer{Peer: net.ParseIP("8.8.8.8")}); ok {
		t.Error("default admitted a public peer")
	}
	a := &AccessRequests{Rules: []AccessRequestRule{
		{Name: "office", CIDRs: []string{"10.0.0.0/8"}},
		{Name: "agents", Token: "s3cret"},
		{Name: "carl", Users: []string{"Carl@Example.com"}},
		{Name: "lan-bob", CIDRs: []string{"192.168.0.0/16"}, Users: []string{"bob@x"}},
	}}
	for _, c := range []struct {
		f    Filer
		want string
	}{
		{Filer{Peer: net.ParseIP("10.1.2.3")}, "office"},
		{Filer{Peer: net.ParseIP("10.1.2.3"), Forwarded: true}, ""},
		{Filer{Peer: net.ParseIP("8.8.8.8"), Forwarded: true, Token: "s3cret"}, "agents"},
		{Filer{Peer: net.ParseIP("8.8.8.8"), Token: "wrong"}, ""},
		{Filer{Peer: net.ParseIP("8.8.8.8"), User: "carl@example.com"}, "carl"},
		{Filer{Peer: lan, User: "bob@x"}, "lan-bob"},
		{Filer{Peer: lan}, ""}, // the LAN rule also wants bob
	} {
		got, ok := a.Allows(c.f)
		if (c.want != "") != ok || got != c.want {
			t.Errorf("%+v → %q %v, want %q", c.f, got, ok, c.want)
		}
	}
	if _, ok := (&AccessRequests{}).Allows(Filer{Peer: lan}); ok {
		t.Error("no rules admitted someone")
	}
	if err := (&AccessRequests{Rules: []AccessRequestRule{{Name: "x"}}}).Validate(); err == nil {
		t.Error("empty rule accepted")
	}
	if err := (&AccessRequests{Rules: []AccessRequestRule{{CIDRs: []string{"192.168.1.1"}}}}).Validate(); err == nil {
		t.Error("bare address accepted as a subnet")
	}
}
