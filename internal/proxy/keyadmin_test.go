package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// A key an admin creates is OFF: creating it is a decision about what it may do,
// not permission for its first caller to start spending.
func TestCreateKeyIsDisabledUntilSwitchedOn(t *testing.T) {
	k := newChildKeyRig(t, nil)
	lim := config.LimitSet{{USD: 1, Per: "calendar-day", Hard: true}}
	key, err := k.px.CreateKey(AccessAsk{Label: "handed-over", Purpose: "handed over by hand", Limits: &lim}, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c := k.chat(key, "mock"); c != http.StatusUnauthorized {
		t.Fatalf("a new key served a request: %d, want 401", c)
	}
	rec := k.do(http.MethodPost, "/v1/chat/completions", key, `{"model":"mock"}`)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "switched off") {
		t.Errorf("refusal = %d %s, want a 401 saying it is off", rec.Code, rec.Body.String())
	}
	// It is listed (the owner has to be able to find it) and says it is off.
	if v, ok := k.px.MintedKey(key); !ok || !v.Disabled {
		t.Fatalf("created key not listed as off: %+v ok=%v", v, ok)
	}
	if err := k.px.SetKeyDisabled(key, false, nil); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if c := k.chat(key, "mock"); c != http.StatusOK {
		t.Fatalf("after switching on: %d", c)
	}
	// Off again, and the row survives a restart switched off.
	if err := k.px.SetKeyDisabled(key, true, nil); err != nil {
		t.Fatal(err)
	}
	k.mount()
	if c := k.chat(key, "mock"); c != http.StatusUnauthorized {
		t.Errorf("after restart: %d, want 401", c)
	}
}

