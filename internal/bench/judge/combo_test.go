package judge

import "testing"

// A run:both probe wrote cold and warm to the SAME file, so the second pass
// overwrote the first and the surviving transcript for a cold/warm
// disagreement was the passing one — exactly the pass nobody needs to debug.
func TestComboVariant_ModeAndRepeatDoNotCollide(t *testing.T) {
	seen := map[string]bool{}
	for _, mode := range []string{"cold", "warm"} {
		for _, run := range []int{0, 1} {
			n := ComboVariant("m", "baseline", "capability-vision", mode, run)
			if seen[n] {
				t.Fatalf("collision on %q", n)
			}
			seen[n] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("want 4 distinct names, got %d: %v", len(seen), seen)
	}
}

// Run 0 with no mode keeps the bare name so single-run output, and everything
// written before the mode was in the filename, still resolves.
func TestComboVariant_PlainNameUnchanged(t *testing.T) {
	if got, want := ComboVariant("m", "baseline", "p", "", 0), ComboName("m", "baseline", "p"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestComboVariant_SanitizesMode(t *testing.T) {
	if got := ComboVariant("m", "b", "p", "co/ld", 0); got != "m_b_p_co-ld" {
		t.Errorf("mode must be sanitized like every other path segment: %q", got)
	}
}

// The judge has no run mode, so it needs a defined lookup order. Warm before
// cold: a quality judgement should read the steady-state pass.
func TestComboCandidates_WarmBeforeCold(t *testing.T) {
	c := ComboCandidates("m", "baseline", "p")
	if len(c) != 3 || c[0] != "m_baseline_p" || c[1] != "m_baseline_p_warm" || c[2] != "m_baseline_p_cold" {
		t.Errorf("unexpected order: %v", c)
	}
}
