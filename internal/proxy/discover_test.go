package proxy

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/freeroster"
)

// Modelled on OpenRouter's live catalog, including the rows that make the
// naive "zero price means free chat model" test wrong.
func catalog() []freeroster.Entry {
	return []freeroster.Entry{
		{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 1000000},
		{ID: "inclusionai/ling-3.0-flash:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 262144},
		{ID: "google/gemma-4-26b-a4b-it:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 262144},
		{ID: "tiny/thing:free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 4096},
		// Priced at zero but NOT a chat model — the trap.
		{ID: "google/lyria-3-pro-preview", Free: true, InputModality: "text", OutputModality: "audio", ContextLength: 1048576},
		// A router pseudo-model.
		{ID: "openrouter/free", Free: true, InputModality: "text", OutputModality: "text", ContextLength: 0},
		// Perfectly good chat model, but paid.
		{ID: "anthropic/claude-opus-4", Free: false, InputModality: "text", OutputModality: "text", ContextLength: 200000},
	}
}

func target(spec *config.Discover) config.DiscoverTarget {
	return config.DiscoverTarget{Extension: "free", Provider: "openrouter", Spec: spec}
}

func TestDiscoveryFiltersToFreeChatModels(t *testing.T) {
	got := selectDiscovered(catalog(), target(&config.Discover{
		Filter: config.DiscoverFilter{
			Free: true, InputModality: "text", OutputModality: "text",
			MinContext: 8192, Exclude: []string{"openrouter/free"},
		},
	}))
	want := map[string]string{
		"openrouter-nvidia-nemotron-3-ultra-550b-a55b": "nvidia/nemotron-3-ultra-550b-a55b:free",
		"openrouter-inclusionai-ling-3.0-flash":        "inclusionai/ling-3.0-flash:free",
		"openrouter-google-gemma-4-26b-a4b-it":         "google/gemma-4-26b-a4b-it:free",
	}
	if len(got) != len(want) {
		t.Fatalf("kept %d models %v, want %d", len(got), keysOf(got), len(want))
	}
	for served, upstream := range want {
		m, ok := got[served]
		if !ok {
			t.Errorf("missing %s", served)
			continue
		}
		// The provider's own id must reach the provider, never the served name.
		if m.Upstream != upstream {
			t.Errorf("%s: upstream = %q, want %q", served, m.Upstream, upstream)
		}
		if m.Extension != "free" || m.ProviderName != "openrouter" {
			t.Errorf("%s: grouping = %s/%s", served, m.Extension, m.ProviderName)
		}
	}
	// The specific failures this filter exists to prevent.
	for _, bad := range []string{"openrouter-google-lyria-3-pro-preview", "openrouter-anthropic-claude-opus-4", "openrouter-free"} {
		if _, leaked := got[bad]; leaked {
			t.Errorf("%s should not have been discovered", bad)
		}
	}
}

// A cap keeps the biggest models, and the choice must not drift between
// refreshes when the provider reorders its catalog.
func TestDiscoveryLimitKeepsLargestDeterministically(t *testing.T) {
	spec := &config.Discover{
		Filter: config.DiscoverFilter{Free: true, InputModality: "text", OutputModality: "text", MinContext: 8192},
		Limit:  2,
	}
	first := selectDiscovered(catalog(), target(spec))
	if len(first) != 2 {
		t.Fatalf("kept %d, want 2", len(first))
	}
	if _, ok := first["openrouter-nvidia-nemotron-3-ultra-550b-a55b"]; !ok {
		t.Error("limit dropped the largest-context model")
	}
	rev := catalog()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	second := selectDiscovered(rev, target(spec))
	if len(second) != len(first) {
		t.Fatalf("ordering changed the result: %v vs %v", keysOf(first), keysOf(second))
	}
	for k := range first {
		if _, ok := second[k]; !ok {
			t.Errorf("unstable selection: %s present only in one pass", k)
		}
	}
}

// A discovered model must never redefine one the operator wrote down, and a
// model that churns out of the roster must disappear on the next pass.
func TestSetDiscoveredRespectsStaticAndReplaces(t *testing.T) {
	c := &config.Config{Models: map[string]config.Model{
		"openrouter-pinned": {Type: "chat", Quality: 9},
	}}
	c.SetDiscovered("openrouter", map[string]config.Model{
		"openrouter-pinned": {Type: "chat", Quality: 1, ProviderName: "openrouter"},
		"openrouter-a":      {Type: "chat", ProviderName: "openrouter"},
	})
	if c.AllModels()["openrouter-pinned"].Quality != 9 {
		t.Error("discovery overrode a declared model")
	}
	if _, ok := c.AllModels()["openrouter-a"]; !ok {
		t.Error("discovered model missing from AllModels")
	}
	// Next pass: "a" is gone from the provider's catalog.
	c.SetDiscovered("openrouter", map[string]config.Model{
		"openrouter-b": {Type: "chat", ProviderName: "openrouter"},
	})
	if _, stale := c.AllModels()["openrouter-a"]; stale {
		t.Error("a churned-out model survived the refresh")
	}
	if _, ok := c.AllModels()["openrouter-b"]; !ok {
		t.Error("new model not contributed")
	}
}

func keysOf(m map[string]config.Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The refresh loop only starts when this reports true, and discovery rides that
// loop. A provider whose models ALL come from discovery has nothing declared to
// trigger it — miss that and it serves nothing, silently.
func TestRosterRefreshStartsForDiscoverOnlyProvider(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
extensions:
  free:
    providers:
      openrouter:
        proxy: { host: openrouter.ai, port: 443, basePath: /api }
        discover:
          filter: { free: true }
          template: { type: chat }
`))
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{}
	p.cfg.Store(cfg) // cfg is an atomic.Pointer now (swapped on reload)
	if !p.HasRosterRefresh() {
		t.Fatal("refresh loop would not start; discovery would never run")
	}
}
