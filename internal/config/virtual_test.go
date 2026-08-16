package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// `free` as the real config means it: an extension that IS a provider, pooling
// its members' catalogues and reaching each model with that member's key.
const virtualCfg = `
extensions:
  free:
    virtual:
      filter: {free: true, inputModality: text, outputModality: text, minContext: 8192}
      template: {type: chat, quality: 3}
      limit: 12
      lanes: [{lane: freepool, order: 60}]
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, basePath: /api}
        credentials:
          - name: paid
            headers: {authorization: "Bearer PAID"}
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
lanes:
  freepool:
    members: [local-floor]
models:
  local-floor:
    type: chat
    quality: 2
    proxy: {host: 127.0.0.1, port: 8080}
`

func virtCfg(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(virtualCfg), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func poolTarget(c *Config, source string) VirtualTarget {
	for _, vt := range c.VirtualTargets() {
		if vt.Source == source {
			return vt
		}
	}
	panic("no target for " + source)
}

// TestPooledModelKeepsItsSourceName is the identity decision, and the reason
// quota, cost and metrics keep working: membership in a pool is a fact ABOUT a
// model, not a second model.
func TestPooledModelKeepsItsSourceName(t *testing.T) {
	c := virtCfg(t)
	served, m := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/thing:free")
	if served != "openrouter-vendor-thing" {
		t.Errorf("served name = %q, want the SOURCE provider's name", served)
	}
	if m.ProviderName != "openrouter" {
		t.Errorf("provider = %q, want the member that actually serves it", m.ProviderName)
	}
	if m.Extension != "free" {
		t.Errorf("extension = %q, want the pool it was drawn into", m.Extension)
	}
	if m.Upstream != "vendor/thing:free" {
		t.Errorf("upstream = %q — the provider's own id must reach the wire", m.Upstream)
	}
	if m.Type != "chat" || m.Quality != 3 {
		t.Errorf("pool template lost: type=%q quality=%v", m.Type, m.Quality)
	}
}

// TestPoolBorrowsItsMembersKeys: a virtual extension holds no credential. Every
// fetch, and every request, goes out with the key of the member serving it.
func TestPoolBorrowsItsMembersKeys(t *testing.T) {
	c := virtCfg(t)
	got := map[string]string{}
	for _, vt := range c.VirtualTargets() {
		got[vt.Source] = vt.Target.Headers["authorization"]
	}
	if got["openrouter"] != "Bearer PAID" {
		t.Errorf("openrouter fetch did not carry its own key: %q", got["openrouter"])
	}
	if _, ok := got["groq"]; !ok {
		t.Error("a member with no explicit credential still needs a fetch (its implicit default)")
	}
}

// TestPoolFeedsItsLane is what makes the pool reachable as ONE thing: ask for
// the lane, get the pool's members behind the lane's declared floor.
func TestPoolFeedsItsLane(t *testing.T) {
	c := virtCfg(t)
	served, m := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/thing:free")
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{served: m})

	cands, ok := c.ResolveServed("freepool")
	if !ok {
		t.Fatal("lane did not resolve")
	}
	var names []string
	for _, cd := range cands {
		names = append(names, cd.Name)
	}
	if len(names) == 0 || names[0] != "local-floor" {
		t.Errorf("declared member lost the front of the ladder: %v", names)
	}
	found := false
	for _, n := range names {
		if n == served {
			found = true
		}
	}
	if !found {
		t.Errorf("pool member did not join the lane it was placed in: %v", names)
	}
}

// TestPoolChurnDropsAMemberFromTheLane: a provider withdrawing a free model
// must take it out of the lane, without disturbing the rest of the pool. This
// is the churn the whole mechanism exists for.
func TestPoolChurnDropsAMemberFromTheLane(t *testing.T) {
	c := virtCfg(t)
	tgt := poolTarget(c, "openrouter")
	aName, aModel := c.VirtualModelFor(tgt, "vendor/a:free")
	bName, bModel := c.VirtualModelFor(tgt, "vendor/b:free")
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{aName: aModel, bName: bModel})

	inLane := func() map[string]bool {
		out := map[string]bool{}
		cands, _ := c.ResolveServed("freepool")
		for _, cd := range cands {
			out[cd.Name] = true
		}
		return out
	}
	if got := inLane(); !got[aName] || !got[bName] {
		t.Fatalf("precondition: both pooled models should be in the lane, got %v", got)
	}
	// Next refresh: the provider went paid on A.
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{bName: bModel})
	got := inLane()
	if got[aName] {
		t.Error("a model that left the pool is still routed to")
	}
	if !got[bName] {
		t.Error("churn on one model took the rest of the pool down with it")
	}
	if !got["local-floor"] {
		t.Error("the lane's declared floor was disturbed by pool churn")
	}
}

// TestOneMemberFailingDoesNotEmptyThePool: contributions are per member, so a
// provider that is down leaves the others serving.
func TestOneMemberFailingDoesNotEmptyThePool(t *testing.T) {
	c := virtCfg(t)
	orName, orModel := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/a:free")
	gqName, gqModel := c.VirtualModelFor(poolTarget(c, "groq"), "vendor/b:free")
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{orName: orModel})
	c.SetVirtualModels("free", "groq", DefaultCredentialName, map[string]Model{gqName: gqModel})

	// groq refreshes to empty (or simply is not refreshed); openrouter stands.
	c.SetVirtualModels("free", "groq", DefaultCredentialName, map[string]Model{})
	if _, ok := c.Discovered()[orName]; !ok {
		t.Error("one member's empty refresh emptied another member's contribution")
	}
	if _, ok := c.Discovered()[gqName]; ok {
		t.Error("the withdrawing member's model is still contributed")
	}
}

