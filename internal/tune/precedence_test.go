package tune

import "testing"

// The defect this guards: the cache is last-writer-wins and corrallm spawns
// constantly, so a bench's carefully-isolated measurement was silently replaced
// by the very next real request's contended one. "llm-bench is the
// authoritative measurer" was false in practice until precedence existed.
func TestUpdate_ServingDoesNotClobberBench(t *testing.T) {
	c := &Cache{data: map[string]Profile{}}
	bench := Profile{BaseMiB: 20000, PerSlotMiB: 500, PeakMiB: 21000, MeasuredSlots: 2,
		Source: SourceBench, Samples: []Sample{{Slots: 2, FootprintMiB: 21000}}}
	c.Update("gpu", "m", bench)

	// A later serving measurement, taken under contention, reports a different
	// (worse) shape.
	serving := Profile{BaseMiB: 9999, PerSlotMiB: 1, PeakMiB: 21500, MeasuredSlots: 1,
		Source: SourceServing, Samples: []Sample{{Slots: 1, FootprintMiB: 21500}}}
	c.Update("gpu", "m", serving)

	got, _ := c.Get("gpu", "m")
	if got.BaseMiB != 20000 || got.PerSlotMiB != 500 {
		t.Errorf("serving clobbered the bench shape: base=%d perSlot=%d", got.BaseMiB, got.PerSlotMiB)
	}
	if got.Source != SourceBench {
		t.Errorf("provenance lost: %q", got.Source)
	}
	// The serving observation is still real evidence about footprint, so its
	// peak and sample must survive even though its split does not.
	if got.PeakMiB != 21500 {
		t.Errorf("serving peak should still raise the peak: %d", got.PeakMiB)
	}
	if len(got.Samples) != 2 {
		t.Errorf("serving sample should still be folded in: %+v", got.Samples)
	}
}

// A newer BENCH measurement must supersede an older one, or a stale profile
// could never be corrected by re-running calibration.
func TestUpdate_BenchSupersedesBench(t *testing.T) {
	c := &Cache{data: map[string]Profile{}}
	c.Update("gpu", "m", Profile{BaseMiB: 100, PerSlotMiB: 10, Source: SourceBench, MeasuredAt: 1})
	c.Update("gpu", "m", Profile{BaseMiB: 200, PerSlotMiB: 20, Source: SourceBench, MeasuredAt: 2})
	got, _ := c.Get("gpu", "m")
	if got.BaseMiB != 200 || got.PerSlotMiB != 20 {
		t.Errorf("a newer bench measurement must win: %+v", got)
	}
}

// With no bench profile, serving measurement is the only data and must apply
// normally — a fresh install has to schedule before anyone calibrates.
func TestUpdate_ServingAppliesWhenNoBenchProfile(t *testing.T) {
	c := &Cache{data: map[string]Profile{}}
	c.Update("gpu", "m", Profile{BaseMiB: 500, PerSlotMiB: 50, Source: SourceServing})
	got, _ := c.Get("gpu", "m")
	if got.BaseMiB != 500 {
		t.Errorf("serving measurement should apply when it is the only data: %+v", got)
	}
}

// Derive is the SINGLE implementation both callers use. The KV fast path and
// the two-point slope must agree with what each caller previously computed
// inline.
func TestDerive(t *testing.T) {
	// KV logged: exact split, and KV is the TOTAL across slots.
	p := Derive(Profile{}, SourceServing, 10000, 2000, 4, 4096, 7)
	if p.PerSlotMiB != 500 || p.BaseMiB != 8000 {
		t.Errorf("kv fast path: base=%d perSlot=%d, want 8000/500", p.BaseMiB, p.PerSlotMiB)
	}
	if p.Source != SourceServing || p.MeasuredAt != 7 {
		t.Errorf("provenance not stamped: %+v", p)
	}

	// No KV log, one sample: per-slot unknown (0 = fail-safe "not tunable yet").
	one := Derive(Profile{}, SourceBench, 10000, 0, 1, 0, 0)
	if one.PerSlotMiB != 0 || one.BaseMiB != 10000 {
		t.Errorf("single sample should leave perSlot unknown: %+v", one)
	}

	// Second sample at a DISTINCT slot count gives the slope.
	two := Derive(one, SourceBench, 12000, 0, 3, 0, 0)
	if two.PerSlotMiB != 1000 {
		t.Errorf("slope from (1,10000),(3,12000) should be 1000/slot, got %d", two.PerSlotMiB)
	}

	// Peak never regresses.
	lower := Derive(Profile{PeakMiB: 99999}, SourceServing, 100, 0, 1, 0, 0)
	if lower.PeakMiB != 99999 {
		t.Errorf("peak must not regress: %d", lower.PeakMiB)
	}
}
