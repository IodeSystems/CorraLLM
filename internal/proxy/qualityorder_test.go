package proxy

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A 1.5 tier must be walked after 2 and before 1 — the ordering the whole
// fractional-quality change exists for.
func TestOrderCandidates_FractionalTierSitsBetween(t *testing.T) {
	cands := []config.Candidate{
		{Name: "bonsai", Model: config.Model{Quality: 1}},
		{Name: "mac", Model: config.Model{Quality: 1.5}},
		{Name: "mtp", Model: config.Model{Quality: 2}},
	}
	got := orderCandidates(cands, 0)
	want := []string{"mtp", "mac", "bonsai"}
	for i, idx := range got {
		if cands[idx].Name != want[i] {
			t.Fatalf("order = %v, want %v", namesOf(cands, got), want)
		}
	}
}

func namesOf(c []config.Candidate, idxs []int) []string {
	out := make([]string, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, c[i].Name)
	}
	return out
}
