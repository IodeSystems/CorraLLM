package config

import (
	"fmt"
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

// TestSavedConfigDoesNotDuplicateProviderModels is the trap extensions already
// sprang once: the fold puts a provider's models into c.Models under prefixed
// names, and a writer that marshals c.Models emits BOTH the provider block and
// its expansion — so the next Load dies on "collides with a model declared
// elsewhere". A save must round-trip.
func TestSavedConfigDoesNotDuplicateProviderModels(t *testing.T) {
	c := localCfg(t)
	out := forWriting(c)
	if _, dup := out.Models["local-Qwen3.8-27B"]; dup {
		t.Error("a provider's model was written back into top-level models:; the next load would refuse it")
	}
	if len(out.Providers) != 1 || len(out.Providers["local"].Models) != 2 {
		t.Errorf("the provider block itself must survive the write: %+v", out.Providers)
	}
}

// TestRemoteProviderCanClaimBareNames: a remote provider is OFF by default —
// having somebody else's endpoint silently answer a bare name would route a
// request off the box on a coincidence — but it can opt in, and then it competes
// with everyone else on precedence.
func TestRemoteProviderCanClaimBareNames(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
        provides:
          llama-70b: {upstream: llama-3.3-70b, type: chat}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ResolveServed("llama-70b"); ok {
		t.Error("a remote provider claimed a bare name without opting in")
	}
	if _, ok := c.ResolveServed("groq-llama-70b"); !ok {
		t.Error("the prefixed name must resolve regardless")
	}

	// Opted in.
	var opted Config
	if err := yaml.Unmarshal([]byte(`
extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
        barePrecedence: 50
        provides:
          llama-70b: {upstream: llama-3.3-70b, type: chat}
`), &opted); err != nil {
		t.Fatal(err)
	}
	if err := opted.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	cands, ok := opted.ResolveServed("llama-70b")
	if !ok {
		t.Fatal("opted-in remote provider did not answer its bare name")
	}
	if cands[0].Name != "groq-llama-70b" {
		t.Errorf("resolved to %q, want the canonical prefixed name", cands[0].Name)
	}
}

// TestLocalOutranksRemoteOnABareName, and a remote can be given the win
// explicitly. This is what "highest precedence" is FOR: the same id offered in
// two places, and a written-down rule for who gets the unprefixed spelling.
func TestLocalOutranksRemoteOnABareName(t *testing.T) {
	base := `
servers:
  box1:
    pools: {gpu0: 24GB}
providers:
  local:
    models:
      shared: {cmd: llama-server --port 5801, server: box1, type: chat, proxy: {host: 127.0.0.1, port: 5801}}
extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
        barePrecedence: %d
        provides:
          shared: {upstream: shared-remote, type: chat}
`
	for _, tc := range []struct {
		prec int
		want string
	}{
		{50, "local-shared"}, // below the local default of 100
		{500, "groq-shared"}, // above it, deliberately
	} {
		var c Config
		if err := yaml.Unmarshal([]byte(fmt.Sprintf(base, tc.prec)), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.resolveExtensions(); err != nil {
			t.Fatal(err)
		}
		cands, ok := c.ResolveServed("shared")
		if !ok {
			t.Fatalf("prec %d: bare name did not resolve", tc.prec)
		}
		if cands[0].Name != tc.want {
			t.Errorf("prec %d: resolved to %q, want %q", tc.prec, cands[0].Name, tc.want)
		}
	}
}
