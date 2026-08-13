package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func provider(t *testing.T, in string) Provider {
	t.Helper()
	var p Provider
	if err := yaml.Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestImplicitDefaultCredential is the back-compat contract: every config
// written before credentials existed must keep working, and callers must be
// able to assume the list is non-empty rather than branching on it.
func TestImplicitDefaultCredential(t *testing.T) {
	p := provider(t, `
proxy: {host: openrouter.ai, port: 443, headers: {authorization: "Bearer ${K}"}}
provides: {m: {type: chat}}
`)
	got := p.CredentialList()
	if len(got) != 1 {
		t.Fatalf("want exactly one synthesised credential, got %d: %+v", len(got), got)
	}
	if got[0].Name != DefaultCredentialName {
		t.Errorf("name = %q, want %q", got[0].Name, DefaultCredentialName)
	}
	if got[0].Headers != nil {
		t.Errorf("the synthesised credential must carry no headers of its own (the provider's already apply), got %+v", got[0].Headers)
	}
}

// TestCredentialHeadersMergeOverShared: shared headers are declared once on the
// provider; a credential adds or overrides only what differs between accounts.
func TestCredentialHeadersMergeOverShared(t *testing.T) {
	shared := map[string]string{"anthropic-version": "2023-06-01", "authorization": "Bearer SHARED"}
	c := Credential{Name: "work", Headers: map[string]string{"authorization": "Bearer WORK"}}
	got := c.MergedHeaders(shared)
	want := map[string]string{"anthropic-version": "2023-06-01", "authorization": "Bearer WORK"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %+v, want %+v", got, want)
	}
	if shared["authorization"] != "Bearer SHARED" {
		t.Error("MergedHeaders mutated the shared map")
	}
}

func TestCredentialScopeKeyNamespacedByProvider(t *testing.T) {
	a := Credential{Name: "personal"}.ScopeKey("openrouter")
	b := Credential{Name: "personal"}.ScopeKey("groq")
	if a == b {
		t.Errorf("two providers' credentials collided on %q — budgets would be shared", a)
	}
}

// TestCredentialAllowsKey: empty allow = open; present = allowlist.
func TestCredentialAllowsKey(t *testing.T) {
	open := Credential{Name: "c"}
	if !open.AllowsKey("anyone") {
		t.Error("empty allow must permit every caller")
	}
	gated := Credential{Name: "c", Allow: []string{"aw3", "dun"}}
	if !gated.AllowsKey("aw3") {
		t.Error("listed key must be permitted")
	}
	if gated.AllowsKey("life-raglit") {
		t.Error("unlisted key must be refused — allowlist, not denylist")
	}
}

// TestValidateCredentialsRejectsUnusable: names key persisted budget counters,
// so an unnamed or duplicated one silently shares or loses a budget.
func TestValidateCredentialsRejectsUnusable(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"unnamed", "credentials: [{headers: {a: b}}]"},
		{"blank name", `credentials: [{name: "   "}]`},
		{"duplicate", "credentials: [{name: k}, {name: k}]"},
		{"bad limit", "credentials: [{name: k, limits: [{per: hour}]}]"},
	} {
		p := provider(t, tc.yaml)
		if err := validateCredentials("free", "openrouter", p); err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
		}
	}
}

// TestValidateCredentialsAcceptsGood, including the full documented shape.
func TestValidateCredentialsAcceptsGood(t *testing.T) {
	p := provider(t, `
credentials:
  - name: personal
    headers: {authorization: "Bearer ${OPENROUTER_KEY_PERSONAL}"}
    limits:
      - {req: 20, per: minute}
      - {usd: 50, per: month}
    allow: [aw3, dun]
  - name: work
    headers: {authorization: "Bearer ${OPENROUTER_KEY_WORK}"}
    limits: [{usd: 200, per: month}]
    allow: [life-raglit]
`)
	if err := validateCredentials("free", "openrouter", p); err != nil {
		t.Fatal(err)
	}
	if len(p.CredentialList()) != 2 {
		t.Fatalf("want both credentials, got %+v", p.CredentialList())
	}
	work, ok := p.CredentialNamed("work")
	if !ok {
		t.Fatal("CredentialNamed missed a declared credential")
	}
	if work.Limits[0].Amount() != 200 || work.Limits[0].Label() != "usd/month" {
		t.Errorf("work limits = %+v", work.Limits)
	}
	if work.AllowsKey("aw3") {
		t.Error("work must not be usable by a key it does not list")
	}
	// ${ENV} stays literal in the stored shape — the API serves this YAML.
	if work.Headers["authorization"] != "Bearer ${OPENROUTER_KEY_WORK}" {
		t.Errorf("env reference was expanded at parse time: %q", work.Headers["authorization"])
	}
}

// TestCredentialsSurviveMarshalRoundTrip guards the property the whole managed
// config rests on: corrallm REWRITES this file on every edit, so a field it can
// parse but not re-emit is silently deleted the next time anyone touches an
// unrelated model. That is how a credential — and its budget, and its ACL —
// would vanish without an error.
func TestCredentialsSurviveMarshalRoundTrip(t *testing.T) {
	src := `
proxy: {host: openrouter.ai, port: 443, basePath: /api}
credentials:
  - name: personal
    headers: {authorization: "Bearer ${K1}"}
    limits: [{req: 20, per: minute}, {usd: 50, per: month}]
    allow: [aw3]
  - name: work
    authTokenCommand: jq -r .tok /tmp/c.json
    limits: [{usd: 200, per: month}]
provides: {m: {type: chat}}
`
	var in Provider
	if err := yaml.Unmarshal([]byte(src), &in); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back Provider
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-parsing what we emitted failed: %v\n%s", err, out)
	}
	if len(back.Credentials) != 2 {
		t.Fatalf("credentials lost in the round trip (%d survived):\n%s", len(back.Credentials), out)
	}
	if !reflect.DeepEqual(in.Credentials, back.Credentials) {
		t.Errorf("credentials changed across the round trip:\n before %+v\n after  %+v\n yaml:\n%s",
			in.Credentials, back.Credentials, out)
	}
	// The secret reference must still be a reference, not an expanded value.
	if got := back.Credentials[0].Headers["authorization"]; got != "Bearer ${K1}" {
		t.Errorf("env reference did not survive as a reference: %q", got)
	}
	if back.Credentials[1].AuthTokenCommand == "" {
		t.Error("authTokenCommand was dropped")
	}
}