// TestPoolSurvivesAConfigReload is the regression that a SIGHUP exposed on the
// live box: a reload builds a NEW Config, the pool lives on the old one, and
// without adoption a one-line YAML edit empties every pool until the next
// refresh tick — 30 minutes at the default interval.
func TestPoolSurvivesAConfigReload(t *testing.T) {
	old := virtCfg(t)
	served, m := old.VirtualModelFor(poolTarget(old, "openrouter"), "vendor/thing:free")
	old.SetVirtualModels("free", "openrouter", "paid", map[string]Model{served: m})
	if _, ok := old.Discovered()[served]; !ok {
		t.Fatal("precondition: the pool should be populated")
	}

	fresh := virtCfg(t) // what Load() produces after the file is edited
	if _, ok := fresh.Discovered()[served]; ok {
		t.Fatal("precondition: a freshly loaded config starts with an empty overlay")
	}
	fresh.AdoptRuntime(old)
	if _, ok := fresh.Discovered()[served]; !ok {
		t.Error("the pool did not survive the reload")
	}
	// And it must still be in its lane, not merely in the registry.
	cands, _ := fresh.ResolveServed("freepool")
	var found bool
	for _, cd := range cands {
		if cd.Name == served {
			found = true
		}
	}
	if !found {
		t.Error("the pool survived the reload but stopped feeding its lane")
	}
}

// TestAdoptDropsContributionsFromDeletedProviders: carrying the overlay across
// must not resurrect a provider the edit removed. For a deleted provider no
// refresh ever comes to retract it, so it would serve forever.
func TestAdoptDropsContributionsFromDeletedProviders(t *testing.T) {
	old := virtCfg(t)
	served, m := old.VirtualModelFor(poolTarget(old, "openrouter"), "vendor/thing:free")
	old.SetVirtualModels("free", "openrouter", "paid", map[string]Model{served: m})

	// The edit removed the whole virtual extension.
	var fresh Config
	if err := yaml.Unmarshal([]byte(`
models:
  local-floor:
    type: chat
    quality: 2
    proxy: {host: 127.0.0.1, port: 8080}
`), &fresh); err != nil {
		t.Fatal(err)
	}
	if err := fresh.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	fresh.AdoptRuntime(old)
	if _, ok := fresh.Discovered()[served]; ok {
		t.Error("a deleted extension's models were carried across and would serve forever")
	}
}

// TestLaneLadderReportsEveryOrigin is what the Overview panel renders. The
// panel used to show `lane.Members` and call it the lane, so `free` displayed
// two entries while resolving to twelve — the one question it exists to answer,
// "what will this try", had a wrong answer on screen.
func TestLaneLadderReportsEveryOrigin(t *testing.T) {
	c := virtCfg(t)
	pooled, m := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/pooled:free")
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{pooled: m})
	chosen, cm := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/chosen")
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paid", Model: chosen,
		Upstream: cm.Upstream, Lanes: []LanePlacement{{Lane: "freepool", Order: 5}},
	}})

	ladder := c.LaneLadder("freepool")
	got := map[string]LaneEntry{}
	var order []string
	for _, e := range ladder {
		got[e.Name] = e
		order = append(order, e.Name)
	}
	if len(order) != 3 {
		t.Fatalf("ladder = %v, want the declared floor plus the selection plus the pool", order)
	}
	if order[0] != "local-floor" {
		t.Errorf("declared member lost the front: %v", order)
	}
	if got["local-floor"].Origin != "declared" {
		t.Errorf("local-floor origin = %q, want declared", got["local-floor"].Origin)
	}
	if got[chosen].Origin != "selection" {
		t.Errorf("%s origin = %q, want selection", chosen, got[chosen].Origin)
	}
	if got[pooled].Origin != "pool" || got[pooled].Pool != "free" {
		t.Errorf("%s origin = %q/%q, want pool/free", pooled, got[pooled].Origin, got[pooled].Pool)
	}
}

// TestLaneLadderDedupesToFirstPosition: a model reachable two ways is tried
// once, at its earliest rung, and the ladder must say so rather than listing it
// twice — a panel that double-counts is a panel that misreports depth.
func TestLaneLadderDedupesToFirstPosition(t *testing.T) {
	c := virtCfg(t)
	name, m := c.VirtualModelFor(poolTarget(c, "openrouter"), "vendor/both:free")
	c.SetVirtualModels("free", "openrouter", "paid", map[string]Model{name: m})
	// The same model ALSO chosen by hand into the same lane.
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paid", Model: name,
		Lanes: []LanePlacement{{Lane: "freepool", Order: 1}},
	}})
	seen := 0
	var origin string
	for _, e := range c.LaneLadder("freepool") {
		if e.Name == name {
			seen++
			origin = e.Origin
		}
	}
	if seen != 1 {
		t.Errorf("model appears %d times in the ladder, want once", seen)
	}
	// Selections are added before pools, so the earlier rung wins.
	if origin != "selection" {
		t.Errorf("origin = %q, want the earlier rung (selection)", origin)
	}
}
