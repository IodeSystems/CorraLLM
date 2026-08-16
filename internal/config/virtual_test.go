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
