package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestFilterByCredentialTrustedCallerPassesAllowlist(t *testing.T) {
	cands := []config.Candidate{{Credential: &config.Credential{Name: "main", Allow: []string{"sk-aw4"}}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	p := &Proxy{}
	if got := p.filterByCredential(r, cands, "bench-1"); len(got) != 0 {
		t.Fatalf("untrusted bench key kept %d candidates, want 0", len(got))
	}
	if got := p.filterByCredential(r, cands, "sk-aw4"); len(got) != 1 {
		t.Fatalf("allowed key kept %d candidates, want 1", len(got))
	}

	p.SetTrustedCaller(func(_ *http.Request, key string) bool { return key == "bench-1" })
	if got := p.filterByCredential(r, cands, "bench-1"); len(got) != 1 {
		t.Fatalf("trusted bench key kept %d candidates, want 1", len(got))
	}
	if got := p.filterByCredential(r, cands, "dun"); len(got) != 0 {
		t.Fatalf("other key kept %d candidates, want 0", len(got))
	}
}
