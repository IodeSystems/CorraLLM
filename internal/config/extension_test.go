package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const extBase = `
servers:
  box1:
    pools: { system: 125GB }
extensions:
  oidio:
    cmd: "exec oidio --addr :5806"
    server: box1
    proxy: 5806
    ramUsage: { system: 3GB }
    persistent: true
    provides:
      stt:         { type: stt }
      stt-diarize: { type: stt }
      tts:         { type: tts }
`

// The whole point of an extension: several served names, ONE process. If these
// ever key apart, killing the process leaves the others proxying at a dead port
// with no way to respawn it — the 502-forever bug extensions exist to remove.
func TestExtensionModelsShareOneProcKey(t *testing.T) {
	c, err := loadYAML(t, extBase)
	if err != nil {
		t.Fatal(err)
	}
	want := "extension:oidio"
	for _, n := range []string{"oidio-stt", "oidio-stt-diarize", "oidio-tts"} {
		if got := c.Models[n].ProcKey(n); got != want {
			t.Errorf("%s: procKey = %q, want %q", n, got, want)
		}
	}
	// A plain model still keys by its own name.
	c2, err := loadYAML(t, extBase+"models:\n  solo: { cmd: \"x\", server: box1, proxy: 5900, type: chat }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Models["solo"].ProcKey("solo"); got != "solo" {
		t.Errorf("solo: procKey = %q, want %q", got, "solo")
	}
}

// A provided model has NO command. The extension owns the process; the model is
// a proxy onto its port. If cmd ever leaks onto the model, every provided model
// looks independently spawnable again — the exact confusion extensions remove.
func TestProvidedModelHasNoCommandOfItsOwn(t *testing.T) {
	c, err := loadYAML(t, extBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"oidio-stt", "oidio-stt-diarize", "oidio-tts"} {
		m := c.Models[n]
		if m.Cmd != "" || m.Server != "" || m.Persistent || len(m.RAMUsage) > 0 || m.Swap != nil {
			t.Errorf("%s: lifecycle leaked onto the model: cmd=%q server=%q persistent=%v ram=%v",
				n, m.Cmd, m.Server, m.Persistent, m.RAMUsage)
		}
		// It IS a proxy: the extension's port must reach it, or nothing routes.
		tgt, err := m.ProxyTarget()
		if err != nil {
			t.Errorf("%s: no proxy target: %v", n, err)
			continue
		}
		if tgt.URL.Port() != "5806" {
			t.Errorf("%s: port = %q, want 5806", n, tgt.URL.Port())
		}
	}
}

// The process layer, and only the process layer, sees the merged view.
func TestEffectiveOverlaysTheExtensionLifecycle(t *testing.T) {
	c, err := loadYAML(t, extBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"oidio-stt", "oidio-tts"} {
		m, ok := c.Effective(n)
		if !ok {
			t.Fatalf("%s: not found", n)
		}
		if m.Cmd == "" || m.Server != "box1" || !m.Persistent || m.RAMUsage["system"] != "3GB" {
			t.Errorf("%s: Effective did not overlay: cmd=%q server=%q persistent=%v ram=%v",
				n, m.Cmd, m.Server, m.Persistent, m.RAMUsage)
		}
	}
	// A plain model is unchanged by the overlay.
	c2, _ := loadYAML(t, extBase+"models:\n  solo: { cmd: \"x\", server: box1, proxy: 5900, type: chat }\n")
	if m, _ := c2.Effective("solo"); m.Cmd != "x" {
		t.Errorf("solo: Effective changed a non-extension model: cmd=%q", m.Cmd)
	}
}

