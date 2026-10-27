package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

func ptr[T any](v T) *T { return &v }

// A keyless agent files, polls, and after approval gets its key exactly once;
// the key runs under what was granted, not what was asked.
func TestAccessRequestNewKey(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.PriorityGroups = map[string]config.PriorityGroup{"batch": {Weight: 1}}

	rec := k.do(http.MethodPost, "/v1/access-requests", "", `{"label":"bench","purpose":"run the suite","models":["mock"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("file: %d %s", rec.Code, rec.Body.String())
	}
	var filed struct{ ID, Claim, Status string }
	_ = json.Unmarshal(rec.Body.Bytes(), &filed)
	if filed.Status != "pending" || !strings.HasPrefix(filed.Claim, "cl-") {
		t.Fatalf("filed = %+v", filed)
	}
	poll := func(claim string) map[string]any {
		r := k.do(http.MethodGet, "/v1/access-requests/"+filed.ID, claim, "")
		var out map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &out)
		out["code"] = float64(r.Code)
		return out
	}
	if got := poll("cl-wrong"); got["code"] != float64(404) {
		t.Errorf("wrong claim: %v", got)
	}
	if got := poll(filed.Claim); got["status"] != "pending" || got["key"] != nil {
		t.Errorf("pending poll: %v", got)
	}

	key, err := k.px.ApproveAccess(filed.ID, "owner", "ok", AccessAsk{Label: "bench", Group: ptr("batch"),
		Limits: &config.LimitSet{{Req: 1, Per: "total", Hard: true}}, TTL: "2d"}, nil)
	if err != nil || !strings.HasPrefix(key, "ck-") {
		t.Fatalf("approve: %q %v", key, err)
	}
	if _, err := k.px.ApproveAccess(filed.ID, "owner", "", AccessAsk{Label: "x"}, nil); err != ErrNotPending {
		t.Errorf("second approval: %v, want ErrNotPending", err)
	}
	if got := poll(filed.Claim); got["status"] != "approved" || got["key"] != key {
		t.Fatalf("first poll after approval: %v", got)
	}
	if got := poll(filed.Claim); got["key"] != nil {
		t.Errorf("key handed over twice: %v", got)
	}

	if c := k.px.resolveCaller(key); c.GroupName != "batch" || !c.Recognized {
		t.Errorf("issued key resolves as %+v", c)
	}
	if c := k.chat(key, "mock"); c != http.StatusOK {
		t.Fatalf("issued key: %d", c)
	}
	rec = k.do(http.MethodPost, "/v1/chat/completions", key, `{"model":"mock"}`)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "allocation_spent") ||
		rec.Header().Get("Retry-After") != "" {
		t.Errorf("one-off budget spent: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if c := k.chat("peer", "mock"); c != http.StatusOK {
		t.Errorf("an unrelated key: %d", c)
	}
}

// Filing is refused through the front proxy, and bounded per address.
func TestAccessRequestGate(t *testing.T) {
	k := newChildKeyRig(t, nil)
	req := func(remote bool, key, body string) int {
		if remote {
			hr, _ := http.NewRequest(http.MethodPost, "/v1/access-requests", strings.NewReader(body))
			hr.Header.Set("X-Test-Remote", "1")
			rec := httptest.NewRecorder()
			k.r.ServeHTTP(rec, hr)
			return rec.Code
		}
		return k.do(http.MethodPost, "/v1/access-requests", key, body).Code
	}
	if c := req(true, "", `{"label":"x","purpose":"y"}`); c != http.StatusForbidden {
		t.Errorf("through the front proxy: %d, want 403", c)
	}
	if c := req(false, "", `{"purpose":"y"}`); c != http.StatusBadRequest {
		t.Errorf("new key without a label: %d, want 400", c)
	}
	if c := req(false, "nobody", `{"purpose":"y"}`); c != http.StatusForbidden {
		t.Errorf("an unknown key: %d, want 403", c)
	}
	if c := req(false, "", `{"label":"x","purpose":"y","models":["nope"]}`); c != http.StatusBadRequest {
		t.Errorf("unknown model: %d, want 400", c)
	}
	for i := 0; i < maxPendingPerIP; i++ {
		if c := req(false, "", `{"label":"x","purpose":"y"}`); c != http.StatusCreated {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	if c := req(false, "", `{"label":"x","purpose":"y"}`); c != http.StatusTooManyRequests {
		t.Errorf("past the per-address bound: %d, want 429", c)
	}
}

// A configured key asks for the mint permission and a value add; approval
// edits its config entry and stores the value add.
func TestAccessRequestExistingKey(t *testing.T) {
	k := newChildKeyRig(t, nil)
	rec := k.do(http.MethodPost, "/v1/access-requests", "plain", `{"purpose":"projects","mint":true,"valueAdds":[{"usd":5,"ttl":"2d"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("file: %d %s", rec.Code, rec.Body.String())
	}
	var filed struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &filed)
	var edited bool
	edit := func(note string, fn func(*config.Config) error) error {
		edited = true
		return fn(k.cfg)
	}
	if _, err := k.px.ApproveAccess(filed.ID, "owner", "", AccessAsk{Mint: ptr(true),
		ValueAdds: []ValueAddAsk{{USD: 5, TTL: "2d"}}}, edit); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !edited || !k.cfg.Keys["plain"].Mint {
		t.Errorf("mint not granted in the config: %+v", k.cfg.Keys["plain"])
	}
	gs := k.px.grants.of("plain")
	if len(gs) != 1 || gs[0].USD != 5 || gs[0].ExpiresMS < time.Now().Add(47*time.Hour).UnixMilli() {
		t.Errorf("value adds = %+v", gs)
	}
}

// A value add is spent only once the key's own hard limit refuses, and only
// up to its amount.
func TestValueAddOverflow(t *testing.T) {
	// Each request costs $0.00105; the key's own limit admits one a day.
	k := newChildKeyRig(t, config.LimitSet{{USD: 0.001, Per: "calendar-day", Hard: true}})
	if c := k.chat("parent", "mock"); c != http.StatusOK {
		t.Fatalf("first: %d", c)
	}
	if c := k.chat("parent", "mock"); c != http.StatusTooManyRequests {
		t.Fatalf("over the limit, no value add: %d", c)
	}
	if _, err := k.px.AddValueAdd("parent", ValueAddAsk{USD: 0.002, Model: "other"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c := k.chat("parent", "mock"); c != http.StatusTooManyRequests {
		t.Errorf("a value add for another model covered mock: %d", c)
	}
	if c := k.chat("parent", "other"); c != http.StatusOK {
		t.Fatalf("on the value add: %d", c)
	}
	if c := k.chat("parent", "other"); c != http.StatusOK {
		t.Fatalf("second on the value add: %d", c)
	}
	if c := k.chat("parent", "other"); c != http.StatusTooManyRequests {
		t.Errorf("value add spent: %d, want 429", c)
	}
	gs := k.px.grants.of("parent")
	if len(gs) != 1 || gs[0].Spent < 0.0021-1e-9 {
		t.Errorf("value add = %+v, want spent 0.0021", gs)
	}
	// Persisted: a restart keeps what was spent.
	k.mount()
	if gs := k.px.grants.of("parent"); len(gs) != 1 || gs[0].Spent < 0.0021-1e-9 {
		t.Errorf("after restart = %+v", gs)
	}
}

// Expiry: a parent sets a child's ttl; an admin disables and re-enables.
func TestMintedKeyExpiry(t *testing.T) {
	k := newChildKeyRig(t, nil)
	child := k.mint(`{"label":"x","ttl":"1h"}`)
	if child.Expires == "" {
		t.Fatalf("no expiry in %+v", child)
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Fatalf("before expiry: %d", c)
	}
	if err := k.px.ExpireMintedKey(child.Key); err != nil {
		t.Fatal(err)
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusUnauthorized {
		t.Errorf("expired: %d, want 401", c)
	}
	if rec := k.do(http.MethodPatch, "/v1/keys/"+child.Key, "parent", `{"ttl":""}`); rec.Code != http.StatusOK {
		t.Fatalf("parent clears expiry: %d %s", rec.Code, rec.Body.String())
	}
	if c := k.chat(child.Key, "mock"); c != http.StatusOK {
		t.Errorf("after clearing: %d", c)
	}
	if rec := k.do(http.MethodPatch, "/v1/keys/"+child.Key, "parent", `{"ttl":"400d"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("ttl past a year: %d", rec.Code)
	}
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"48h": 48 * time.Hour, "7d": 7 * 24 * time.Hour, "1d12h": 36 * time.Hour} {
		if got, err := ParseTTL(in); err != nil || got != want {
			t.Errorf("%s = %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0h", "x", "d", "-1h", "367d"} {
		if _, err := ParseTTL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An issued key passes a credential allowlist it was granted, and only that.
func TestIssuedKeyCredentials(t *testing.T) {
	k := newChildKeyRig(t, nil)
	key, err := k.px.IssueKey(AccessAsk{Label: "x",
		Credentials: &[]config.KeyCredential{{Credential: "cred:fw/main"}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cand := func(provider, name string) config.Candidate {
		return config.Candidate{Name: "m", Model: config.Model{ProviderName: provider},
			Credential: &config.Credential{Name: name}}
	}
	caller := k.px.resolveCaller(key)
	if v := k.px.credentialVerdict(caller, cand("fw", "main")); !v.allowed {
		t.Error("granted credential refused")
	}
	if v := k.px.credentialVerdict(caller, cand("fw", "other")); v.allowed {
		t.Error("credential allowed without a pattern")
	}
	// The permission is about the CALLER and nothing else (P63): a key with no
	// patterns may use every credential, whatever list the credential used to
	// carry. A credential's own `allow:` is migrated onto keys by Finalize and is
	// never consulted here again.
	if v := k.px.credentialVerdict(k.px.resolveCaller("parent"), cand("fw", "main")); !v.allowed {
		t.Error("a key with no patterns should reach every credential")
	}
	legacy := cand("fw", "main")
	legacy.Credential.Allow = []string{"sk-someone-else"}
	if v := k.px.credentialVerdict(k.px.resolveCaller("parent"), legacy); !v.allowed {
		t.Error("a credential's stale allow: list decided a permission the key's patterns own")
	}
}

// The configured rules decide who may file, and the request records which
// rule and which dashboard user.
func TestAccessRequestRulesApplied(t *testing.T) {
	k := newChildKeyRig(t, nil)
	k.cfg.AccessRequests = &config.AccessRequests{Rules: []config.AccessRequestRule{
		{Name: "agents", Token: "tok"}, {Name: "people", Users: []string{"carl@x"}}}}
	file := func(hdr map[string]string) int {
		hr := httptest.NewRequest(http.MethodPost, "/v1/access-requests", strings.NewReader(`{"label":"x","purpose":"y"}`))
		for h, v := range hdr {
			hr.Header.Set(h, v)
		}
		rec := httptest.NewRecorder()
		k.r.ServeHTTP(rec, hr)
		return rec.Code
	}
	if c := file(nil); c != http.StatusForbidden {
		t.Errorf("LAN peer, no rule matching: %d, want 403", c)
	}
	if c := file(map[string]string{"X-Test-Remote": "1", config.HeaderRequestToken: "tok"}); c != http.StatusCreated {
		t.Errorf("token through the front proxy: %d, want 201", c)
	}
	if c := file(map[string]string{"X-Test-User": "carl@x"}); c != http.StatusCreated {
		t.Errorf("dashboard user: %d, want 201", c)
	}
	rs, _ := k.px.AccessRequests()
	got := map[string]string{}
	for _, r := range rs {
		got[r.Via] = r.FiledBy
	}
	if v, ok := got["people"]; !ok || v != "carl@x" || len(got) != 2 {
		t.Errorf("recorded = %v", got)
	}
}
