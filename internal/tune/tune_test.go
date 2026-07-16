package tune

import (
	"path/filepath"
	"testing"
)

// TestSlotsForNoProfile: an empty cache reports ok=false — the fail-safe
// path callers rely on to leave a model's cmd untouched.
func TestSlotsForNoProfile(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.SlotsFor("RTX 5090", "qwen3-coder", 20000); ok {
		t.Error("want ok=false for a model with no cached profile")
	}
}

// TestSlotsForDivByZeroGuard: a degenerate profile (PerSlotMiB<=0) is
// treated as no profile, not a panic.
func TestSlotsForDivByZeroGuard(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "broken", Profile{BaseMiB: 1000, PerSlotMiB: 0, PeakMiB: 1000, MeasuredSlots: 1})
	if _, ok := c.SlotsFor("RTX 5090", "broken", 20000); ok {
		t.Error("want ok=false when PerSlotMiB<=0 (div-by-zero guard)")
	}
}

// TestSlotsForMath verifies the growth term and the division: a profile
// measured at 1 slot (base 4000, per-slot 1000, peak 5500 — 500 MiB of
// growth beyond the tidy 4000+1*1000=5000 accounting) against a 12000 MiB
// budget: n = (12000 - 4000 - 500) / 1000 = 7.
func TestSlotsForMath(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "qwen3-coder", Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5500, MeasuredSlots: 1, Ctx: 220160})
	n, ok := c.SlotsFor("RTX 5090", "qwen3-coder", 12000)
	if !ok {
		t.Fatal("want ok=true")
	}
	if n != 7 {
		t.Errorf("n = %d, want 7", n)
	}
}

// TestSlotsForClampToOne: a budget that can't fit even one slot's worth still
// floors to 1 (a scheduling/eviction problem elsewhere, not this function's
// job to refuse).
func TestSlotsForClampToOne(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "huge", Profile{BaseMiB: 20000, PerSlotMiB: 4000, PeakMiB: 20000, MeasuredSlots: 1})
	n, ok := c.SlotsFor("RTX 5090", "huge", 1000) // way under BaseMiB alone
	if !ok {
		t.Fatal("want ok=true")
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 (floored)", n)
	}
}

// TestSlotsForClampToCap: an enormous budget clamps to DefaultCap rather than
// reporting an absurd slot count.
func TestSlotsForClampToCap(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "tiny", Profile{BaseMiB: 100, PerSlotMiB: 10, PeakMiB: 110, MeasuredSlots: 1})
	n, ok := c.SlotsFor("RTX 5090", "tiny", 1_000_000)
	if !ok {
		t.Fatal("want ok=true")
	}
	if n != DefaultCap {
		t.Errorf("n = %d, want cap %d", n, DefaultCap)
	}
}

// TestUpdatePeakMonotonic: Update never lowers PeakMiB — the highest observed
// footprint sticks even if a later measurement's snapshot is smaller.
func TestUpdatePeakMonotonic(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "m", Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 9000, MeasuredSlots: 1})
	c.Update("RTX 5090", "m", Profile{BaseMiB: 4100, PerSlotMiB: 1050, PeakMiB: 5000, MeasuredSlots: 2}) // smaller peak this time
	p, ok := c.Get("RTX 5090", "m")
	if !ok {
		t.Fatal("want profile present")
	}
	if p.PeakMiB != 9000 {
		t.Errorf("PeakMiB = %d, want 9000 (max retained)", p.PeakMiB)
	}
	if p.BaseMiB != 4100 || p.PerSlotMiB != 1050 || p.MeasuredSlots != 2 {
		t.Errorf("latest measurement not applied: %+v", p)
	}
}

// TestBumpPeakNoopWithoutExistingProfile: the periodic sampler must not
// synthesize a profile with only a peak — Update (from measure-on-load) owns
// creating profiles.
func TestBumpPeakNoopWithoutExistingProfile(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.BumpPeak("RTX 5090", "never-measured", 99999)
	if _, ok := c.Get("RTX 5090", "never-measured"); ok {
		t.Error("BumpPeak must not create a profile from scratch")
	}
}

// TestBumpPeakRaisesExisting: BumpPeak raises PeakMiB on an existing profile,
// and never lowers it.
func TestBumpPeakRaisesExisting(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Update("RTX 5090", "m", Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5000, MeasuredSlots: 1})
	c.BumpPeak("RTX 5090", "m", 6000)
	if p, _ := c.Get("RTX 5090", "m"); p.PeakMiB != 6000 {
		t.Errorf("PeakMiB = %d, want 6000", p.PeakMiB)
	}
	c.BumpPeak("RTX 5090", "m", 5500) // lower — must not regress
	if p, _ := c.Get("RTX 5090", "m"); p.PeakMiB != 6000 {
		t.Errorf("PeakMiB regressed to %d, want 6000", p.PeakMiB)
	}
}

// TestLoadMissingFileIsEmpty: New against a nonexistent path is not an error.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	c, err := New(filepath.Join(t.TempDir(), "nope", "vram-profile.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.Get("gpu", "model"); ok {
		t.Error("want empty cache for a missing file")
	}
}

// TestSaveLoadRoundTrip: Save then New(path) recovers identical profiles,
// including directory creation for a path whose parent doesn't exist yet.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "vram-profile.json")
	c, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := Profile{BaseMiB: 4000, PerSlotMiB: 1000, PeakMiB: 5500, MeasuredSlots: 1, Ctx: 220160}
	c.Update("RTX 5090", "qwen3-coder", want)
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c2, err := New(path)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	got, ok := c2.Get("RTX 5090", "qwen3-coder")
	if !ok {
		t.Fatal("want profile present after reload")
	}
	if got != want {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}
