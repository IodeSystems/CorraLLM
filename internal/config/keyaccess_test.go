package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSelectorMatches(t *testing.T) {
	local := Target{Model: "local-Swift", Provider: "local"}
	fw := Target{Model: "fireworks-deepseek-v4", Provider: "fireworks", Cred: "cred:fireworks/default", Remote: true}
	claude := Target{Model: "claude-sub-*", Provider: "claude", Remote: true}
	for _, c := range []struct {
		sel  string
		t    Target
		want bool
	}{
		{"*", fw, true},
		{"local", local, true},
		{"local", fw, false},
		{"proxy", fw, true},
		{"proxy", local, false},
		// A paid account served on loopback is not local.
		{"local", Target{Model: "fw-tunnel", Provider: "fireworks", Cred: "cred:fireworks/default"}, false},
		{"proxy", Target{Model: "fw-tunnel", Provider: "fireworks", Cred: "cred:fireworks/default"}, true},
		{"provider:fireworks", fw, true},
		{"provider:fire*", fw, true},
		{"provider:fireworks", claude, false},
		{"cred:fireworks/*", fw, true},
		{"cred:fireworks/*", claude, false}, // no credential: a cred selector never matches
		{"model:*deepseek*", fw, true},
		{"model:*deepseek*", local, false},
		{NormalizeSelector("local-Swift"), local, true}, // bare name = model:
	} {
		if got := SelectorMatches(c.sel, c.t); got != c.want {
			t.Errorf("%s on %+v = %v, want %v", c.sel, c.t, got, c.want)
		}
	}
	if !ModelsPermit(nil, fw) {
		t.Error("no models list must permit everything")
	}
	if ModelsPermit([]string{"local"}, fw) {
		t.Error("models [local] must not permit a remote model")
	}
}

func TestParseLimitCompactRoundTrip(t *testing.T) {
	for _, s := range []string{"$3/calendar-day,hard", "100req/hour", "$1.7/total", "60sec/minute"} {
		l, err := ParseLimit(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := l.Compact(); got != s {
			t.Errorf("%s → %s", s, got)
		}
	}
	for _, bad := range []string{"3/day", "$0/day", "$3", "$3/fortnight", "lots/day"} {
		if _, err := ParseLimit(bad); err == nil {
			t.Errorf("%q parsed; want an error", bad)
		}
	}
}

// budgets read in either form, and write compact.
func TestKeyBudgetsYAMLRoundTrip(t *testing.T) {
	src := `default: batch
models: [local, "provider:fireworks"]
budgets:
  "*": ["$20/calendar-day"]
  model:*deepseek*:
    - $3/calendar-day,hard
    - {req: 1000, per: hour}
`
	var k KeyPolicy
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	if len(k.Models) != 2 || k.Models[0] != "local" || k.Allow["models"] || k.Allow["budgets"] {
		t.Fatalf("models = %v, allow = %v", k.Models, k.Allow)
	}
	ds := k.Budgets["model:*deepseek*"]
	if len(ds) != 2 || ds[0].USD != 3 || !ds[0].Hard || ds[1].Req != 1000 {
		t.Fatalf("deepseek budget = %+v", ds)
	}
	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "$3/calendar-day,hard") {
		t.Errorf("budgets not written compact:\n%s", out)
	}
	var back KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("what we wrote does not parse: %v\n%s", err, out)
	}
	if len(back.Budgets["*"]) != 1 || back.Budgets["*"][0].USD != 20 || len(back.Models) != 2 {
		t.Errorf("lost in the round trip: %+v\n%s", back, out)
	}
}

