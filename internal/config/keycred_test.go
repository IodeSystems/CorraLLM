package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// An allow pattern parses from the mapping form, keeps its budget, and survives
// the save round trip — the same trap key allocations fell into (P56): a struct
// that marshals to a shape it cannot parse fails to load what it just wrote.
func TestKeyCredentialsSurviveTheYAMLRoundTrip(t *testing.T) {
	src := `default: batch
credentials:
  - credential: "cred:fireworks/*"
    limits: [{usd: 2, per: calendar-day, hard: true}]
  - credential: "cred:fireworks/paid"
    limits: [{usd: 20, per: calendar-day, hard: true}]
`
	var k KeyPolicy
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	if len(k.Credentials) != 2 || k.Credentials[0].Credential != "cred:fireworks/*" {
		t.Fatalf("credentials = %+v", k.Credentials)
	}
	if len(k.Credentials[1].Limits) != 1 || k.Credentials[1].Limits[0].USD != 20 {
		t.Errorf("pattern budget = %+v", k.Credentials[1].Limits)
	}
	// `credentials:` is a reserved word, not a group this key may escalate into.
	if k.Allow["credentials"] {
		t.Errorf("allow = %v, want no `credentials` group", k.Allow)
	}
	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	var back KeyPolicy
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("what we wrote does not parse: %v\n%s", err, out)
	}
	if len(back.Credentials) != 2 || len(back.Credentials[0].Limits) != 1 ||
		back.Credentials[0].Limits[0].USD != 2 || !back.Credentials[0].Limits[0].Hard {
		t.Errorf("patterns lost in the round trip: %+v\n%s", back.Credentials, out)
	}
}

// `disabled` is a policy statement, so it must survive being written back out —
// and a key that is NOT disabled must stay the bare string, so adding this
// feature does not rewrite every key in the revision history.
func TestKeyDisabledRoundTrip(t *testing.T) {
	var off KeyPolicy
	if err := yaml.Unmarshal([]byte("default: batch\ndisabled: true\n"), &off); err != nil {
		t.Fatal(err)
	}
	if !off.Disabled || off.Group != "batch" || off.Allow["disabled"] {
		t.Fatalf("off = %+v", off)
	}
	out, _ := yaml.Marshal(off)
	if !strings.Contains(string(out), "disabled: true") {
		t.Errorf("disabled lost: %s", out)
	}
	on, _ := yaml.Marshal(KeyPolicy{Group: "batch"})
	if strings.TrimSpace(string(on)) != "batch" {
		t.Errorf("an enabled key marshals to %q, want the bare group", on)
	}
}

// Most specific wins: the pattern that spells the most of the credential out
// carries the budget, and a wider pattern beneath it contributes nothing.
func TestAllowedCredentialMostSpecificWins(t *testing.T) {
	pats := []KeyCredential{
		{Credential: "cred:*", Limits: LimitSet{{USD: 100, Per: "calendar-day"}}},
		{Credential: "cred:fireworks/*", Limits: LimitSet{{USD: 2, Per: "calendar-day"}}},
		{Credential: "cred:fireworks/paid", Limits: LimitSet{{USD: 20, Per: "calendar-day"}}},
	}
	for _, c := range []struct {
		scope string
		usd   float64
	}{
		{"cred:fireworks/paid", 20},
		{"cred:fireworks/free", 2},
		{"cred:groq/main", 100},
	} {
		ls, ok := AllowedCredential(pats, c.scope)
		if !ok || len(ls) != 1 || ls[0].USD != c.usd {
			t.Errorf("%s: %+v ok=%v, want $%g", c.scope, ls, ok, c.usd)
		}
	}
	// An exact name beats a same-length wildcard that pins less of the front.
	ties := []KeyCredential{
		{Credential: "cred:fw/*", Limits: LimitSet{{USD: 1, Per: "day"}}},
		{Credential: "cred:fw/ma*", Limits: LimitSet{{USD: 2, Per: "day"}}},
		{Credential: "cred:fw/main", Limits: LimitSet{{USD: 3, Per: "day"}}},
	}
	if ls, _ := AllowedCredential(ties, "cred:fw/main"); ls[0].USD != 3 {
		t.Errorf("exact name gave $%g, want $3", ls[0].USD)
	}
	if ls, _ := AllowedCredential(ties, "cred:fw/match"); ls[0].USD != 2 {
		t.Errorf("prefix wildcard gave $%g, want $2", ls[0].USD)
	}
}

// No patterns is every credential (the behaviour every key had); some patterns
// is an allowlist, and nothing else is admitted.
func TestPermitsCredential(t *testing.T) {
	if _, ok := PermitsCredential(nil, "cred:any/thing"); !ok {
		t.Error("no patterns should admit every credential")
	}
	pats := []KeyCredential{{Credential: "cred:fw/*"}}
	if _, ok := PermitsCredential(pats, "cred:fw/main"); !ok {
		t.Error("matching pattern refused")
	}
	if _, ok := PermitsCredential(pats, "cred:groq/main"); ok {
		t.Error("unmatched pattern admitted")
	}
}

// A malformed pattern is refused at load: it would match nothing, and a key
// that matches nothing can reach no upstream at all.
func TestValidateKeyCredentials(t *testing.T) {
	for _, bad := range [][]KeyCredential{
		{{Credential: "fireworks/*"}},                                             // no cred: prefix
		{{Credential: "cred:fireworks"}},                                          // no account
		{{Credential: "cred:/main"}},                                              // no provider
		{{Credential: "cred:fw/ma\x01in"}},                                        // control character, mid-name
		{{Credential: "cred:fw/main\x7f"}},                                        // DEL
		{{Credential: "cred:fw/*"}, {Credential: "cred:fw/*"}},                    // written twice
		{{Credential: "cred:fw/*", Limits: LimitSet{{Sec: 60, Per: "hour"}}}},     // dwell is never charged
		{{Credential: "cred:fw/*", Limits: LimitSet{{USD: 5, Per: "fortnight"}}}}, // no such window
	} {
		if err := ValidateKeyCredentials(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := ValidateKeyCredentials([]KeyCredential{{Credential: "cred:fw/*", Limits: LimitSet{{USD: 5, Per: "day", Hard: true}}}}); err != nil {
		t.Errorf("a well-formed pattern refused: %v", err)
	}
}

// Validate refuses a bad pattern on a CONFIG key, not just through the API: a
// hand-edited file has to fail where someone can read the error.
func TestConfigValidateRefusesBadKeyPattern(t *testing.T) {
	c := &Config{
		PriorityGroups: map[string]PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]KeyPolicy{"k": {Group: "default", Credentials: []KeyCredential{{Credential: "nope"}}}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `key "k"`) {
		t.Fatalf("err = %v, want a refusal naming the key", err)
	}
}

// The ledger identity of a pattern must not collide with another's: the
// separator cannot occur in a scope, so two patterns differing only where a
// naive ":" join would put them stay apart.
func TestKeyCredentialScopeKeyIsUnambiguous(t *testing.T) {
	a := KeyCredentialScopeKey("k", "cred:fw/a")
	b := KeyCredentialScopeKey("k", "cred:fw/b")
	if a == b {
		t.Fatal("two patterns share a scope")
	}
	if strings.Contains(a, "\x00") {
		t.Fatalf("scope %q carries a control character", a)
	}
	if !strings.HasPrefix(a, "keycred:k") {
		t.Errorf("scope %q is not namespaced by key", a)
	}
}
