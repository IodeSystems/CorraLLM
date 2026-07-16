// Package tune learns each model's empirical VRAM footprint shape (weights,
// per-slot KV cost, observed peak) from spawned processes (internal/proc) and
// sizes --parallel for the model's NEXT spawn against whatever VRAM is
// actually free. It is purely additive: a Cache with no entry for a
// (gpu, model) pair, or asked for slots it can't compute, returns ok=false —
// callers (internal/proc) treat that exactly like introspection being
// unavailable and leave the model's configured cmd untouched. This package
// never imports internal/proc (proc calls into tune, not the reverse).
package tune

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultCap bounds SlotsFor's output against a runaway budget/measurement
// computing an absurd slot count.
const DefaultCap = 32

// Profile is one (gpu, model) pair's measured VRAM footprint shape.
type Profile struct {
	BaseMiB       int // process footprint with KV excluded (weights + fixed overhead)
	PerSlotMiB    int // KV cache cost per slot (measured KV total / MeasuredSlots, or slope-derived — see Samples)
	PeakMiB       int // highest total process footprint ever observed
	MeasuredSlots int // n_slots the measurement was taken at
	Ctx           int // n_ctx at measurement time

	// Samples holds up to the two most-recent-DISTINCT (slots, footprint)
	// spawn measurements. It exists for hosts where llama.cpp logs no KV
	// cache size, so BaseMiB/PerSlotMiB can't be split out of a single
	// spawn's footprint directly: with two spawns at different --parallel,
	// the SLOPE of footprint vs slots gives PerSlotMiB (and the intercept
	// gives BaseMiB) empirically — see SlopeFromSamples. When the KV log IS
	// available, BaseMiB/PerSlotMiB are computed directly (the cheaper, more
	// precise path) and Samples is still recorded but not needed. A profile
	// decoded from before this field existed simply has Samples == nil,
	// which SlopeFromSamples treats as "not enough data" — no behavior
	// change for already-tuned (KV-log-derived) profiles.
	Samples []Sample
}

// Sample is one spawn's empirical VRAM measurement: total observed process
// footprint at a specific slot (--parallel) count.
type Sample struct {
	Slots        int
	FootprintMiB int
}

// MergeSample folds a fresh (slots, footprint) measurement into samples,
// keeping at most the two most-recent-distinct slot counts (index 0 older,
// index 1 newer). A sample at a slot count already present refreshes that
// entry in place (repeat measurement / noise) rather than growing the set.
// Always returns a new slice — never mutates samples' backing array, so a
// caller holding a Profile read from the cache can pass its Samples in
// without risking a concurrent reader observing a half-updated slice.
func MergeSample(samples []Sample, s Sample) []Sample {
	out := make([]Sample, 0, 2)
	replaced := false
	for _, e := range samples {
		if e.Slots == s.Slots {
			out = append(out, s)
			replaced = true
		} else {
			out = append(out, e)
		}
	}
	if replaced {
		return out
	}
	if len(out) < 2 {
		return append(out, s)
	}
	// Already two distinct slot counts: drop the older (index 0), keep the
	// most-recent-distinct pair.
	return []Sample{out[1], s}
}

// SlopeFromSamples derives PerSlotMiB/BaseMiB from the two furthest-apart
// distinct slot counts in samples:
//
//	perSlot = (f2 - f1) / (s2 - s1)
//	base    = f1 - s1*perSlot        (clamped >= 0)
//
// ok=false when samples doesn't hold at least two distinct slot counts, or
// the slope would be negative — a noisy/invalid pair that must never be
// trusted to size slots (negative per-slot cost would make SlotsFor
// overcommit VRAM as slot count grows).
func SlopeFromSamples(samples []Sample) (perSlot, base int, ok bool) {
	var lo, hi Sample
	found := false
	for i := 0; i < len(samples); i++ {
		for j := i + 1; j < len(samples); j++ {
			a, b := samples[i], samples[j]
			if a.Slots == b.Slots {
				continue
			}
			if a.Slots > b.Slots {
				a, b = b, a
			}
			if !found || b.Slots-a.Slots > hi.Slots-lo.Slots {
				lo, hi, found = a, b, true
			}
		}
	}
	if !found {
		return 0, 0, false
	}
	perSlot = (hi.FootprintMiB - lo.FootprintMiB) / (hi.Slots - lo.Slots)
	if perSlot < 0 {
		return 0, 0, false
	}
	base = lo.FootprintMiB - lo.Slots*perSlot
	if base < 0 {
		base = 0
	}
	return perSlot, base, true
}

