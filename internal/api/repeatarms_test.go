package api

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/store"
)

// Repeats of one arm are samples of the same measurement, so the arm view must
// POOL them. The previous fold kept whichever row was scanned last and threw the
// rest away, which reports one sample as though it were the result.
func TestArmsForPoolsRepeats(t *testing.T) {
	rows := []store.BenchProbeResult{
		{Toolset: "baseline", ToolFormat: "json", Repeat: 0,
			Stages: 3, StagesPassed: 3, ChecksTotal: 5, ChecksPassed: 5, Pass: true, WallMS: 100},
		{Toolset: "baseline", ToolFormat: "json", Repeat: 1,
			Stages: 3, StagesPassed: 3, ChecksTotal: 5, ChecksPassed: 5, Pass: true, WallMS: 200},
	}
	arms := armsFor(rows)
	if len(arms) != 1 {
		t.Fatalf("got %d arms, want 1 — repeats are the same arm", len(arms))
	}
	a := arms[0]
	if a.Repeats != 2 {
		t.Errorf("Repeats = %d, want 2", a.Repeats)
	}
	if a.Stages != 6 || a.StagesPassed != 6 {
		t.Errorf("stages = %d/%d, want totals 6/6 across the two samples", a.StagesPassed, a.Stages)
	}
	if a.WallMS != 300 {
		t.Errorf("WallMS = %d, want 300 (both samples)", a.WallMS)
	}
	// Pooled score equals the mean when the samples share a stage count, so a
	// clean 2/2 must still read as 100% rather than being inflated or halved.
	if a.Score != 1 {
		t.Errorf("Score = %v, want 1", a.Score)
	}
	if a.Flaky {
		t.Error("Flaky = true when both repeats agreed")
	}
}

// Disagreement between repeats is the variance --runs exists to find. A pooled
// percentage hides it, so it has to be flagged.
func TestArmsForFlagsDisagreeingRepeats(t *testing.T) {
	rows := []store.BenchProbeResult{
		{Toolset: "baseline", ToolFormat: "json", Repeat: 0,
			Stages: 3, StagesPassed: 3, Pass: true},
		{Toolset: "baseline", ToolFormat: "json", Repeat: 1,
			Stages: 3, StagesPassed: 1, Pass: false},
	}
	arms := armsFor(rows)
	if len(arms) != 1 {
		t.Fatalf("got %d arms, want 1", len(arms))
	}
	if !arms[0].Flaky {
		t.Error("Flaky = false when one repeat passed and the other failed")
	}
	if arms[0].Pass {
		t.Error("Pass = true when a repeat failed; a flaky probe is not a pass")
	}
}

// Distinct arms must stay distinct — pooling repeats must not pool the A/B
// comparison the arms exist to make.
func TestArmsForKeepsDistinctArmsApart(t *testing.T) {
	rows := []store.BenchProbeResult{
		{Toolset: "baseline", ToolFormat: "json", Repeat: 0, Stages: 3, StagesPassed: 3, Pass: true},
		{Toolset: "baseline", ToolFormat: "toon", Repeat: 0, Stages: 3, StagesPassed: 1},
	}
	arms := armsFor(rows)
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2 distinct arms", len(arms))
	}
	for _, a := range arms {
		if a.Repeats != 1 {
			t.Errorf("arm %s has Repeats=%d, want 1", a.Label, a.Repeats)
		}
	}
}
