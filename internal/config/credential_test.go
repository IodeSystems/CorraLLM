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

// cfgWithCredentials builds a loaded config whose openrouter provider holds two
// accounts, via the real Validate() path that materialises provided models.
func cfgWithCredentials(t *testing.T, creds string) *Config {
	t.Helper()
	src := `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, basePath: /api, headers: {x-shared: "1"}}
        provides:
          m: {type: chat, upstream: vendor/m}
` + creds
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	// resolveExtensions is what materialises provided models into c.Models;
	// Validate alone leaves the map empty, which is a subtle way to write a
	// test that asserts nothing.
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Models) == 0 {
		t.Fatal("no models materialised — the fixture would assert nothing")
	}
	return &c
}

// TestResolveServedExpandsAcrossCredentials is P21b's core: ONE served name
// becomes one candidate per account, which is what turns several keys of one
// provider into a fallback ladder instead of several served names.
func TestResolveServedExpandsAcrossCredentials(t *testing.T) {
	c := cfgWithCredentials(t, `
        credentials:
          - name: personal
            headers: {authorization: "Bearer P"}
          - name: work
            headers: {authorization: "Bearer W"}
`)
	cands, ok := c.ResolveServed("openrouter-m")
	if !ok {
		t.Fatal("served name did not resolve")
	}
	if len(cands) != 2 {
		t.Fatalf("want one candidate per credential, got %d: %+v", len(cands), cands)
	}
	if cands[0].Credential.Name != "personal" || cands[1].Credential.Name != "work" {
		t.Errorf("order should follow config: %q, %q", cands[0].Credential.Name, cands[1].Credential.Name)
	}
	// Same served name throughout — the expansion is invisible to callers.
	for _, cd := range cands {
		if cd.Name != "openrouter-m" {
			t.Errorf("served name changed to %q; expansion must not create new names", cd.Name)
		}
	}
	// Distinct process identities, or the second would reuse the first's
	// connection and budget.
	if cands[0].ProcKey() == cands[1].ProcKey() {
		t.Errorf("both credentials share procKey %q", cands[0].ProcKey())
	}
}

// TestCandidateTargetMergesCredentialHeaders: shared headers survive, auth is
// per account.
func TestCandidateTargetMergesCredentialHeaders(t *testing.T) {
	c := cfgWithCredentials(t, `
        credentials:
          - name: personal
            headers: {authorization: "Bearer P"}
          - name: work
            headers: {authorization: "Bearer W"}
`)
	cands, _ := c.ResolveServed("openrouter-m")
	for i, want := range []string{"Bearer P", "Bearer W"} {
		tgt, err := cands[i].Target()
		if err != nil {
			t.Fatal(err)
		}
		if got := tgt.Headers["authorization"]; got != want {
			t.Errorf("credential %q authorization = %q, want %q", cands[i].Credential.Name, got, want)
		}
		if tgt.Headers["x-shared"] != "1" {
			t.Errorf("credential %q lost the provider's shared header: %+v", cands[i].Credential.Name, tgt.Headers)
		}
	}
	// The provider's own target must be untouched by the merge.
	base, _ := c.Models["openrouter-m"].ProxyTarget()
	if base.Headers["authorization"] != "" {
		t.Errorf("merging wrote back into the provider's headers: %+v", base.Headers)
	}
}

// TestNoCredentialsLeavesCandidatesUntouched is the back-compat half: a config
// that declares none must resolve exactly as it did before, with a nil
// Credential so Target() returns the model's proxy unchanged.
func TestNoCredentialsLeavesCandidatesUntouched(t *testing.T) {
	c := cfgWithCredentials(t, "")
	cands, ok := c.ResolveServed("openrouter-m")
	if !ok || len(cands) != 1 {
		t.Fatalf("want exactly one candidate, got %d", len(cands))
	}
	if cands[0].Credential != nil {
		t.Errorf("no credentials declared, so the candidate must carry none: %+v", cands[0].Credential)
	}
	if cands[0].ProcKey() != "openrouter-m" {
		t.Errorf("procKey changed to %q for an unmodified model", cands[0].ProcKey())
	}
	tgt, err := cands[0].Target()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Headers["x-shared"] != "1" {
		t.Errorf("shared headers lost: %+v", tgt.Headers)
	}
}

// TestCredentialAuthTokenCommandOverrides: one account may use a rotating
// credential store while a sibling uses a static key.
func TestCredentialAuthTokenCommandOverrides(t *testing.T) {
	c := cfgWithCredentials(t, `
        credentials:
          - name: rotating
            authTokenCommand: jq -r .tok /tmp/c.json
`)
	cands, _ := c.ResolveServed("openrouter-m")
	tgt, err := cands[0].Target()
	if err != nil {
		t.Fatal(err)
	}
	if tgt.AuthTokenCommand != "jq -r .tok /tmp/c.json" {
		t.Errorf("authTokenCommand = %q", tgt.AuthTokenCommand)
	}
}

