package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func laneCfg(t *testing.T, body string) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

const twoProviders = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides:
          big: {type: chat, quality: 5, upstream: v/big}
          small: {type: chat, quality: 1, upstream: v/small}
      groq:
        proxy: {host: api.groq.com, port: 443}
        provides:
          l70: {type: chat, quality: 3, upstream: v/l70}
lanes:
  free:
    members:
      - groq-l70
      - {provider: openrouter}
`

// TestSelectorExpandsProviderModels: the lane names a provider, not ids, so a
// roster that churns does not need the lane rewritten.
func TestSelectorExpandsProviderModels(t *testing.T) {
	c := laneCfg(t, twoProviders)
	cands, ok := c.ResolveServed("free")
	if !ok {
		t.Fatal("lane did not resolve")
	}
	var names []string
	for _, cd := range cands {
		names = append(names, cd.Name)
	}
	// Explicit member keeps its declared position; the selector expands in place.
	want := []string{"groq-l70", "openrouter-big", "openrouter-small"}
	if len(names) != len(want) {
		t.Fatalf("members = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("members = %v, want %v", names, want)
		}
	}
}

// TestSelectorOrdersByQualityThenName: an expansion whose order came from map
// iteration would reshuffle the fallback ladder on every restart.
func TestSelectorOrdersByQualityThenName(t *testing.T) {
	c := laneCfg(t, twoProviders)
	for i := 0; i < 20; i++ {
		cands, _ := c.ResolveServed("free")
		if cands[1].Name != "openrouter-big" || cands[2].Name != "openrouter-small" {
			t.Fatalf("iteration %d reshuffled: %s, %s", i, cands[1].Name, cands[2].Name)
		}
	}
}

// TestSelectorRespectsMinQuality: a selector need not accept everything a
// provider publishes.
func TestSelectorRespectsMinQuality(t *testing.T) {
	c := laneCfg(t, `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides:
          big: {type: chat, quality: 5, upstream: v/big}
          small: {type: chat, quality: 1, upstream: v/small}
lanes:
  free:
    members:
      - {provider: openrouter, minQuality: 3}
`)
	cands, _ := c.ResolveServed("free")
	if len(cands) != 1 || cands[0].Name != "openrouter-big" {
		t.Errorf("members = %+v, want only openrouter-big", cands)
	}
}

// TestSelectorSurvivesValidation is the point of the whole change: a lane whose
// membership is a selector must LOAD, where a named discovered model cannot.
func TestSelectorSurvivesValidation(t *testing.T) {
	c := laneCfg(t, twoProviders)
	if err := c.Validate(); err != nil {
		t.Fatalf("selector membership must validate at load: %v", err)
	}
}

// TestNamedUnknownMemberStillRejected: relaxing the check for selectors must
// not relax it for names — a misspelled member is still a config error, which
// is exactly what the "tolerate unknown names" alternative would have lost.
func TestNamedUnknownMemberStillRejected(t *testing.T) {
	c := laneCfg(t, `
models:
  real: {proxy: 9000, type: chat}
lanes:
  l:
    members: [real, typoo]
`)
	err := c.Validate()
	if err == nil {
		t.Fatal("an unknown NAMED member must still fail validation")
	}
}

// TestSelectorPicksUpDiscoveredModels: the case named membership cannot serve
// at all — models that appear after load, on the refresh loop.
func TestSelectorPicksUpDiscoveredModels(t *testing.T) {
	c := laneCfg(t, twoProviders)
	before, _ := c.ResolveServed("free")
	c.SetDiscovered("openrouter", map[string]Model{
		"openrouter-new": {Type: "chat", Quality: 4, ProviderName: "openrouter", Extension: "free"},
	})
	after, _ := c.ResolveServed("free")
	if len(after) != len(before)+1 {
		t.Fatalf("discovered model did not join the lane: %d -> %d", len(before), len(after))
	}
	// quality 4 sits between big(5) and small(1)
	if after[2].Name != "openrouter-new" {
		var names []string
		for _, cd := range after {
			names = append(names, cd.Name)
		}
		t.Errorf("ordering = %v, want openrouter-new third by quality", names)
	}
}

// localAndRemote is the case the goal names: the same model family reachable
// locally, on an attached host, and via a paid remote. They are DIFFERENT
// backends — different latency, cost and failure modes — and a lane exists to
// order them, not to pool them.
const localAndRemote = `
servers:
  box1: {pools: {gpu0: 32GB, gpu1: 10GB, system: 125GB}}
  mac1: {pools: {system: 64GB}}