func TestExtensionRejectsBadConfigs(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"unknown extension",
			"models:\n  a: { extension: nope, type: stt, proxy: 1 }\n", "unknown extension"},
		{"provided model sets a lifecycle field",
			"servers:\n  box1: { pools: { system: 1GB } }\nextensions:\n  e:\n    cmd: x\n    server: box1\n    proxy: 1\n    provides:\n      m: { type: stt, cmd: \"y\" }\n", "belong to the extension"},
		{"remote extension with residency knobs",
			"servers:\n  box1: { pools: { system: 1GB } }\nextensions:\n  e: { proxy: 1, server: box1, provides: { m: { type: chat } } }\n", "need a cmd"},
		{"extension without proxy",
			"servers:\n  box1: { pools: { system: 1GB } }\nextensions:\n  e: { cmd: x, server: box1, provides: { m: { type: stt } } }\n", "needs proxy"},
		{"extension declares nothing",
			"servers:\n  box1: { pools: { system: 1GB } }\nextensions:\n  e: { cmd: x, server: box1, proxy: 1 }\n", "declares no capabilities"},
		{"provided name collides with a declared model",
			"servers:\n  box1: { pools: { system: 1GB } }\nextensions:\n  e:\n    cmd: x\n    server: box1\n    proxy: 1\n    provides:\n      m: { type: stt }\nmodels:\n  e-m: { cmd: y, server: box1, proxy: 2 }\n", "collides with a declared model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The extension creates the models; nothing is declared under `models:`. The
// served name is derived, and the upstream id is the key — so the rename to
// oidio-* never has to be restated as a rewrite.
func TestExtensionCreatesItsModels(t *testing.T) {
	c, err := loadYAML(t, extBase)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"oidio-stt":         "stt",
		"oidio-stt-diarize": "stt-diarize",
		"oidio-tts":         "tts",
	}
	for served, upstream := range want {
		m, ok := c.Models[served]
		if !ok {
			t.Errorf("%s: not created by the extension", served)
			continue
		}
		if m.Upstream != upstream {
			t.Errorf("%s: upstream = %q, want %q", served, m.Upstream, upstream)
		}
		if m.Extension != "oidio" {
			t.Errorf("%s: extension = %q, want oidio", served, m.Extension)
		}
	}
	if got := c.ExtensionModels("oidio"); len(got) != 3 {
		t.Errorf("ExtensionModels = %v, want 3", got)
	}
	// The backend's own names must NOT become served names.
	for _, raw := range []string{"stt", "stt-diarize", "tts"} {
		if _, ok := c.Models[raw]; ok {
			t.Errorf("unprefixed %q leaked into the served registry", raw)
		}
	}
}

// A remote integration — Anthropic, Groq — is a shared endpoint and credentials
// with no local process. It must be expressible as an extension, and its models
// must NOT share a ProcKey: there is nothing to share, and pooling them would
// merge admission slots that belong to separate upstream models.
func TestRemoteExtensionHasNoLifecycle(t *testing.T) {
	const y = `
extensions:
  claude:
    proxy: { host: api.anthropic.com, port: 443 }
    provides:
      opus-4:   { type: anthropic-opus, quality: 5, upstream: claude-opus-4 }
      sonnet-4: { type: anthropic-sonnet, quality: 4, upstream: claude-sonnet-4 }
`
	c, err := loadYAML(t, y)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"claude-opus-4", "claude-sonnet-4"} {
		m, ok := c.Models[n]
		if !ok {
			t.Fatalf("%s: not created", n)
		}
		if m.Cmd != "" || m.Server != "" {
			t.Errorf("%s: remote model got a lifecycle: cmd=%q server=%q", n, m.Cmd, m.Server)
		}
		if got := m.ProcKey(n); got != n {
			t.Errorf("%s: ProcKey = %q, want its own name (nothing to share)", n, got)
		}
		if eff, _ := c.Effective(n); eff.Cmd != "" {
			t.Errorf("%s: Effective invented a cmd for a remote extension", n)
		}
		tgt, err := m.ProxyTarget()
		if err != nil || tgt.URL.Hostname() != "api.anthropic.com" {
			t.Errorf("%s: proxy target not inherited: %v %v", n, tgt, err)
		}
	}
	// A remote provider's id is arbitrary, so it is never auto-derived.
	if c.Models["claude-opus-4"].Upstream != "claude-opus-4" {
		t.Errorf("upstream = %q", c.Models["claude-opus-4"].Upstream)
	}
}

// A hosted extension still shares one process — the oidio case must not regress.
func TestHostedExtensionStillSharesOneProcess(t *testing.T) {
	c, err := loadYAML(t, extBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"oidio-stt", "oidio-tts"} {
		if got := c.Models[n].ProcKey(n); got != "extension:oidio" {
			t.Errorf("%s: ProcKey = %q, want extension:oidio", n, got)
		}
	}
}

