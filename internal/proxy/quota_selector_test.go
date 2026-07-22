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