// TestQuotaKeyIsTheCredential: a budget belongs to the ACCOUNT, not the model.
// Keying on the served model splits one key's quota across every model routed
// through it, and lets a per-model cap multiply — the exact defect P20 shipped
// and this replaces.
func TestQuotaKeyIsTheCredential(t *testing.T) {
	c := cfgWithCredentials(t, `
        credentials:
          - name: personal
            headers: {authorization: "Bearer P"}
          - name: work
            headers: {authorization: "Bearer W"}
`)
	cands, _ := c.ResolveServed("openrouter-m")
	if cands[0].QuotaKey() == cands[1].QuotaKey() {
		t.Fatalf("both accounts share budget key %q", cands[0].QuotaKey())
	}
	for _, cd := range cands {
		if cd.QuotaKey() == cd.Name {
			t.Errorf("%s keyed on the served model, not its credential", cd.Credential.Name)
		}
	}
	// Namespaced by provider, so two providers may each have a "personal".
	if got := cands[0].QuotaKey(); got != "cred:openrouter/personal" {
		t.Errorf("QuotaKey = %q", got)
	}
}

// TestQuotaKeyFallsBackToModel: without credentials the budget stays where it
// has always been, so existing counters keep their identity across the upgrade.
func TestQuotaKeyFallsBackToModel(t *testing.T) {
	c := cfgWithCredentials(t, "")
	cands, _ := c.ResolveServed("openrouter-m")
	if got := cands[0].QuotaKey(); got != "openrouter-m" {
		t.Errorf("QuotaKey = %q, want the served name — persisted counters key on it", got)
	}
}

// cfgCascade builds a config with budgets at several scopes at once.
func cfgCascade(t *testing.T, providerLimits, globalLimits string) *Config {
	t.Helper()
	src := `
` + globalLimits + `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides:
          m: {type: chat, upstream: vendor/m}
` + providerLimits + `
        credentials:
          - name: personal
            limits: [{usd: 10, per: month}]
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return &c
}

// TestQuotaScopesCascade: a request answers to its model, its credential, that
// credential's provider, and the box — narrowest first.
func TestQuotaScopesCascade(t *testing.T) {
	c := cfgCascade(t, "        limits: [{usd: 100, per: month}]", "limits: [{usd: 500, per: day}]")
	cands, ok := c.ResolveServed("openrouter-m")
	if !ok || len(cands) != 1 {
		t.Fatalf("resolve failed: %d candidates", len(cands))
	}
	got := cands[0].QuotaScopes(c)
	want := []string{"openrouter-m", "cred:openrouter/personal", "prov:openrouter", "*"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QuotaScopes = %v, want %v", got, want)
	}
}

// TestQuotaScopesOnlyWhatExists: a scope with no declared budget gets no
// counter, so the cost is proportional to what was actually asked for.
func TestQuotaScopesOnlyWhatExists(t *testing.T) {
	c := cfgCascade(t, "", "") // no provider limit, no global limit
	cands, _ := c.ResolveServed("openrouter-m")
	got := cands[0].QuotaScopes(c)
	want := []string{"openrouter-m", "cred:openrouter/personal"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QuotaScopes = %v, want %v — undeclared scopes must not appear", got, want)
	}
}

// TestQuotaScopesWithoutCredentials: the pre-P21 shape still charges its model,
// plus the global guard when one is declared.
func TestQuotaScopesWithoutCredentials(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
limits: [{usd: 50, per: day}]
models:
  plain: {proxy: 9000, type: chat}
`), &c); err != nil {
		t.Fatal(err)
	}
	cands, _ := c.ResolveServed("plain")
	got := cands[0].QuotaScopes(&c)
	want := []string{"plain", "*"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QuotaScopes = %v, want %v", got, want)
	}
}

// TestGlobalScopeIsUnroutable-around: the box-wide guard exists precisely
// because every narrower scope can be got wrong. It must appear for a
// credential-backed candidate too, not just a plain model.
func TestGlobalScopeAppliesToCredentialCandidates(t *testing.T) {
	c := cfgCascade(t, "", "limits: [{usd: 50, per: day}]")
	cands, _ := c.ResolveServed("openrouter-m")
	scopes := cands[0].QuotaScopes(c)
	if scopes[len(scopes)-1] != GlobalScopeKey {
		t.Errorf("global scope missing from %v", scopes)
	}
}

// TestACLAllowlistSemantics: absent allow = open, present = allowlist only.
// Verified on the resolved candidates, since that is where the filter reads it.
func TestACLAllowlistSemantics(t *testing.T) {
	c := cfgWithCredentials(t, `
        credentials:
          - name: open
          - name: gated
            allow: [aw3]
`)
	cands, _ := c.ResolveServed("openrouter-m")
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(cands))
	}
	byName := map[string]Credential{}
	for _, cd := range cands {
		byName[cd.Credential.Name] = *cd.Credential
	}
	if !byName["open"].AllowsKey("anyone") {
		t.Error("a credential with no allow list must serve every caller")
	}
	if !byName["gated"].AllowsKey("aw3") {
		t.Error("listed caller must be permitted")
	}
	if byName["gated"].AllowsKey("life-raglit") {
		t.Error("unlisted caller must be refused — allowlist, not denylist")
	}
}