// One extension, several upstreams. "free" is a single integration but Groq and
// Cerebras are different hosts with different keys, so the provider — not the
// extension — is the served-name prefix and the proxy target.
func TestExtensionWithMultipleProviders(t *testing.T) {
	const y = `
extensions:
  free:
    providers:
      groq:
        proxy: { host: api.groq.com, port: 443, basePath: /openai }
        provides:
          llama-70b: { upstream: llama-3.3-70b-versatile, type: chat }
      cerebras:
        proxy: { host: api.cerebras.ai, port: 443 }
        provides:
          gpt-oss-120b: { upstream: gpt-oss-120b, type: chat }
`
	c, err := loadYAML(t, y)
	if err != nil {
		t.Fatal(err)
	}
	for served, want := range map[string]struct{ host, provider, upstream string }{
		"groq-llama-70b":        {"api.groq.com", "groq", "llama-3.3-70b-versatile"},
		"cerebras-gpt-oss-120b": {"api.cerebras.ai", "cerebras", "gpt-oss-120b"},
	} {
		m, ok := c.Models[served]
		if !ok {
			t.Errorf("%s: not created", served)
			continue
		}
		if m.Extension != "free" || m.ProviderName != want.provider {
			t.Errorf("%s: extension/provider = %q/%q, want free/%q", served, m.Extension, m.ProviderName, want.provider)
		}
		if m.Upstream != want.upstream {
			t.Errorf("%s: upstream = %q, want %q", served, m.Upstream, want.upstream)
		}
		tgt, err := m.ProxyTarget()
		if err != nil || tgt.URL.Hostname() != want.host {
			t.Errorf("%s: host = %v (err %v), want %s", served, tgt, err, want.host)
		}
		// Different upstreams must never pool into one process.
		if got := m.ProcKey(served); got != served {
			t.Errorf("%s: ProcKey = %q, want its own name", served, got)
		}
	}
	if _, leaked := c.Models["free-groq-llama-70b"]; leaked {
		t.Error("extension name leaked into the served name")
	}
}

