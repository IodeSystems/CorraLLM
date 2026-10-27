package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The credential permission is the CALLER's (its allow patterns), so a
// candidate's own allowlist no longer grants anything; and a trusted caller (the
// benchmark harness) bypasses the check entirely.
func TestFilterByCredentialTrustedCallerPassesAllowlist(t *testing.T) {
	cands := []config.Candidate{{Credential: &config.Credential{Name: "main"}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	p := &Proxy{}
	p.cfg.Store(&config.Config{Keys: map[string]config.KeyPolicy{
		"sk-aw4":     {Group: "default"},
		"restricted": {Group: "default", Credentials: []config.KeyCredential{{Credential: "cred:other/*"}}},
	}})
	key := func(k string) config.Caller { return config.Caller{Key: k} }
	// No patterns → every credential, the pre-existing behaviour.
	if got := p.filterByCredential(r, cands, key("sk-aw4")); len(got) != 1 {
		t.Fatalf("key with no patterns kept %d candidates, want 1", len(got))
	}
	// Patterns that do not match → refused, whatever a credential says.
	if got := p.filterByCredential(r, cands, key("restricted")); len(got) != 0 {
		t.Fatalf("unmatched pattern kept %d candidates, want 0", len(got))
	}
	// A credential's own allowlist grants nothing now: the caller's entry does.
	if got := p.filterByCredential(r, cands, key("stranger")); len(got) != 1 {
		t.Fatalf("unknown key kept %d candidates, want 1 (no patterns = all)", len(got))
	}

	p.SetTrustedCaller(func(_ *http.Request, k string) bool { return k == "bench-1" })
	if got := p.filterByCredential(r, cands, key("bench-1")); len(got) != 1 {
		t.Fatalf("trusted bench key kept %d candidates, want 1", len(got))
	}
	if got := p.filterByCredential(r, cands, key("restricted")); len(got) != 0 {
		t.Fatalf("other key kept %d candidates, want 0", len(got))
	}
}