// Cache is a persisted, concurrency-safe table of VRAM profiles keyed by
// (gpuName, model). It is the fail-safe boundary: every read either finds a
// concrete, computable profile or reports ok=false.
type Cache struct {
	path string

	mu   sync.Mutex
	data map[string]Profile
}

func key(gpuName, model string) string {
	return gpuName + "\x1f" + model
}

// New loads path into a Cache. A missing or empty file yields an empty cache
// — not an error — so a fresh box (no profiles measured yet) boots exactly
// like introspection is disabled.
func New(path string) (*Cache, error) {
	c := &Cache{path: path, data: map[string]Profile{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tune cache %s: %w", path, err)
	}
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c.data); err != nil {
		return nil, fmt.Errorf("parse tune cache %s: %w", path, err)
	}
	return c, nil
}

// Save persists the cache as JSON, creating the parent directory if needed. A
// Cache constructed with an empty path (e.g. introspect's read-only use) is a
// no-op — nothing to write to.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *Cache) saveLocked() error {
	if c.path == "" {
		return nil
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o644)
}

// Get returns the profile for (gpuName, model), if any — used by introspect
// to report raw measurements alongside the live SlotsFor computation.
func (c *Cache) Get(gpuName, model string) (Profile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.data[key(gpuName, model)]
	return p, ok
}

// SlotsFor computes how many concurrent slots (gpuName, model)'s profile
// supports within budgetMiB of free VRAM. ok=false means "no profile" (or a
// degenerate one, PerSlotMiB<=0) — the fail-safe caller then leaves the
// model's cmd/config untouched.
//
// growth accounts for the gap already observed between the resting footprint
// (BaseMiB + MeasuredSlots*PerSlotMiB) and the highest footprint ever seen
// (PeakMiB) — batching, long-context requests, and allocator slack that a
// pure per-slot KV estimate would miss. It is subtracted from the budget
// before dividing into slots, so tuning stays conservative relative to the
// worst footprint actually observed, not just the tidy accounting model.
//
// A budget that can't even fit one slot's worth still returns 1 (floored,
// not refused) — a model that doesn't fit at all is a scheduling/eviction
// problem elsewhere, not this function's job.
func (c *Cache) SlotsFor(gpuName, model string, budgetMiB int) (int, bool) {
	p, ok := c.Get(gpuName, model)
	if !ok || p.PerSlotMiB <= 0 {
		return 0, false
	}
	growth := p.PeakMiB - (p.BaseMiB + p.MeasuredSlots*p.PerSlotMiB)
	if growth < 0 {
		growth = 0
	}
	n := (budgetMiB - p.BaseMiB - growth) / p.PerSlotMiB
	return clamp(n, 1, DefaultCap), true
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// Update records a fresh measurement for (gpuName, model). PeakMiB only ever
// grows (max of the existing and new value) — the highest footprint observed
// across the model's life is what SlotsFor's growth term needs, and a single
// spawn's snapshot at health-check time may not have hit it yet (see the
// periodic sampler in internal/proc, which calls BumpPeak between full
// measurements). Base/PerSlot/MeasuredSlots/Ctx take the LATEST measurement
// unconditionally: a config or quant change invalidates the old numbers, and
// there's no cheap way to distinguish "same model, fresh sample" from
// "model's shape changed" other than trusting the newest one. Does not
// persist — callers call Save() explicitly (measure-on-load treats a failed
// Save as non-fatal but still wants the in-memory cache updated either way).
func (c *Cache) Update(gpuName, model string, p Profile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key(gpuName, model)
	if existing, ok := c.data[k]; ok && existing.PeakMiB > p.PeakMiB {
		p.PeakMiB = existing.PeakMiB
	}
	c.data[k] = p
}

// BumpPeak raises an EXISTING profile's PeakMiB to max(current, footprint). A
// no-op if no profile exists yet for (gpuName, model) — the periodic
// runtime sampler must not synthesize a profile with only a peak and no
// Base/PerSlot; that's Update's job, seeded from an exact spawn-time
// measurement.
func (c *Cache) BumpPeak(gpuName, model string, footprintMiB int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key(gpuName, model)
	p, ok := c.data[k]
	if !ok || footprintMiB <= p.PeakMiB {
		return
	}
	p.PeakMiB = footprintMiB
	c.data[k] = p
}
