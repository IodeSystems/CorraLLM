package api

import (
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// providerCfg is the shape a real box has: models are AUTHORED under a
// provider, and c.Models is the flat index built from them at load.
func providerCfg() *config.Config {
	c := &config.Config{
		Scheduler: config.SchedulerConfig{MaxWait: "15s", MaxQueueDepth: 8},
		Providers: map[string]config.LocalProvider{
			"local": {Models: map[string]config.Model{
				"Qwen3.8-27B": {MaxConcurrent: 1},
			}},
		},
		Models: map[string]config.Model{},
	}
	resolved := c.Providers["local"].Models["Qwen3.8-27B"]
	resolved.ProviderName = "local"
	c.Models["local-Qwen3.8-27B"] = resolved
	return c
}

// The bug this test exists for: the first cut wrote only c.Models, the mutation
// reported success, and the setting vanished at the next save because the store
// re-derives from the authored blocks. Asserting on the in-memory view alone
// would have passed.
func TestQueueDepthLandsWhereTheModelIsAuthored(t *testing.T) {
	c := providerCfg()
	if err := editAuthoredModel(c, "local-Qwen3.8-27B", func(m *config.Model) {
		m.Scheduler = &config.SchedulerConfig{MaxQueueDepth: 3}
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	authored := c.Providers["local"].Models["Qwen3.8-27B"]
	if authored.Scheduler == nil || authored.Scheduler.MaxQueueDepth != 3 {
		t.Errorf("authored entry = %+v, want the override written under the provider", authored.Scheduler)
	}
	// And the running view agrees, because that is what the scheduler reads
	// until the next reload.
	if got := c.SchedulerFor("local-Qwen3.8-27B"); got.MaxQueueDepth != 3 {
		t.Errorf("resolved = %+v, want depth 3", got)
	}
	// The inherited field is untouched.
	if got := c.SchedulerFor("local-Qwen3.8-27B"); got.MaxWait != "15s" {
		t.Errorf("maxWait = %q, want the inherited 15s", got.MaxWait)
	}
}

// The two copies must not share a pointer, or clearing one silently clears the
// other and the config disagrees with itself.
func TestAuthoredAndResolvedOverridesAreSeparate(t *testing.T) {
	c := providerCfg()
	if err := editAuthoredModel(c, "local-Qwen3.8-27B", func(m *config.Model) {
		m.Scheduler = &config.SchedulerConfig{MaxQueueDepth: 3}
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	authored := c.Providers["local"].Models["Qwen3.8-27B"]
	resolved := c.Models["local-Qwen3.8-27B"]
	if authored.Scheduler == resolved.Scheduler {
		t.Error("authored and resolved share one *SchedulerConfig; editing either would change both")
	}
}

// An extension's own provides is derived from the extension, so editing the
// derived copy would be overwritten on the next reload. Refused, not half-done.
func TestQueueDepthRefusesAnExtensionProvidedModel(t *testing.T) {
	c := &config.Config{
		Models: map[string]config.Model{
			"oidio-stt": {Extension: "oidio"},
		},
	}
	err := editAuthoredModel(c, "oidio-stt", func(m *config.Model) {
		m.Scheduler = &config.SchedulerConfig{MaxQueueDepth: 2}
	})
	if err == nil {
		t.Fatal("editing an extension-provided model was allowed; it would vanish on reload")
	}
	if c.Models["oidio-stt"].Scheduler != nil {
		t.Error("the refusal still mutated the model")
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	c := providerCfg()
	if err := editAuthoredModel(c, "nope", func(*config.Model) {}); err == nil {
		t.Error("editing an unknown model was allowed")
	}
}
