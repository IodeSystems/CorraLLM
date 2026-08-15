package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

const twoKeyProvider = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, headers: {x-shared: "1"}}
        provides:
          anchor: {type: chat, upstream: v/anchor}
        discover:
          filter: {free: true}
          template: {type: chat, quality: 3}
        credentials:
          - name: freekey
            headers: {authorization: "Bearer FREE"}
          - name: paidkey
            headers: {authorization: "Bearer PAID"}
`

func discCfg(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(twoKeyProvider), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func disc(provider string) Model {
	return Model{Type: "chat", Quality: 3, ProviderName: provider, Extension: "free"}
}

// TestDiscoveryRunsPerCredential: the catalogues differ by key, so one target
// per credential — each carrying that credential's auth.
func TestDiscoveryRunsPerCredential(t *testing.T) {
	c := discCfg(t)
	targets := c.DiscoverTargets()
	if len(targets) != 2 {
		t.Fatalf("want one discover target per credential, got %d", len(targets))
	}
	seen := map[string]string{}
	for _, dt := range targets {
		seen[dt.Credential] = dt.Target.Headers["authorization"]
		if dt.Target.Headers["x-shared"] != "1" {
			t.Errorf("%s lost the provider's shared header", dt.Credential)
		}
	}
	if seen["freekey"] != "Bearer FREE" || seen["paidkey"] != "Bearer PAID" {
		t.Errorf("targets carry the wrong auth: %+v", seen)
	}
}

// TestModelOfferedOnlyOnCredentialsThatSawIt is the failure this prevents:
// recording a union would offer a paid-only model on the free key, and every
// request routed there would 404 — looking like provider flakiness rather than
// a config error.
func TestModelOfferedOnlyOnCredentialsThatSawIt(t *testing.T) {
	c := discCfg(t)
	c.SetDiscoveredFor("openrouter", "freekey", map[string]Model{
		"openrouter-common": disc("openrouter"),
		"openrouter-freeonly": disc("openrouter"),
	})
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{
		"openrouter-common": disc("openrouter"),
		"openrouter-paidonly": disc("openrouter"),
	})

	creds := func(served string) []string {
		cands, ok := c.ResolveServed(served)
		if !ok {
			t.Fatalf("%s did not resolve", served)
		}
		var out []string
		for _, cd := range cands {
			if cd.Credential != nil {
				out = append(out, cd.Credential.Name)
			}
		}
		return out
	}
	if got := creds("openrouter-freeonly"); len(got) != 1 || got[0] != "freekey" {
		t.Errorf("free-only model offered on %v, want [freekey]", got)
	}
	if got := creds("openrouter-paidonly"); len(got) != 1 || got[0] != "paidkey" {
		t.Errorf("paid-only model offered on %v, want [paidkey]", got)
	}
	if got := creds("openrouter-common"); len(got) != 2 {
		t.Errorf("a model both keys saw should be reachable by both, got %v", got)
	}
}

// TestOneCredentialRefreshDoesNotEraseAnother: retraction is scoped to the
// credential that is refreshing, or two accounts refreshing in turn would each
// wipe the other's contribution.
func TestOneCredentialRefreshDoesNotEraseAnother(t *testing.T) {
	c := discCfg(t)
	c.SetDiscoveredFor("openrouter", "freekey", map[string]Model{"openrouter-a": disc("openrouter")})
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{"openrouter-b": disc("openrouter")})
	got := c.Discovered()
	if _, ok := got["openrouter-a"]; !ok {
		t.Error("freekey's model was erased by paidkey's refresh")
	}
	if _, ok := got["openrouter-b"]; !ok {
		t.Error("paidkey's model missing")
	}
}

// TestChurnRetractsOnlyWhenNoCredentialSeesIt: a model that leaves ONE key's
// catalogue is still reachable by the other; it disappears only when nobody
// sees it.
func TestChurnRetractsOnlyWhenNoCredentialSeesIt(t *testing.T) {
	c := discCfg(t)
	both := map[string]Model{"openrouter-x": disc("openrouter")}
	c.SetDiscoveredFor("openrouter", "freekey", both)
	c.SetDiscoveredFor("openrouter", "paidkey", both)

	// churns out of the free tier only
	c.SetDiscoveredFor("openrouter", "freekey", map[string]Model{})
	if _, ok := c.Discovered()["openrouter-x"]; !ok {
		t.Fatal("model vanished though paidkey still sees it")
	}
	cands, _ := c.ResolveServed("openrouter-x")
	if len(cands) != 1 || cands[0].Credential.Name != "paidkey" {
		t.Errorf("want it reachable only by paidkey, got %d candidates", len(cands))
	}

	// now it leaves the paid catalogue too
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{})
	if _, ok := c.Discovered()["openrouter-x"]; ok {
		t.Error("model should be gone once no credential sees it")
	}
}

// TestSetDiscoveredKeepsSingleCredentialBehaviour: the pre-P21 entry point
// still works, so a provider with no declared credentials is unaffected.
func TestSetDiscoveredKeepsSingleCredentialBehaviour(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443}
        provides: {anchor: {type: chat, upstream: v/a}}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	c.SetDiscovered("groq", map[string]Model{"groq-x": disc("groq")})
	cands, ok := c.ResolveServed("groq-x")
	if !ok || len(cands) != 1 {
		t.Fatalf("want one candidate, got %d", len(cands))
	}
	if cands[0].Credential != nil {
		t.Errorf("no declared credentials, so none should be attached: %+v", cands[0].Credential)
	}
}
