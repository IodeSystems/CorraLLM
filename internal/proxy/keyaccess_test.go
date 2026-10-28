package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// A model the key's `models` hide is a 403 that names the filter, and is
// missing from its /v1/models.
func TestHiddenModelIs403NamingTheFilter(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.Keys["narrow"] = config.KeyPolicy{Models: []string{"model:mock"}}
	k.mount()
	if c := k.chat("narrow", "mock"); c != http.StatusOK {
		t.Fatalf("permitted model: %d", c)
	}
	rec := k.do(http.MethodPost, "/v1/chat/completions", "narrow", `{"model":"other"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key narrow may not use other: its models are [model:mock]") {
		t.Fatalf("hidden model: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.Unmarshal(k.do(http.MethodGet, "/v1/models", "narrow", "").Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != "mock" {
		t.Errorf("narrow lists %+v, want only mock", list.Data)
	}
	_ = json.Unmarshal(k.do(http.MethodGet, "/v1/models", "parent", "").Body.Bytes(), &list)
	if len(list.Data) < 2 {
		t.Errorf("an unfiltered key lists %+v", list.Data)
	}
	// A subkey is cut to its parent's filter, and the 403 says it was the parent.
	k.cfg.Keys["parent"] = config.KeyPolicy{Mint: true, Models: []string{"model:mock"}}
	k.mount()
	child := k.mint(`{"label":"kid"}`)
	rec = k.do(http.MethodPost, "/v1/chat/completions", child.Key, `{"model":"other"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "parent key parent may not use other") {
		t.Errorf("child: %d %s", rec.Code, rec.Body.String())
	}
}

// A subkey stopped by its PARENT's budget is told so, with the budget named.
func TestParentBudgetRefusalSaysParent(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.Keys["parent"] = config.KeyPolicy{Mint: true,
		Budgets: config.KeyBudgets{"model:mock": {{Req: 1, Per: "calendar-day", Hard: true}}}}
	k.mount()
	child := k.mint(`{"label":"kid","budgets":{"*":["5req/calendar-day,hard"]}}`)
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Fatalf("first: %d", c)
	}
	rec := k.do(http.MethodPost, "/v1/chat/completions", child.Key, `{"model":"mock"}`)
	if rec.Code != http.StatusTooManyRequests ||
		!strings.Contains(rec.Body.String(), "parent key parent's budget on model:mock: 1 requests of 1 requests spent today (UTC)") {
		t.Fatalf("over the parent's budget: %d %s", rec.Code, rec.Body.String())
	}
	// The child's own `*` budget charged too, and another model still serves.
	if c := k.chat(child.Key, "other"); c != http.StatusOK {
		t.Errorf("another model: %d", c)
	}
	acts, _ := k.st.RecentActivity(store.Window{}, 10, "", "", "")
	found := false
	for _, a := range acts {
		if a.Status == http.StatusTooManyRequests && strings.Contains(a.Error, "parent key parent's budget on model:mock") {
			found = true
		}
	}
	if !found {
		t.Error("the activity row does not name the budget that refused")
	}
}

// A pre-P65 child row (limits with a model, credentials as bare scopes) loads
// in the new shape, reaches what it reached, and is written back migrated.
func TestLegacyChildRowMigrates(t *testing.T) {
	k := newChildKeyRig(t, nil)
	row := store.ChildKey{Key: "ck-legacy", Parent: "", Label: "old",
		Limits:      `[{"usd":3,"per":"total","hard":true},{"usd":1.7,"per":"calendar-day","hard":true,"model":"mock"}]`,
		Credentials: `["cred:fw/default"]`, CreatedMS: 1, UpdatedMS: 1}
	if err := k.st.SaveChildKey(row); err != nil {
		t.Fatal(err)
	}
	k.mount()
	ck, ok := k.px.children.get("ck-legacy")
	if !ok {
		t.Fatal("legacy row not loaded")
	}
	if !containsStr(ck.models, "cred:fw/default") || !containsStr(ck.models, "local") {
		t.Errorf("models = %v", ck.models)
	}
	if len(ck.budgets["*"]) != 1 || ck.budgets["*"][0].Per != "total" ||
		len(ck.budgets["model:mock"]) != 1 || ck.budgets["model:mock"][0].Model != "" {
		t.Errorf("budgets = %+v", ck.budgets)
	}
	rows, _ := k.st.LoadChildKeys()
	for _, r := range rows {
		if r.Key == "ck-legacy" && (r.Limits != "" || r.Credentials != "" || r.Models == "" || r.Budgets == "") {
			t.Errorf("row not written back migrated: %+v", r)
		}
	}
	if c := k.chat("ck-legacy", "mock"); c != http.StatusOK {
		t.Errorf("migrated key on a local model: %d", c)
	}
}

// Subkey budgets that add up past the parent's own are allowed and warned.
func TestOversubscriptionWarns(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.Keys["parent"] = config.KeyPolicy{Mint: true,
		Budgets: config.KeyBudgets{"model:mock": {{USD: 3, Per: "calendar-day", Hard: true}}}}
	k.mount()
	k.mint(`{"label":"a","budgets":{"model:mock":["$1.70/calendar-day,hard"]}}`)
	b := k.mint(`{"label":"b","budgets":{"model:mock":["$1.70/calendar-day,hard"]}}`)
	if len(b.Warnings) != 1 || b.Warnings[0] != "Sub-keys can spend $3.40 a day on mock together; this key's own $3.00 a day stops them first." {
		t.Errorf("warnings = %v", b.Warnings)
	}
	var list struct {
		Self struct {
			Warnings []string `json:"warnings"`
		} `json:"self"`
	}
	_ = json.Unmarshal(k.do(http.MethodGet, "/v1/keys", "parent", "").Body.Bytes(), &list)
	if len(list.Self.Warnings) != 1 {
		t.Errorf("list warnings = %v", list.Self.Warnings)
	}
	// A parent budget on `*` covers the subkeys' budgets on any selector.
	k.cfg.Keys["parent"] = config.KeyPolicy{Mint: true,
		Budgets: config.KeyBudgets{"*": {{USD: 3, Per: "calendar-day", Hard: true}}}}
	k.mount()
	if w := k.px.oversubscription("parent"); len(w) != 1 || !strings.Contains(w[0], "this key's own $3.00 a day stops them first") {
		t.Errorf("warnings against the parent's * budget = %v", w)
	}
}

// A typo in a selector is refused, not saved as a filter that hides
// everything.
func TestUnknownSelectorNameRefused(t *testing.T) {
	k := newChildKeyRig(t, nil)
	rec := k.do(http.MethodPost, "/v1/keys", "parent", `{"label":"x","models":["model:mokc"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `no model \"mokc\"`) {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

// Any key reads its own entry; only a minting key manages subkeys.
func TestAnyKeyReadsItsOwnBudgets(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"kid","budgets":{"*":["$2/calendar-day,hard"]}}`)
	rec := k.do(http.MethodGet, "/v1/keys", child.Key, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"budgets":{"*":[{"usd":2`) {
		t.Errorf("child reads itself: %d %s", rec.Code, rec.Body.String())
	}
	if rec := k.do(http.MethodPost, "/v1/keys", child.Key, `{"label":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("child mints: %d", rec.Code)
	}
	if rec := k.do(http.MethodGet, "/v1/keys", "plain", ""); rec.Code != http.StatusOK {
		t.Errorf("a configured key without mint: %d %s", rec.Code, rec.Body.String())
	}
	if rec := k.do(http.MethodGet, "/v1/keys", "ck-nope", ""); rec.Code == http.StatusOK {
		t.Error("an unknown key read an entry")
	}
}
