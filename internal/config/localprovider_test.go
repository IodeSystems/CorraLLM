package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

const localProviderCfg = `
servers:
  box1:
    pools: {gpu0: 24GB}
providers:
  local:
    models:
      Qwen3.8-27B:
        cmd: llama-server --model qwen.gguf --port 5801
        server: box1
        type: chat
        quality: 5
        ramUsage: {gpu0: 20GB}
        proxy: {host: 127.0.0.1, port: 5801}
      nomic-embed-text:
        cmd: llama-server --model nomic.gguf --port 5811
        server: box1
        type: embed
        quality: 1
        ramUsage: {gpu0: 1GB}
        proxy: {host: 127.0.0.1, port: 5811}
lanes:
  chat:
    members: [local-Qwen3.8-27B]
`

func localCfg(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(localProviderCfg), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

// TestLocalProviderModelsArePrefixed: local stops being the special case, so
// its models are named like every other provider's.
func TestLocalProviderModelsArePrefixed(t *testing.T) {
	c := localCfg(t)
	m, ok := c.Models["local-Qwen3.8-27B"]
	if !ok {
		t.Fatalf("model not folded under its prefixed name: %v", keys(c.Models))
	}
	if m.ProviderName != "local" {
		t.Errorf("provider = %q, want local", m.ProviderName)
	}
	if m.Cmd == "" || m.Server != "box1" {
		t.Errorf("lifecycle fields lost in the fold: cmd=%q server=%q", m.Cmd, m.Server)
	}
	if _, unprefixed := c.Models["Qwen3.8-27B"]; unprefixed {
		t.Error("the bare name was also registered as a model; there must be exactly one identity")
	}
}

// TestBareNameStillResolves is the whole reason the rename is survivable: every
// caller that asks for the old name keeps working.
func TestBareNameStillResolves(t *testing.T) {
	c := localCfg(t)
	cands, ok := c.ResolveServed("Qwen3.8-27B")
	if !ok {
		t.Fatal("the bare name resolved to nothing — every existing caller just broke")
	}
	if cands[0].Name != "local-Qwen3.8-27B" {
		t.Errorf("bare name resolved to %q, want the CANONICAL prefixed name so metrics and residency stay on one id", cands[0].Name)
	}
}

// TestBareNameNeverShadowsSomethingExplicit. A fallback that outranks a written
// name is not a fallback.
func TestBareNameNeverShadowsSomethingExplicit(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
servers:
  box1:
    pools: {gpu0: 24GB}
models:
  Qwen3.8-27B:
    type: chat
    quality: 9
    proxy: {host: 127.0.0.1, port: 9999}
providers:
  local:
    models:
      Qwen3.8-27B:
        cmd: llama-server --port 5801
        server: box1
        type: chat
        quality: 5
        proxy: {host: 127.0.0.1, port: 5801}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	cands, ok := c.ResolveServed("Qwen3.8-27B")
	if !ok {
		t.Fatal("did not resolve")
	}
	// The hand-written top-level model wins: it is an exact name.
	if cands[0].Model.Quality != 9 {
		t.Errorf("bare precedence shadowed an explicitly declared model: quality %v", cands[0].Model.Quality)
	}
}

// TestHighestPrecedenceWinsABareName: two providers offering the same id, and
// the stronger claim takes it.
func TestHighestPrecedenceWinsABareName(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
servers:
  box1:
    pools: {gpu0: 24GB}
providers:
  local:
    models:
      shared-model:
        cmd: llama-server --port 5801
        server: box1
        type: chat
        proxy: {host: 127.0.0.1, port: 5801}
  spare:
    barePrecedence: 200
    models:
      shared-model:
        cmd: llama-server --port 5802
        server: box1
        type: chat
        proxy: {host: 127.0.0.1, port: 5802}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	got := c.BareClaims("shared-model")
	if len(got) != 2 {
		t.Fatalf("claims = %v, want both providers", got)
	}
	if got[0] != "spare-shared-model" {
		t.Errorf("strongest claim = %q, want the higher precedence (200) to win", got[0])
	}
	cands, _ := c.ResolveServed("shared-model")
	if cands[0].Name != "spare-shared-model" {
		t.Errorf("resolved to %q, want the strongest claim", cands[0].Name)
	}
}

// TestBarePrecedenceZeroOptsOut: written as 0, only the prefixed name resolves.
// Distinguishable from "unset", which defaults ON — that is why the field is a
// pointer.
func TestBarePrecedenceZeroOptsOut(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
servers:
  box1:
    pools: {gpu0: 24GB}
providers:
  local:
    barePrecedence: 0
    models:
      only-prefixed:
        cmd: llama-server --port 5801
        server: box1
        type: chat
        proxy: {host: 127.0.0.1, port: 5801}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ResolveServed("only-prefixed"); ok {
		t.Error("barePrecedence: 0 still answered a bare name")
	}
	if _, ok := c.ResolveServed("local-only-prefixed"); !ok {
		t.Error("the prefixed name must still resolve")
	}
}

// TestLocalProviderModelInALane: lanes reference the prefixed name, and that is
// what the ladder reports.
func TestLocalProviderModelInALane(t *testing.T) {
	c := localCfg(t)
	cands, ok := c.ResolveServed("chat")
	if !ok || len(cands) == 0 {
		t.Fatal("lane did not resolve to its local member")
	}
	if cands[0].Name != "local-Qwen3.8-27B" {
		t.Errorf("lane member = %q", cands[0].Name)
	}
}

// TestProviderWithNoModelsIsRejected: a provider that declares nothing is a
// half-written block, the same class of typo the remote side already catches.
func TestProviderWithNoModelsIsRejected(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("providers:\n  local: {}\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err == nil {
		t.Error("a provider declaring no models was accepted")
	}
}

// TestLocalModelNeedsAProcessOrAnEndpoint.
func TestLocalModelNeedsAProcessOrAnEndpoint(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
providers:
  local:
    models:
      nothing:
        type: chat
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err == nil {
		t.Error("a model with neither cmd nor proxy was accepted; it can reach nothing")
	}
}

func keys(m map[string]Model) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