models:
  qwen-local:
    cmd: llama-server
    server: box1
    proxy: 5800
    type: chat
    quality: 5
    ramUsage: {gpu0: 30GB}
  qwen-local-small:
    cmd: llama-server
    server: box1
    proxy: 5802
    type: chat
    quality: 3
    ramUsage: {gpu1: 8GB}
  qwen-mac:
    cmd: llama-server
    server: mac1
    proxy: 5810
    type: chat
    quality: 4
    ramUsage: {system: 34GB}
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides:
          qwen: {type: chat, quality: 2, upstream: v/qwen}
`

// TestSelectorScopesByHost: "the local copy first, then the attached box, then
// the remote" — which is meaningless if a selector cannot tell hosts apart.
func TestSelectorScopesByHost(t *testing.T) {
	c := laneCfg(t, localAndRemote+`
lanes:
  chat:
    members:
      - {server: box1}
      - {server: mac1}
      - {provider: openrouter}
`)
	cands, ok := c.ResolveServed("chat")
	if !ok {
		t.Fatal("lane did not resolve")
	}
	var got []string
	for _, cd := range cands {
		got = append(got, cd.Name)
	}
	want := []string{"qwen-local", "qwen-local-small", "qwen-mac", "openrouter-qwen"}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("members = %v, want %v — declared member order IS lane priority", got, want)
		}
	}
}

// TestSelectorDoesNotConfuseLocalAndRemote: the explicit goal. A host-scoped
// selector must not sweep in the remote copy, however similar the model.
func TestSelectorDoesNotConfuseLocalAndRemote(t *testing.T) {
	c := laneCfg(t, localAndRemote+`
lanes:
  localonly:
    members: [{server: box1}]
`)
	cands, _ := c.ResolveServed("localonly")
	for _, cd := range cands {
		if cd.Model.ProviderName == "openrouter" {
			t.Errorf("a host-scoped selector picked up the remote %q", cd.Name)
		}
		if cd.Model.Server != "box1" {
			t.Errorf("%q is on server %q, not box1", cd.Name, cd.Model.Server)
		}
	}
	if len(cands) != 2 {
		t.Errorf("want both box1 models, got %d", len(cands))
	}
}

// TestSelectorScopesByDevice: two cards on one box are not interchangeable —
// a 5090 and a 3080 differ ~3x in bandwidth — so "the local copy" is sometimes
// a per-device statement.
func TestSelectorScopesByDevice(t *testing.T) {
	c := laneCfg(t, localAndRemote+`
lanes:
  fastcard:
    members: [{server: box1, device: gpu0}]
`)
	cands, _ := c.ResolveServed("fastcard")
	if len(cands) != 1 || cands[0].Name != "qwen-local" {
		var got []string
		for _, cd := range cands {
			got = append(got, cd.Name)
		}
		t.Errorf("members = %v, want only qwen-local (the gpu0 one)", got)
	}
}

// TestSelectorCombinesScopes: provider AND server both set means both must
// match, so a provider reachable from two hosts stays separable.
func TestSelectorCombinesScopes(t *testing.T) {
	c := laneCfg(t, localAndRemote+`
lanes:
  none:
    members: [{provider: openrouter, server: box1}]
`)
	cands, ok := c.ResolveServed("none")
	if ok && len(cands) > 0 {
		t.Errorf("openrouter is not on box1; want no members, got %d", len(cands))
	}
}
