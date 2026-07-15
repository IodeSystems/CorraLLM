package proxy

import (
	"reflect"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// TestPartitionResident covers the preferResident reorder: resident backends
// float to the front in their existing (quality) order; the rest follow, order
// preserved. walk arrives quality-ordered from orderCandidates.
func TestPartitionResident(t *testing.T) {
	// Three lane-member candidates, distinctly named (plain model names — no
	// "#idx" suffix in the new schema).
	cands := []config.Candidate{{Name: "chat-0"}, {Name: "chat-1"}, {Name: "chat-2"}}
	res := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}

	cases := []struct {
		name     string
		walk     []int
		resident map[string]bool
		want     []int
	}{
		{"none resident → unchanged", []int{0, 1, 2}, res(), []int{0, 1, 2}},
		{"warm low tier floats over cold top tier", []int{0, 1}, res("chat-1"), []int{1, 0}},
		{"warm top tier already first → unchanged", []int{0, 1}, res("chat-0"), []int{0, 1}},
		{"all resident → quality order preserved", []int{0, 1, 2}, res("chat-0", "chat-1", "chat-2"), []int{0, 1, 2}},
		{"middle resident floats, others keep order", []int{0, 1, 2}, res("chat-1"), []int{1, 0, 2}},
		{"two of three warm keep relative order", []int{0, 1, 2}, res("chat-2", "chat-0"), []int{0, 2, 1}},
		{"single-element walk untouched", []int{2}, res("chat-0"), []int{2}},
		{"empty walk untouched", []int{}, res("chat-0"), []int{}},
		{"resident name for a different candidate is ignored", []int{0, 1}, res("other-1"), []int{0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionResident(tc.walk, cands, tc.resident)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("partitionResident(%v) = %v, want %v", tc.walk, got, tc.want)
			}
		})
	}
}
