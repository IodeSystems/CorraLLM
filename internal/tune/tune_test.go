package tune

import (
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

// TestSlopeFromSamplesTwoPoints: two spawns at distinct slot counts (2 and
// 5) derive PerSlotMiB/BaseMiB by slope: perSlot = (11000-8000)/(5-2) = 1000,
// base = 8000 - 2*1000 = 6000.
func TestSlopeFromSamplesTwoPoints(t *testing.T) {
	perSlot, base, ok := SlopeFromSamples([]Sample{
		{Slots: 2, FootprintMiB: 8000},
		{Slots: 5, FootprintMiB: 11000},
	})
	if !ok {
		t.Fatal("want ok=true with two distinct-slot samples")
	}
	if perSlot != 1000 {
		t.Errorf("perSlot = %d, want 1000", perSlot)
	}
	if base != 6000 {
		t.Errorf("base = %d, want 6000", base)
	}
}

// TestSlopeFromSamplesOrderIndependent: the same two points in reverse
// insertion order (older sample last in the slice) derive identically —
// SlopeFromSamples picks the pair by slot-count distance, not slice order.
func TestSlopeFromSamplesOrderIndependent(t *testing.T) {
	perSlot, base, ok := SlopeFromSamples([]Sample{
		{Slots: 5, FootprintMiB: 11000},
		{Slots: 2, FootprintMiB: 8000},
	})
	if !ok || perSlot != 1000 || base != 6000 {
		t.Errorf("perSlot=%d base=%d ok=%v, want 1000/6000/true", perSlot, base, ok)
	}
}

// TestSlopeFromSamplesInsufficientData: fewer than two distinct slot counts
// (zero, one, or a repeat of the same slot count) reports ok=false — the
// fail-safe "not enough data yet" signal that leaves PerSlotMiB undetermined.
func TestSlopeFromSamplesInsufficientData(t *testing.T) {
	cases := [][]Sample{
		nil,
		{{Slots: 2, FootprintMiB: 8000}},
		{{Slots: 2, FootprintMiB: 8000}, {Slots: 2, FootprintMiB: 8100}}, // same slots, not distinct
	}
	for _, samples := range cases {
		if _, _, ok := SlopeFromSamples(samples); ok {
			t.Errorf("samples=%+v: want ok=false (insufficient distinct data)", samples)
		}
	}
}

// TestSlopeFromSamplesNegativeSlopeRejected: a decreasing footprint as slots
// rise is impossible (KV cost can't be negative) — treat it as noise and
// refuse to derive a profile from it, rather than sizing slots into
// overcommit.
func TestSlopeFromSamplesNegativeSlopeRejected(t *testing.T) {
	if _, _, ok := SlopeFromSamples([]Sample{
		{Slots: 2, FootprintMiB: 9000},
		{Slots: 5, FootprintMiB: 8000}, // smaller footprint at MORE slots
	}); ok {
		t.Error("want ok=false for a negative slope")
	}
}

// TestMergeSampleCapsAtTwoMostRecentDistinct: a third distinct slot count
// evicts the oldest, keeping exactly the two most-recent-distinct entries; a
// repeat of an already-present slot count refreshes in place instead of
// growing the set.
func TestMergeSampleCapsAtTwoMostRecentDistinct(t *testing.T) {
	var samples []Sample
	samples = MergeSample(samples, Sample{Slots: 1, FootprintMiB: 5000})
	samples = MergeSample(samples, Sample{Slots: 2, FootprintMiB: 6000})
	if len(samples) != 2 {
		t.Fatalf("after 2 distinct: len = %d, want 2", len(samples))
	}
	// Repeat measurement at slots=2 (noise) must refresh, not grow.
	samples = MergeSample(samples, Sample{Slots: 2, FootprintMiB: 6100})
	if len(samples) != 2 {
		t.Fatalf("after repeat: len = %d, want 2", len(samples))
	}
	// A third distinct slot count evicts the oldest (slots=1).
	samples = MergeSample(samples, Sample{Slots: 4, FootprintMiB: 8000})
	if len(samples) != 2 {
		t.Fatalf("after 3rd distinct: len = %d, want 2", len(samples))
	}
	for _, s := range samples {
		if s.Slots == 1 {
			t.Errorf("oldest distinct sample (slots=1) should have been evicted: %+v", samples)
		}
	}
	// Surviving pair is (slots=2, 6100) and (slots=4, 8000):
	// perSlot = (8000-6100)/(4-2) = 950, base = 6100 - 2*950 = 4200.
	perSlot, base, ok := SlopeFromSamples(samples)
	if !ok {
		t.Fatal("want ok=true after capping to 2 distinct")
	}
	if perSlot != 950 {
		t.Errorf("perSlot = %d, want 950", perSlot)
	}
	if base != 4200 {
		t.Errorf("base = %d, want 4200", base)
	}
}

// TestEffectivePeakHoldsUntilWindowFills: a profile with fewer than
// RecentWindow observations must keep reserving the all-time peak. Deciding an
// outlier has expired on two data points is how a model gets admitted against
// VRAM it will immediately take back.
func TestEffectivePeakHoldsUntilWindowFills(t *testing.T) {
	p := Profile{PeakMiB: 10706}
	for i := 0; i < RecentWindow-1; i++ {
		p.RecentPeaks = append(p.RecentPeaks, 6196)
		if got := p.EffectivePeakMiB(); got != 10706 {
			t.Fatalf("with %d samples EffectivePeakMiB = %d, want the all-time 10706", i+1, got)
		}
	}
}

// TestEffectivePeakExpiresOutlier: chandra's case. One 400 dpi page peaks at
// 10706; every later spawn sits near 6196. Once the window is full of the
// smaller readings the reservation must come back down, or the card is
// budgeted forever for a page it saw once.
func TestEffectivePeakExpiresOutlier(t *testing.T) {
	p := Derive(Profile{}, SourceServing, 10706, 0, 1, 0, 1)
	if got := p.EffectivePeakMiB(); got != 10706 {
		t.Fatalf("first observation should stand: %d", got)
	}
	for i := 0; i < RecentWindow; i++ {
		p = Derive(p, SourceServing, 6196, 0, 1, 0, int64(i+2))
	}
	if p.PeakMiB != 10706 {
		t.Errorf("all-time PeakMiB must be preserved for diagnostics, got %d", p.PeakMiB)
	}
	if got := p.EffectivePeakMiB(); got != 6196 {
		t.Errorf("EffectivePeakMiB = %d, want 6196 once the outlier aged out", got)
	}
}

// TestEffectivePeakNeverBelowRecentObservation: the window expires history, it
// does not discount the present. Anything seen inside the window is still
// reserved in full.
func TestEffectivePeakNeverBelowRecentObservation(t *testing.T) {
	p := Profile{}
	for i := 0; i < RecentWindow; i++ {
		p = Derive(p, SourceServing, 5000, 0, 1, 0, int64(i+1))
	}
	p = Derive(p, SourceServing, 9000, 0, 1, 0, 99) // a big page, just now
	if got := p.EffectivePeakMiB(); got != 9000 {
		t.Errorf("EffectivePeakMiB = %d, want 9000 — a reading inside the window is never discounted", got)
	}
}

// TestWithPeakAtLeastRaisesWindowToo: raising the all-time mark without raising
// the current window entry would let a deliberately-published peak decay away.
func TestWithPeakAtLeastRaisesWindowToo(t *testing.T) {
	p := Derive(Profile{}, SourceBench, 5000, 0, 1, 0, 1)
	p = p.WithPeakAtLeast(12000)
	if p.PeakMiB != 12000 {
		t.Errorf("PeakMiB = %d, want 12000", p.PeakMiB)
	}
	if n := len(p.RecentPeaks); n == 0 || p.RecentPeaks[n-1] != 12000 {
		t.Errorf("RecentPeaks tail = %v, want it raised to 12000", p.RecentPeaks)
	}
	// Fill the window with smaller readings; the raised value must survive
	// exactly as long as any other observation would, then expire.
	for i := 0; i < RecentWindow-1; i++ {
		p = Derive(p, SourceServing, 5000, 0, 1, 0, int64(i+2))
	}
	if got := p.EffectivePeakMiB(); got != 12000 {
		t.Errorf("EffectivePeakMiB = %d, want 12000 while still inside the window", got)
	}
	p = Derive(p, SourceServing, 5000, 0, 1, 0, 100)
	if got := p.EffectivePeakMiB(); got != 5000 {
		t.Errorf("EffectivePeakMiB = %d, want 5000 once it aged out", got)
	}
}

// TestRecentWindowBounded: the ring must not grow without limit — it is
// persisted on every spawn.
func TestRecentWindowBounded(t *testing.T) {
	p := Profile{}
	for i := 0; i < RecentWindow*3; i++ {
		p = Derive(p, SourceServing, 1000+i, 0, 1, 0, int64(i))
	}
	if len(p.RecentPeaks) != RecentWindow {
		t.Errorf("len(RecentPeaks) = %d, want %d", len(p.RecentPeaks), RecentWindow)
	}
}

// TestServingSampleKeepsWindowOnBenchProfile: the bench-precedence merge keeps
// the bench profile's SHAPE but must still carry the serving window forward.
// If it dropped it, a bench-measured model's window would never fill and the
// decay would never engage at all.
func TestServingSampleKeepsWindowOnBenchProfile(t *testing.T) {
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	bench := Derive(Profile{}, SourceBench, 9000, 0, 1, 0, 1)
	bench.BaseMiB, bench.PerSlotMiB = 8000, 500
	c.Update("GPU", "m", bench)
	for i := 0; i < RecentWindow; i++ {
		existing, _ := c.Get("GPU", "m")
		c.Update("GPU", "m", Derive(existing, SourceServing, 6000, 0, 1, 0, int64(i+2)))
	}
	got, _ := c.Get("GPU", "m")
	if got.BaseMiB != 8000 || got.PerSlotMiB != 500 {
		t.Errorf("serving overwrote bench shape: base %d perSlot %d", got.BaseMiB, got.PerSlotMiB)
	}
	if len(got.RecentPeaks) != RecentWindow {
		t.Fatalf("window did not accumulate across the merge: %v", got.RecentPeaks)
	}
	if e := got.EffectivePeakMiB(); e != 6000 {
		t.Errorf("EffectivePeakMiB = %d, want 6000", e)
	}
}
