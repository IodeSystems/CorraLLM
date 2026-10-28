package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The permission is the CALLER's (its `models` selectors), so a
// candidate's own allowlist no longer grants anything; and a trusted caller (the
// benchmark harness) bypasses the check entirely.
func TestFilterByAccessTrustedCallerPassesAllowlist(t *testing.T) {
	cands := []config.Candidate{{Credential: &config.Credential{Name: "main"}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	p := &Proxy{}
	p.cfg.Store(&config.Config{Keys: map[string]config.KeyPolicy{
		"sk-aw4":     {Group: "default"},
		"restricted": {Group: "default", Models: []string{"cred:other/*"}},
	}})
	key := func(k string) config.Caller { return p.config().ResolveCaller(k) }
	// No models filter → everything.
	if got := kept(p.filterByAccess(r, cands, key("sk-aw4"))); len(got) != 1 {
		t.Fatalf("key with no patterns kept %d candidates, want 1", len(got))
	}
	// A filter that does not match → refused, whatever a credential says.
	if got := kept(p.filterByAccess(r, cands, key("restricted"))); len(got) != 0 {
		t.Fatalf("unmatched pattern kept %d candidates, want 0", len(got))
	}
	// A caller with no key entry spends no declared account (owner 10-27).
	if got := kept(p.filterByAccess(r, cands, key("stranger"))); len(got) != 0 {
		t.Fatalf("unknown key kept %d candidates, want 0", len(got))
	}

	p.SetTrustedCaller(func(_ *http.Request, k string) bool { return k == "bench-1" })
	if got := kept(p.filterByAccess(r, cands, key("bench-1"))); len(got) != 1 {
		t.Fatalf("trusted bench key kept %d candidates, want 1", len(got))
	}
	if got := kept(p.filterByAccess(r, cands, key("restricted"))); len(got) != 0 {
		t.Fatalf("other key kept %d candidates, want 0", len(got))
	}
}

func kept(c []config.Candidate, _ string) []config.Candidate { return c }
