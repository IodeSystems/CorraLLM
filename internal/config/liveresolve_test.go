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
	for _, declared := range []string{"groq-llama-70b", "cerebras-gpt-oss-120b", "Qwen3.8-27B"} {
		if !seen[declared] {
			t.Errorf("declared member %q vanished from the free lane: %v", declared, order)
		}
	}
	for _, p := range pooled {
		if !seen[p] {
			t.Errorf("pool member %q did not join the free lane: %v", p, order)
		}
	}
	// Declared members keep the front; the pool is appended.
	if len(order) > 0 && order[0] != "groq-llama-70b" {
		t.Errorf("declared order disturbed, lane starts with %q: %v", order[0], order)
	}
}
