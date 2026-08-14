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
