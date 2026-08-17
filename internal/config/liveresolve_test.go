package config

import (
	"os"
	"testing"
)

// A check against the OPERATOR'S REAL CONFIG, skipped when it is not present.
//
// Unit tests use fixtures, which is what makes them stable and what makes them
// blind to "did the file on this machine actually get wired up". This closes
// that gap for the one claim worth being sure of: that the free lane resolves
// to its declared members AND the pool behind them.
func TestLiveConfigFreeLaneIncludesThePool(t *testing.T) {
	path := os.Getenv("HOME") + "/.corrallm/config.yml"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no live config on this machine")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("live config failed to load: %v", err)
	}
	var pooled []string
	for _, vt := range c.VirtualTargets() {
		if vt.Source != "openrouter" {
			continue
		}
		models := map[string]Model{}
		for _, id := range []string{"vendor/a:free", "vendor/b:free"} {
			n, m := c.VirtualModelFor(vt, id)
			models[n] = m
			pooled = append(pooled, n)
		}
		c.SetVirtualModels(vt.Virtual, vt.Source, vt.Credential, models)
	}
	if len(pooled) == 0 {
		t.Fatal("live config exposes no pool fetches for openrouter")
	}
	cands, ok := c.ResolveServed("free")
	if !ok {
		t.Fatal("the free lane did not resolve")
	}
	seen := map[string]bool{}
	var order []string
	for _, cd := range cands {
		if !seen[cd.Name] {
			seen[cd.Name] = true
			order = append(order, cd.Name)
		}
	}
	// Read the declared members from the lane itself rather than naming them
	// here. Hardcoding them made this test assert the CONTENTS of a config the
	// operator edits, so repointing a dead upstream broke it — `groq-llama-70b`
	// went away in c0b98eb and this failed for a config change that was
	// entirely correct. What is worth pinning is the INVARIANT: every declared
	// member survives resolution, and the declared ones come before the pool.
	var declared []string
	for _, m := range c.Lanes["free"].Members {
		if m.Model != "" {
			declared = append(declared, m.Model)
		}
	}
	if len(declared) == 0 {
		t.Fatal("the free lane declares no members by name")
	}
	for _, d := range declared {
		if !seen[d] {
			t.Errorf("declared member %q vanished from the free lane: %v", d, order)
		}
	}
	// The local floor was deliberately removed from THIS lane (2026-08-15) so
	// the pool is reached before the walk falls off the end. A declared member
	// is always tried before a pool, so leaving it in meant the pooled models
	// were only ever reached when the local box was unavailable too — backwards
	// for a lane whose purpose is spending someone else's quota first.
	if seen["Qwen3.8-27B"] {
		t.Errorf("the local floor is back in the free lane, so the pool is unreachable behind it: %v", order)
	}
	for _, p := range pooled {
		if !seen[p] {
			t.Errorf("pool member %q did not join the free lane: %v", p, order)
		}
	}
	// Declared members keep the front; the pool is appended.
	if len(order) > 0 && order[0] != declared[0] {
		t.Errorf("declared order disturbed, lane starts with %q, want the first declared member %q: %v",
			order[0], declared[0], order)
	}
}
