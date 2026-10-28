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

// A budget on a selector is its own: it refuses what it names and nothing
// else, so the walk still has the other account.
func TestSelectorBudgetDropsOnlyWhatItNames(t *testing.T) {
	k := newChildKeyRig(t, nil)
	tiny, big := k.cfg.Models["mock"], k.cfg.Models["mock"]
	tiny.ProviderName, tiny.Extension = "fw", "ext"
	big.ProviderName, big.Extension = "fw", "ext"
	k.cfg.Models = map[string]config.Model{"tiny": tiny, "big": big}
	k.cfg.Keys["spender"] = config.KeyPolicy{Group: "default", Models: []string{"cred:fw/*"},
		Budgets: config.KeyBudgets{"cred:fw/tiny": {{Req: 1, Per: "calendar-day", Hard: true}}}}
	cand := func(model, cred string) config.Candidate {
		return config.Candidate{Name: model, Model: k.cfg.Models[model],
			Credential: &config.Credential{Name: cred}}
	}
	caller := config.Caller{Key: "spender", GroupName: "default", Recognized: true}
	k.px.SetConfig(k.cfg)

	if !k.permits(caller, cand("tiny", "elsewhere")) {
		t.Error("cred:fw/* should cover another account of the same provider")
	}
	elsewhere := cand("tiny", "elsewhere")
	elsewhere.Model.ProviderName = "groq"
	if k.permits(caller, elsewhere) {
		t.Error("an account on an unlisted provider was admitted")
	}
	if h := k.px.budgetHits(caller, cand("tiny", "tiny")); len(h) != 1 ||
		h[0].scope != config.KeyCredentialScopeKey("spender", "cred:fw/tiny") {
		t.Errorf("tiny hits = %+v", h)
	}
	if h := k.px.budgetHits(caller, cand("big", "big")); len(h) != 0 {
		t.Errorf("no budget names big, but it hit %+v", h)
	}
	k.px.quota.Charge(config.KeyCredentialScopeKey("spender", "cred:fw/tiny"), config.DimRequests, 1)
	kept, full, _ := k.px.filterByKeyBudgets([]config.Candidate{cand("tiny", "tiny"), cand("big", "big")}, caller)
	if len(kept) != 1 || kept[0].Name != "big" || len(full) != 1 {
		t.Fatalf("kept %+v full %v, want only big", kept, full)
	}
}

// A subkey answers to its own budgets and its parent's: both are checked, each
// under the key that declares it, so spending the subkey's own leaves the
// parent's alone.
func TestChildBudgetAndParentBudgetBothApply(t *testing.T) {
	k := newChildKeyRig(t, nil)
	oneReq := config.LimitSet{{Req: 1, Per: "calendar-day", Hard: true}}
	k.cfg.Keys["parent"] = config.KeyPolicy{Group: "default", Mint: true,
		Budgets: config.KeyBudgets{"cred:fw/main": oneReq}}
	child, err := k.px.issueKey(AccessAsk{Label: "kid", Budgets: &config.KeyBudgets{"cred:fw/main": oneReq}}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	ck, _ := k.px.children.get(child)
	ck.Parent = "parent"
	if err := k.px.persistChildKey(ck); err != nil {
		t.Fatal(err)
	}
	k.px.SetConfig(k.cfg)
	cand := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"},
		Credential: &config.Credential{Name: "main"}}
	childCaller := k.px.resolveCaller(child)
	if childCaller.Parent != "parent" {
		t.Fatalf("child resolves as %+v", childCaller)
	}
	hits := k.px.budgetHits(childCaller, cand)
	if len(hits) != 2 || hits[0].scope != config.KeyCredentialScopeKey(child, "cred:fw/main") ||
		hits[1].scope != config.KeyCredentialScopeKey("parent", "cred:fw/main") {
		t.Errorf("child hits = %+v, want its own then its parent's", hits)
	}
	k.px.quota.Charge(config.KeyCredentialScopeKey(child, "cred:fw/main"), config.DimRequests, 1)
	if kept, _, _ := k.px.filterByKeyBudgets([]config.Candidate{cand}, childCaller); len(kept) != 0 {
		t.Error("the child spent its budget and was still admitted")
	}
	if kept, _, _ := k.px.filterByKeyBudgets([]config.Candidate{cand}, k.px.resolveCaller("parent")); len(kept) != 1 {
		t.Error("the parent was refused by its child's budget")
	}
}

// A subkey may not widen what its parent may use: both filters must admit.
func TestChildCannotWidenItsParent(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.Keys["parent"] = config.KeyPolicy{Group: "default", Mint: true, Models: []string{"cred:groq/*"}}
	k.px.SetConfig(k.cfg)
	key, err := k.px.issueKey(AccessAsk{Label: "kid", Models: &[]string{"cred:fw/*"}}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	ck, _ := k.px.children.get(key)
	ck.Parent = "parent"
	if err := k.px.persistChildKey(ck); err != nil {
		t.Fatal(err)
	}
	caller := k.px.resolveCaller(key)
	fw := config.Candidate{Name: "m", Model: config.Model{ProviderName: "fw"},
		Credential: &config.Credential{Name: "main"}}
	if k.permits(caller, fw) {
		t.Error("a child reached a provider its parent may not")
	}
	groq := config.Candidate{Name: "m", Model: config.Model{ProviderName: "groq"},
		Credential: &config.Credential{Name: "main"}}
	if k.permits(caller, groq) {
		t.Error("a child reached an account its OWN models exclude")
	}
}
