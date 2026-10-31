package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func guardConfig(t *testing.T, src string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

const guardYAML = `
models:
  guard: {proxy: {host: 127.0.0.1, port: 1}}
priorityGroups:
  batch: {weight: 1}
guardrails:
  violence: {model: guard, policy: "violence?"}
  pii: {model: guard, policy: "pii?", complete: 0.7, status: 451}
  jailbreak: {model: guard, policy: "jailbreak?"}
keys:
  sk-a:
    default: batch
    guardrails: {violence: hard, pii: soft, jailbreak: opt-in}
`

func TestKeyGuardrailsRoundTripAndDefaults(t *testing.T) {
	c := guardConfig(t, guardYAML)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Keys["sk-a"].Guardrails["jailbreak"]; got != GuardOptIn {
		t.Errorf("jailbreak = %q", got)
	}
	out, err := yaml.Marshal(c.Keys["sk-a"])
	if err != nil {
		t.Fatal(err)
	}
	var back KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Guardrails) != 3 || back.Guardrails["violence"] != GuardHard {
		t.Errorf("round trip lost guardrails:\n%s", out)
	}
	v, p := c.Guardrails["violence"], c.Guardrails["pii"]
	if v.CompleteThreshold() != 0.5 || v.IncompleteThreshold() != 0.2 || v.FailStatus() != 422 {
		t.Errorf("defaults: %v %v %v", v.CompleteThreshold(), v.IncompleteThreshold(), v.FailStatus())
	}
	if p.CompleteThreshold() != 0.7 || p.FailStatus() != 451 {
		t.Errorf("tuned: %v %v", p.CompleteThreshold(), p.FailStatus())
	}
}

func TestActiveGuardrailsOnlyTighten(t *testing.T) {
	c := guardConfig(t, guardYAML)
	got := func(ask string) string {
		active, ignored := c.ActiveGuardrails("sk-a", ask)
		var s []string
		for _, a := range active {
			lvl := "soft"
			if a.Hard {
				lvl = "hard"
			}
			s = append(s, a.Name+"="+lvl)
		}
		return strings.Join(s, ",") + " | " + strings.Join(ignored, ",")
	}
	for ask, want := range map[string]string{
		"":                         "pii=soft,violence=hard | ",
		"jailbreak":                "jailbreak=soft,pii=soft,violence=hard | ",
		"jailbreak=hard, pii=hard": "jailbreak=hard,pii=hard,violence=hard | ",
		"violence=soft":            "pii=soft,violence=hard | ",
		"nosuch, pii=off":          "pii=soft,violence=hard | nosuch,pii=off",
	} {
		if g := got(ask); g != want {
			t.Errorf("ask %q: got %q, want %q", ask, g, want)
		}
	}
	if active, _ := c.ActiveGuardrails("stranger", "violence"); len(active) != 0 {
		t.Errorf("a key with no guardrails got %v", active)
	}
}

func TestGuardrailValidation(t *testing.T) {
	for name, tc := range map[string]struct{ edit, want string }{
		"unknown model":       {`guardrails: {g: {model: nope, policy: p}}`, "not a configured model"},
		"no policy":           {`guardrails: {g: {model: guard}}`, "no `policy`"},
		"inverted":            {`guardrails: {g: {model: guard, policy: p, incomplete: 0.6, complete: 0.4}}`, "above complete"},
		"bad status":          {`guardrails: {g: {model: guard, policy: p, status: 200}}`, "not a 4xx"},
		"unknown guard":       {"keys: {sk-b: {default: batch, guardrails: {g: hard}}}", "no guardrail named"},
		"bad mode":            {"guardrails: {g: {model: guard, policy: p}}\nkeys: {sk-b: {default: batch, guardrails: {g: strict}}}", "want hard, soft or opt-in"},
		"guard not in models": {"guardrails: {g: {model: guard, policy: p}}\nkeys: {sk-b: {default: batch, models: [other], guardrails: {g: hard}}}", "do not include guardrail"},
	} {
		c := guardConfig(t, "models:\n  guard: {proxy: {host: 127.0.0.1, port: 1}}\npriorityGroups:\n  batch: {weight: 1}\n"+tc.edit)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
