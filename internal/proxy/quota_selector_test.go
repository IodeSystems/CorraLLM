package proxy

import (
	"net/http"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
)

func cand(name string) config.Candidate { return config.Candidate{Name: name} }

func names(cs []config.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func exhaust(l *quota.Ledger, name string) {
	l.ObserveResponse(name, 200, http.Header{
		"X-Ratelimit-Limit-Requests":     {"1000"},
		"X-Ratelimit-Remaining-Requests": {"0"},
		"X-Ratelimit-Reset-Requests":     {"60s"},
	})
}

// The selector drops an exhausted free backend but keeps the local floor.
func TestFilterByQuota(t *testing.T) {
	p := &Proxy{quota: quota.New()}
	cands := []config.Candidate{cand("groq-a"), cand("groq-b"), cand("MTP-local")}

	// Nothing observed yet → all available (optimistic), unchanged.
	if got := p.filterByQuota(cands); len(got) != 3 {
		t.Fatalf("unobserved backends should all pass: %v", names(got))
	}

	// Exhaust groq-a → dropped, others kept.
	exhaust(p.quota, "groq-a")
	got := p.filterByQuota(cands)
	if len(got) != 2 || got[0].Name != "groq-b" || got[1].Name != "MTP-local" {
		t.Errorf("exhausted groq-a should be dropped: %v", names(got))
	}

	// Exhaust groq-b too → only the local floor remains.
	exhaust(p.quota, "groq-b")
	if got := p.filterByQuota(cands); len(got) != 1 || got[0].Name != "MTP-local" {
		t.Errorf("only the local floor should remain: %v", names(got))
	}
}

// A free-only lane with everything exhausted keeps the unfiltered walk rather
// than emptying to a blind 503.
func TestFilterByQuota_NeverEmpties(t *testing.T) {
	p := &Proxy{quota: quota.New()}
	cands := []config.Candidate{cand("groq-a")}
	exhaust(p.quota, "groq-a")
	if got := p.filterByQuota(cands); len(got) != 1 {
		t.Errorf("must not empty the candidate set: %v", names(got))
	}
}

// A hard-down backend (402/403 via MarkDown) is filtered out of the candidate set.
func TestFilterByQuota_SkipsHardDown(t *testing.T) {
	p := &Proxy{quota: quota.New()}
	cands := []config.Candidate{cand("groq-a"), cand("MTP-local")}
	p.quota.MarkDown("groq-a")
	got := p.filterByQuota(cands)
	if len(got) != 1 || got[0].Name != "MTP-local" {
		t.Errorf("hard-down groq-a should be dropped: %v", names(got))
	}
}

func TestIsHardFail(t *testing.T) {
	for _, c := range []struct {
		status int
		want   bool
	}{{401, true}, {402, true}, {403, true}, {429, false}, {500, false}, {200, false}} {
		if got := isHardFail(c.status); got != c.want {
			t.Errorf("isHardFail(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

// Privacy tiering: a sensitive request drops non-private remotes, keeping local
// backends (own box) and private remotes.
func TestFilterBySensitive(t *testing.T) {
	local := config.Candidate{Name: "MTP", Model: config.Model{}}                                                // FreeTier nil
	priv := config.Candidate{Name: "groq", Model: config.Model{FreeTier: &config.FreeTier{Private: true}}}       // won't train
	pub := config.Candidate{Name: "openrouter", Model: config.Model{FreeTier: &config.FreeTier{Private: false}}} // may train

	got := filterBySensitive([]config.Candidate{local, priv, pub})
	if len(got) != 2 {
		t.Fatalf("want local + private, got %v", names(got))
	}
	for _, c := range got {
		if c.Name == "openrouter" {
			t.Error("a non-private remote must be excluded for a sensitive request")
		}
	}

	// No keep-all fallback: if nothing is private/local, the result is empty
	// (the handler then refuses rather than leaking to a training backend).
	if got := filterBySensitive([]config.Candidate{pub}); len(got) != 0 {
		t.Errorf("all-public → empty (refuse), got %v", names(got))
	}
}

func TestIsSensitive(t *testing.T) {
	for _, c := range []struct {
		hdr  string
		want bool
	}{{"true", true}, {"1", true}, {"YES", true}, {"", false}, {"false", false}, {"0", false}} {
		r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		if c.hdr != "" {
			r.Header.Set("X-Corrallm-Sensitive", c.hdr)
		}
		if got := isSensitive(r); got != c.want {
			t.Errorf("isSensitive(%q) = %v, want %v", c.hdr, got, c.want)
		}
	}
}