// An approved key is the opposite: the owner already answered "yes" to a
// request, so it is live the moment it is issued.
func TestApprovedKeyIsLive(t *testing.T) {
	k := newChildKeyRig(t, nil)
	key, err := k.px.IssueKey(AccessAsk{Label: "asked-for"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c := k.chat(key, "mock"); c != http.StatusOK {
		t.Errorf("an approved key needed switching on: %d", c)
	}
}

// Switching a key off reaches CONFIGURED keys too, through the config editor —
// and switching one off stops the child keys it minted, since they run as it.
func TestDisableReachesConfiguredKeysAndTheirChildren(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"p"}`)
	edit := func(note string, fn func(*config.Config) error) error { return fn(k.cfg) }
	if err := k.px.SetKeyDisabled("parent", true, edit); err != nil {
		t.Fatalf("disable configured key: %v", err)
	}
	if !k.cfg.Keys["parent"].Disabled {
		t.Error("the config was not written")
	}
	if c := k.chat("parent", "mock"); c != http.StatusUnauthorized {
		t.Errorf("configured key switched off: %d, want 401", c)
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusUnauthorized {
		t.Errorf("child of a key switched off: %d, want 401", c)
	}
	// An unrelated key is untouched.
	if c := k.chat("peer", "mock"); c != http.StatusOK {
		t.Errorf("another key: %d", c)
	}
	if err := k.px.SetKeyDisabled("parent", false, edit); err != nil {
		t.Fatal(err)
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Errorf("child after switching the parent back on: %d", c)
	}
}

// A pattern's budget is its own: spent on the account it names, and refusing
// that account alone while another pattern still admits the request.
func TestCredentialPatternBudget(t *testing.T) {
	k := newChildKeyRig(t, nil)
	// Two accounts on one provider, each with a model of its own: the walk over
	// one served name yields one candidate per credential (expandCredentials).
	tiny, big := k.cfg.Models["mock"], k.cfg.Models["mock"]
	tiny.ProviderName, tiny.Extension = "fw", "ext"
	big.ProviderName, big.Extension = "fw", "ext"
	k.cfg.Models = map[string]config.Model{"tiny": tiny, "big": big}
	k.cfg.Keys["spender"] = config.KeyPolicy{Group: "default", Credentials: []config.KeyCredential{
		{Credential: "cred:fw/tiny", Limits: config.LimitSet{{Req: 1, Per: "calendar-day", Hard: true}}},
		{Credential: "cred:fw/*"},
	}}
	cand := func(model, cred string) config.Candidate {
		return config.Candidate{Name: model, Model: k.cfg.Models[model],
			Credential: &config.Credential{Name: cred}}
	}
	caller := config.Caller{Key: "spender", GroupName: "default", Recognized: true}

	// The narrower pattern admits its account with its budget; the wildcard
	// admits the other with none of its own.
	if v := k.px.credentialVerdict(caller, cand("tiny", "tiny")); !v.allowed || len(v.limits) != 1 || v.limits[0].Req != 1 {
		t.Fatalf("tiny: %+v, want the 1-request budget", v)
	}
	if v := k.px.credentialVerdict(caller, cand("big", "big")); !v.allowed || len(v.limits) != 0 {
		t.Fatalf("big: %+v, want admitted with no budget", v)
	}
	// The wildcard covers every OTHER account of the provider; an account on a
	// provider no pattern names at all is refused.
	if v := k.px.credentialVerdict(caller, cand("tiny", "elsewhere")); !v.allowed {
		t.Error("the wildcard should cover another account of the same provider")
	}
	elsewhere := cand("tiny", "elsewhere")
	elsewhere.Model.ProviderName = "groq"
	if v := k.px.credentialVerdict(caller, elsewhere); v.allowed {
		t.Error("an account on an unlisted provider was admitted")
	}
	if sc := k.px.credentialScopesFor(caller, cand("tiny", "tiny")); len(sc) != 1 ||
		sc[0] != config.KeyCredentialScopeKey("spender", "cred:fw/tiny") {
		t.Errorf("charge scopes = %v", sc)
	}
	if sc := k.px.credentialScopesFor(caller, cand("big", "big")); len(sc) != 0 {
		t.Errorf("the wildcard carries no budget, but charged %v", sc)
	}
	// Spent once, the gate drops that account and keeps the other.
	k.px.SetConfig(k.cfg) // seed the ledger's pattern scopes
	k.px.quota.Charge(config.KeyCredentialScopeKey("spender", "cred:fw/tiny"), config.DimRequests, 1)
	if kept := k.px.filterByCredential(nil, []config.Candidate{cand("tiny", "tiny"), cand("big", "big")}, caller); len(kept) != 1 || kept[0].Name != "big" {
		t.Fatalf("kept %d candidates, want only the unbudgeted account", len(kept))
	}
}

// A child's own pattern narrows its parent's, and the budget that gates and
// charges it belongs to the CHILD — the key whose entry spells it out. Gating
// under the caller while charging under the owner (or the reverse) would check a
// scope nothing was ever seeded under, silently making a budget unlimited.
func TestChildPatternBudgetIsTheChilds(t *testing.T) {
	k := newChildKeyRig(t, nil)
	limited := config.KeyCredential{Credential: "cred:fw/main",
		Limits: config.LimitSet{{Req: 1, Per: "calendar-day", Hard: true}}}
	k.cfg.Keys["parent"] = config.KeyPolicy{Group: "default", Mint: true,
		Credentials: []config.KeyCredential{{Credential: "cred:fw/*"}, limited}}
	child, err := k.px.issueKey(AccessAsk{Label: "kid", Credentials: &[]config.KeyCredential{limited}}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	// A child is a minted row with a PARENT (exactly what POST /v1/keys writes).
	ck, _ := k.px.children.get(child)
	ck.Parent = "parent"
	if err := k.px.persistChildKey(ck); err != nil {
		t.Fatal(err)
	}
	other, err := k.px.IssueKey(AccessAsk{Label: "loose"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cand := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"},
		Credential: &config.Credential{Name: "main"}}

	childCaller := k.px.resolveCaller(child)
	if childCaller.Parent != "parent" {
		t.Fatalf("child resolves as %+v", childCaller)
	}
	if sc := k.px.credentialScopesFor(childCaller, cand); len(sc) != 1 ||
		sc[0] != config.KeyCredentialScopeKey(child, "cred:fw/main") {
		t.Errorf("child charge scopes = %v, want its own", sc)
	}
	// The same pattern on the parent is the PARENT's budget when the parent calls.
	if sc := k.px.credentialScopesFor(k.px.resolveCaller("parent"), cand); len(sc) != 1 ||
		sc[0] != config.KeyCredentialScopeKey("parent", "cred:fw/main") {
		t.Errorf("parent charge scopes = %v", sc)
	}
	// A key with no patterns of its own inherits the parent's permission, and
	// its budget with it.
	if sc := k.px.credentialScopesFor(k.px.resolveCaller(other), cand); len(sc) != 0 {
		t.Errorf("an admin-issued key has no parent pattern, but charged %v", sc)
	}

	// Spend the child's budget: its model is refused while the parent's own is
	// untouched, because they are two budgets.
	k.px.SetConfig(k.cfg)
	k.px.quota.Charge(config.KeyCredentialScopeKey(child, "cred:fw/main"), config.DimRequests, 1)
	if kept := k.px.filterByCredential(nil, []config.Candidate{cand}, childCaller); len(kept) != 0 {
		t.Error("the child spent its pattern budget and was still admitted")
	}
	if kept := k.px.filterByCredential(nil, []config.Candidate{cand}, k.px.resolveCaller("parent")); len(kept) != 1 {
		t.Error("the parent's budget was spent by its child's requests")
	}
}

// A child may not widen what its parent may spend: the chain must admit, so a
// pattern on the child that the parent's patterns exclude is no pattern at all.
func TestChildCannotWidenItsParent(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.Keys["parent"] = config.KeyPolicy{Group: "default", Mint: true,
		Credentials: []config.KeyCredential{{Credential: "cred:groq/*"}}}
	// A child is a minted row with a parent, so write one: the parent's own
	// patterns are the ceiling, whatever the child was given.
	key, err := k.px.issueKey(AccessAsk{Label: "kid",
		Credentials: &[]config.KeyCredential{{Credential: "cred:fw/*"}}}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	ck, _ := k.px.children.get(key)
	ck.Parent = "parent"
	if err := k.px.persistChildKey(ck); err != nil {
		t.Fatal(err)
	}
	caller := k.px.resolveCaller(key)
	if caller.Parent != "parent" {
		t.Fatalf("caller = %+v", caller)
	}
	fw := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"},
		Credential: &config.Credential{Name: "main"}}
	if v := k.px.credentialVerdict(caller, fw); v.allowed {
		t.Error("a child reached a provider its parent may not")
	}
	groq := config.Candidate{Name: "m", Model: config.Model{ProviderName: "groq"},
		Credential: &config.Credential{Name: "main"}}
	if v := k.px.credentialVerdict(caller, groq); v.allowed {
		t.Error("a child reached an account its OWN patterns exclude")
	}
}