// The live config's key shapes (10-28) migrate to exactly what they reached:
// a `:none` key keeps local models and the providers that declare no
// accounts; a pattern key keeps those plus its account; limits become budgets.
func TestMigrateKeyAccessKeepsReach(t *testing.T) {
	c, err := LoadBytesForTest([]byte(`
extensions:
  fw:
    providers:
      fireworks:
        proxy: {host: api.fireworks.ai, port: 443}
        credentials: [{name: default, headers: {authorization: "Bearer x"}}]
        provides:
          deepseek: {type: chat, upstream: d}
  claude:
    providers:
      claude:
        proxy: {host: api.anthropic.com, port: 443}
        provides:
          sub: {type: chat, upstream: s}
models:
  swift: {proxy: {host: 127.0.0.1, port: 5800}, type: chat}
priorityGroups: {interactive: {weight: 2}, batch: {weight: 1}}
keys:
  aw3: {default: interactive, credentials: [{credential: ":none"}]}
  sk-aw4:
    default: batch
    credentials: [{credential: "cred:fireworks/default", limits: ["$2/calendar-day,hard"]}]
    limits: [{usd: 10, per: calendar-day}, {usd: 1.7, per: calendar-day, hard: true, model: fw-deepseek}]
  plain: batch
`))
	if err != nil {
		t.Fatal(err)
	}
	aw3 := c.Keys["aw3"]
	if strings.Join(aw3.Models, " ") != "local provider:claude" || len(aw3.Credentials) != 0 {
		t.Errorf("aw3 models = %v, credentials = %v", aw3.Models, aw3.Credentials)
	}
	aw4 := c.Keys["sk-aw4"]
	if strings.Join(aw4.Models, " ") != "cred:fireworks/default local provider:claude" {
		t.Errorf("aw4 models = %v", aw4.Models)
	}
	if b := aw4.Budgets; len(b["*"]) != 1 || b["*"][0].USD != 10 ||
		len(b["model:fw-deepseek"]) != 1 || b["model:fw-deepseek"][0].Model != "" ||
		len(b["cred:fireworks/default"]) != 1 || len(aw4.Limits) != 0 {
		t.Errorf("aw4 budgets = %+v, limits = %v", aw4.Budgets, aw4.Limits)
	}
	if p := c.Keys["plain"]; len(p.Models) != 0 || len(p.Budgets) != 0 {
		t.Errorf("a key with nothing to move gained %+v", p)
	}
	// What each key could reach before is what it reaches now.
	for _, tc := range []struct {
		key  string
		t    Target
		want bool
	}{
		{"aw3", Target{Model: "swift", Provider: "local"}, true},
		{"aw3", Target{Model: "fw-deepseek", Provider: "fireworks", Cred: "cred:fireworks/default", Remote: true}, false},
		{"aw3", Target{Model: "claude-sub", Provider: "claude", Remote: true}, true},
		{"sk-aw4", Target{Model: "fw-deepseek", Provider: "fireworks", Cred: "cred:fireworks/default", Remote: true}, true},
		{"sk-aw4", Target{Model: "fw-deepseek", Provider: "fireworks", Cred: "cred:fireworks/other", Remote: true}, false},
	} {
		if got := ModelsPermit(c.Keys[tc.key].Models, tc.t); got != tc.want {
			t.Errorf("%s may use %+v = %v, want %v", tc.key, tc.t, got, tc.want)
		}
	}
}

func TestKeyBudgetScopeKeepsOldCounters(t *testing.T) {
	for sel, want := range map[string]string{
		"*":                      KeyScopeKey("k"),
		"model:deepseek":         KeyModelScopeKey("k", "deepseek"),
		"cred:fireworks/default": KeyCredentialScopeKey("k", "cred:fireworks/default"),
		"model:*deepseek*":       "key:k|model:*deepseek*",
		"local":                  "key:k|local",
	} {
		if got := KeyBudgetScope("k", sel); got != want {
			t.Errorf("%s → %s, want %s", sel, got, want)
		}
	}
}

func TestKeyBudgetsValidate(t *testing.T) {
	for _, b := range []KeyBudgets{
		{"bogus:x": {{USD: 1, Per: "day"}}},
		{"*": {{Sec: 5, Per: "day"}}},
		{"*": {{USD: 1, Per: "day", Model: "m"}}},
		{"*": {{USD: 1, Per: "day"}, {USD: 2, Per: "day"}}},
	} {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v validated; want an error", b)
		}
	}
}