func TestProviderShapeConflicts(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"both shapes",
			"extensions:\n  e:\n    proxy: 1\n    provides: { a: { type: chat } }\n    providers:\n      p:\n        proxy: 2\n        provides: { b: { type: chat } }\n", "not both"},
		{"providers with a cmd",
			"servers:\n  b: { pools: { system: 1GB } }\nextensions:\n  e:\n    cmd: x\n    server: b\n    providers:\n      p:\n        proxy: 2\n        provides: { b: { type: chat } }\n", "serves exactly one"},
		{"provider without proxy",
			"extensions:\n  e:\n    providers:\n      p:\n        provides: { b: { type: chat } }\n", "needs proxy"},
		{"provider contributing nothing",
			"extensions:\n  e:\n    providers:\n      p:\n        proxy: 1\n", "contributes no models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Listing a discovered model is not enough — requests route through Resolve, so
// a model that appears in /v1/models but cannot be resolved is worse than one
// that was never listed.
func TestResolveFindsDiscoveredModels(t *testing.T) {
	c := &Config{Models: map[string]Model{
		"declared": {Type: "chat", Quality: 9},
	}}
	c.SetDiscovered("openrouter", map[string]Model{
		"openrouter-ling-3.0-flash": {Type: "chat", Upstream: "inclusionai/ling-3.0-flash:free", ProviderName: "openrouter"},
	})

	cands, ok := c.ResolveServed("openrouter-ling-3.0-flash")
	if !ok || len(cands) != 1 {
		t.Fatalf("discovered model did not resolve: ok=%v cands=%v", ok, cands)
	}
	if cands[0].Model.Upstream != "inclusionai/ling-3.0-flash:free" {
		t.Errorf("upstream = %q", cands[0].Model.Upstream)
	}
	if _, ok := c.ResolveServed("openrouter-nope"); ok {
		t.Error("resolved a model nobody declared or discovered")
	}
	// A declared model still wins over anything discovery contributes.
	c.SetDiscovered("x", map[string]Model{"declared": {Type: "chat", Quality: 1, ProviderName: "x"}})
	if cs, _ := c.ResolveServed("declared"); cs[0].Model.Quality != 9 {
		t.Error("discovery shadowed a declared model")
	}
}

// A `discover` filter no longer contributes anything, so a provider carrying
// only that reaches nothing — and is rejected rather than silently serving an
// empty catalogue forever.
func TestDiscoverOnlyProviderIsRejected(t *testing.T) {
	_, err := loadYAML(t, `
extensions:
  free:
    providers:
      openrouter:
        proxy: { host: openrouter.ai, port: 443, basePath: /api }
        directory:
          filter: { free: true, inputModality: text, outputModality: text }
`)
	if err == nil {
		t.Fatal("a provider whose only content is a directory filter was accepted; it can contribute no models")
	}
}

// The retired `discover:` spelling still loads and becomes the directory's
// default filter, so an existing config does not fail to parse — it just stops
// enrolling, which is the point.
func TestLegacyDiscoverBecomesDirectoryFilter(t *testing.T) {
	c, err := loadYAML(t, `
extensions:
  free:
    providers:
      openrouter:
        proxy: { host: openrouter.ai, port: 443, basePath: /api }
        manual: true
        discover:
          filter: { free: true, minContext: 8192 }
`)
	if err != nil {
		t.Fatalf("legacy discover key failed to load: %v", err)
	}
	pv := c.Extensions["openrouter"].Providers["openrouter"]
	if pv.Directory == nil {
		pv = c.Extensions["free"].Providers["openrouter"]
	}
	if pv.Directory == nil || !pv.Directory.Filter.Free || pv.Directory.Filter.MinContext != 8192 {
		t.Errorf("legacy discover did not carry over as the directory filter: %+v", pv.Directory)
	}
	if n := len(c.AllModels()); n != 0 {
		t.Errorf("a directory filter enrolled %d models; it must enrol none", n)
	}
}

// A virtual extension exposes one fetch per member provider per credential, and
// serves nothing until the first refresh lands.
func TestVirtualExtensionExposesFetchTargets(t *testing.T) {
	c, err := loadYAML(t, `
extensions:
  free:
    virtual:
      filter: { free: true, inputModality: text, outputModality: text }
      template: { type: chat, quality: 3 }
      limit: 12
      lanes: [{ lane: free, order: 60 }]
    providers:
      openrouter:
        proxy: { host: openrouter.ai, port: 443, basePath: /api }
      groq:
        proxy: { host: api.groq.com, port: 443, basePath: /openai }
`)
	if err != nil {
		t.Fatalf("virtual extension rejected: %v", err)
	}
	if n := len(c.AllModels()); n != 0 {
		t.Errorf("served %d models before any refresh, want 0", n)
	}
	tg := c.VirtualTargets()
	if len(tg) != 2 {
		t.Fatalf("want one fetch per member provider, got %+v", tg)
	}
	hosts := map[string]bool{}
	for _, vt := range tg {
		hosts[vt.Target.URL.Hostname()] = true
		if vt.Virtual != "free" {
			t.Errorf("fetch attributed to %q, want the pool that asked for it", vt.Virtual)
		}
	}
	if !hosts["openrouter.ai"] || !hosts["api.groq.com"] {
		t.Errorf("pool did not span both members: %+v", hosts)
	}
	if tg[0].Spec.Template.Type != "chat" || tg[0].Spec.Limit != 12 {
		t.Errorf("pool spec lost: %+v", tg[0].Spec)
	}
}

// A virtual extension with no filter and no cap would pool every model every
// member offers — hundreds, most of them wrong for it.
func TestUnboundedVirtualIsRejected(t *testing.T) {
	_, err := loadYAML(t, `
extensions:
  free:
    virtual: {}
    providers:
      openrouter:
        proxy: { host: openrouter.ai, port: 443, basePath: /api }
`)
	if err == nil {
		t.Fatal("an unfiltered, uncapped pool was accepted")
	}
}

// Residency is a claim about THIS box. The three cases that must not blur:
// a hosted extension's model has a local process (its extension's), a remote
// provider has none, and a pure proxy pointing at a local port does — the last
// is what stops a sidecar on 127.0.0.1 from being classed as somebody else's
// machine.
func TestLocalProcessAndRemote(t *testing.T) {
	const y = `
servers:
  box1:
    pools: { system: 125GB }
models:
  local-llm:
    cmd: "exec llama-server"
    server: box1
    proxy: 5801
  sidecar:
    proxy: 5806     # no cmd, but the port is ours
extensions:
  oidio:
    cmd: "exec oidio --addr :5806"
    server: box1
    proxy: 5806
    ramUsage: { system: 3GB }
    provides:
      stt: { type: stt }
  free:
    providers:
      groq:
        proxy: { host: api.groq.com, port: 443 }
        provides:
          llama-70b: { type: chat, upstream: llama-3.3-70b-versatile }
`
	c, err := loadYAML(t, y)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		local, remote bool
	}{
		{"local-llm", true, false},
		{"oidio-stt", true, false}, // no cmd of its own; its extension has one
		{"sidecar", false, false},  // no process, but the target is this box
		{"groq-llama-70b", false, true},
	} {
		m, ok := c.Models[tc.name]
		if !ok {
			t.Fatalf("%s: missing", tc.name)
		}
		if got := m.LocalProcess(); got != tc.local {
			t.Errorf("%s: LocalProcess = %v, want %v", tc.name, got, tc.local)
		}
		if got := m.Remote(); got != tc.remote {
			t.Errorf("%s: Remote = %v, want %v", tc.name, got, tc.remote)
		}
	}
}
