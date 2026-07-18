package run

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/bench/task"
)

// A probe declaring `requires: {modality: image}` must SKIP a text-only model,
// not fail it. Recording a skip as a failure puts a number in the results table
// that reads as a capability gap when it is a configuration fact — the same
// category error as letting a turn cap veto passing checks.
func TestSkipReason(t *testing.T) {
	vision := &task.Task{Name: "v", Requires: task.Requires{Modality: "image"}}
	plain := &task.Task{Name: "p"}
	mods := map[string]map[string]bool{
		"seer":  {"text": true, "image": true},
		"blind": {"text": true},
	}
	cases := []struct {
		name  string
		tsk   *task.Task
		model string
		skip  bool
	}{
		{"vision probe on a vision model runs", vision, "seer", false},
		{"vision probe on a text-only model skips", vision, "blind", true},
		{"probe with no requires always runs", plain, "blind", false},
		// An unknown model means the catalog fetch failed or the model is not
		// listed. Run it: a spurious failure is visible in the results, a
		// silent skip is not — silently dropping coverage is the worse failure.
		{"unknown model runs rather than silently dropping coverage", vision, "ghost", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipReason(tc.tsk, tc.model, mods)
			if (got != "") != tc.skip {
				t.Errorf("skipReason = %q, want skip=%v", got, tc.skip)
			}
		})
	}
}

// With no catalog at all nothing is skipped — a corrallm outage must not
// produce an empty matrix that looks like a clean run.
func TestSkipReason_EmptyCatalogSkipsNothing(t *testing.T) {
	vision := &task.Task{Name: "v", Requires: task.Requires{Modality: "image"}}
	if got := skipReason(vision, "anything", map[string]map[string]bool{}); got != "" {
		t.Errorf("empty catalog should skip nothing, got %q", got)
	}
}
