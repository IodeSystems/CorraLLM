package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Disabling is only worth anything if it closes EVERY door. A model is
// reachable nine ways — by name, by alias, as a declared lane member, through a
// selector, through a selection, through a virtual pool, as a discovered id, as
// a glob template, and by its bare unprefixed spelling — and one left open is
// not a weaker feature, it is a broken one: the operator is told the backend is
// out of their setup and traffic keeps arriving at it by whichever route was
// missed.
//
// So there is a test per door.

func disabledCfg(t *testing.T, body string) *Config {
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

func names(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Name)
	}
	return out
}

func mustNotResolve(t *testing.T, c *Config, served string) {
	t.Helper()
	if cands, ok := c.ResolveServed(served); ok || len(cands) > 0 {
		t.Fatalf("%s resolved to %v — a disabled model must reach nothing", served, names(cands))
	}
}

// --- by name, and what the caller is told --------------------------------

const byNameCfg = `
models:
  keep:
    type: chat
    proxy: {host: 127.0.0.1, port: 8080}
  gone:
    type: chat
    disabled: true
    aliases: [gone-old]
    proxy: {host: 127.0.0.1, port: 8081}
`

func TestDisabledModelIsUnreachableByName(t *testing.T) {
	c := disabledCfg(t, byNameCfg)
	mustNotResolve(t, c, "gone")
	if _, ok := c.ResolveServed("keep"); !ok {
		t.Fatal("disabling one model must not touch another")
	}
}

func TestDisabledModelIsUnreachableByAlias(t *testing.T) {
	c := disabledCfg(t, byNameCfg)
	mustNotResolve(t, c, "gone-old")
}

// The answer a by-name request gets has to say WHICH — a name that is spelled
// correctly and sitting in the config, reported as "unknown model", sends an
// operator hunting for a typo that is not there.
func TestDisabledIsDistinguishableFromUnknown(t *testing.T) {
	c := disabledCfg(t, byNameCfg)
	if !c.DisabledServed("gone") {
		t.Error("a disabled model must not be reported as merely unknown")
	}
	if !c.DisabledServed("gone-old") {
		t.Error("an alias of a disabled model is disabled too")
	}
	if c.DisabledServed("never-existed") {
		t.Error("a name nobody declared is unknown, not disabled")
	}
	if c.DisabledServed("keep") {
		t.Error("a live model reported as disabled")
	}
}

// A disabled model is not in the served registry, so nothing advertises a name
// that answers 404 on the next call. This is the one place disabled and paused
// deliberately differ: a paused model stays listed, in state "paused".
func TestDisabledModelIsNotInTheServedRegistry(t *testing.T) {
	c := disabledCfg(t, byNameCfg)
	if _, listed := c.AllModels()["gone"]; listed {
		t.Error("a disabled model must not be advertised as served")
	}
	if _, listed := c.AllModels()["keep"]; !listed {
		t.Error("AllModels dropped a live model")
	}
	if _, written := c.Models["gone"]; !written {
		t.Error("Config.Models is the document — the entry must survive so enabling restores it")
	}
}

// --- lane membership ------------------------------------------------------

const laneRungCfg = `
models:
  top:
    type: chat
    quality: 5
    disabled: true
    proxy: {host: 127.0.0.1, port: 8080}
  floor:
    type: chat
    quality: 1
    proxy: {host: 127.0.0.1, port: 8081}
lanes:
  chat:
    members: [top, floor]
`

func TestDisabledRungLeavesTheLadder(t *testing.T) {
	c := disabledCfg(t, laneRungCfg)
	cands, ok := c.ResolveServed("chat")
	if !ok {
		t.Fatal("lane stopped resolving entirely")
	}
	got := names(cands)
	if len(got) != 1 || got[0] != "floor" {
		t.Fatalf("ladder = %v, want [floor] — the lane must fall through, not stall on a rung it will never try", got)
	}
}

// The ladder VIEW keeps a declared rung it will not try, marked. The lane
// panel renders this list; a rung silently vanishing from it would hide the
// operator's own decision from them, and leave nowhere to undo it.
func TestLaneLadderKeepsADisabledDeclaredRungMarked(t *testing.T) {
	c := disabledCfg(t, laneRungCfg)
	ladder := c.LaneLadder("chat")
	if len(ladder) != 2 {
		t.Fatalf("ladder = %+v, want both rungs listed", ladder)
	}
	if ladder[0].Name != "top" || !ladder[0].Disabled {
		t.Errorf("rung 0 = %+v, want top marked disabled", ladder[0])
	}
	if ladder[1].Disabled {
		t.Errorf("rung 1 = %+v, want floor live", ladder[1])
	}
}

// --- selector membership --------------------------------------------------

const selectorCfg = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides:
          big: {type: chat, quality: 5, upstream: v/big}
          small: {type: chat, quality: 1, upstream: v/small, disabled: true}
lanes:
  free:
    members:
      - {provider: openrouter}
`

func TestSelectorDoesNotContributeADisabledModel(t *testing.T) {
	c := disabledCfg(t, selectorCfg)
	cands, ok := c.ResolveServed("free")
	if !ok {
		t.Fatal("lane did not resolve")
	}
	got := names(cands)
	if len(got) != 1 || got[0] != "openrouter-big" {
		t.Fatalf("selector expanded to %v, want [openrouter-big]", got)
	}
	// And the same model is refused when named directly — a selector and a
	// name are two doors to one model, not two models.
	mustNotResolve(t, c, "openrouter-small")
}

// --- selection ------------------------------------------------------------

// Choosing a model off a directory and then disabling it is not a
// contradiction: the selection records the pick, the disable is the later
// decision about whether it serves. The later one wins, and the selection
// survives so enabling restores the lane placement it came with.
func TestSelectionDoesNotReviveADisabledModel(t *testing.T) {
	c := disabledCfg(t, `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides: {anchor: {type: chat, upstream: v/a}}
        credentials:
          - name: paidkey
lanes:
  chat:
    members: [openrouter-anchor]
`)
	// A selection contributes to the overlay; a DECLARED model always wins, so
	// disable the declared one and place it by selection.
	c.Models["openrouter-anchor"] = func() Model {
		m := c.Models["openrouter-anchor"]
		m.Disabled = true
		return m
	}()
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-anchor", Lanes: []LanePlacement{{Lane: "chat", Order: 1}},
	}})
	if got := c.selectedLaneMembers("chat"); len(got) != 0 {
		t.Fatalf("selection put a disabled model back in the lane: %v", names(got))
	}
	if cands, ok := c.ResolveServed("chat"); ok {
		t.Fatalf("lane resolved to %v, want nothing left", names(cands))
	}
	if !c.DisabledServed("chat") {
		t.Error("a lane whose every declared rung is disabled is not an unknown lane")
	}
}

// --- virtual pool contribution --------------------------------------------

// The motivating case. groq-gpt-oss-120b is authored in a provider's
// `provides` inside a virtual extension, and reaches the `free` lane through
// the pool rather than through the lane's member list.
func TestPoolDoesNotContributeADisabledModel(t *testing.T) {
	c := disabledCfg(t, `
extensions:
  free:
    virtual:
      filter: {free: true}
      template: {type: chat, quality: 3}
      lanes: [{lane: free, order: 60}]
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443}
        provides: {gpt-oss-120b: {type: chat, upstream: openai/gpt-oss-120b, disabled: true}}
lanes:
  free:
    members: [local-floor]
models:
  local-floor:
    type: chat
    quality: 1
    proxy: {host: 127.0.0.1, port: 8080}
`)
	// The pool records what a refresh contributed. Feed it the disabled model
	// exactly as a refresh would.
	c.SetVirtualModels("free", "groq", DefaultCredentialName, map[string]Model{
		"groq-gpt-oss-120b": c.Models["groq-gpt-oss-120b"],
	})
	if got := c.virtualLaneMembers("free"); len(got) != 0 {
		t.Fatalf("the pool contributed %v, want nothing", names(got))
	}
	cands, _ := c.ResolveServed("free")
	if got := names(cands); len(got) != 1 || got[0] != "local-floor" {
		t.Fatalf("lane = %v, want [local-floor] only", got)
	}
	mustNotResolve(t, c, "groq-gpt-oss-120b")
}

// --- a discover: filter's whole contribution ------------------------------

// The only way a model with no authored entry can carry the flag: the
// provider's discover template sets it, disabling everything that filter
// admits.
func TestDiscoverTemplateCanDisableItsWholeContribution(t *testing.T) {
	c := disabledCfg(t, `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides: {anchor: {type: chat, upstream: v/a}}
`)
	c.SetDiscovered("openrouter", map[string]Model{
		"openrouter-x": {Type: "chat", Quality: 3, ProviderName: "openrouter", Disabled: true},
		"openrouter-y": {Type: "chat", Quality: 3, ProviderName: "openrouter"},
	})
	mustNotResolve(t, c, "openrouter-x")
	if _, ok := c.ResolveServed("openrouter-y"); !ok {
		t.Error("an undisabled discovered model must still resolve")
	}
	if _, listed := c.AllModels()["openrouter-x"]; listed {
		t.Error("a disabled discovered model must not be advertised")
	}
}

// --- glob template and bare name ------------------------------------------

const templateCfg = `
models:
  "claude-opus-*":
    type: anthropic-opus
    disabled: true
    proxy: {host: api.anthropic.com, port: 443}
providers:
  local:
    models:
      Qwen3.8-27B:
        type: chat
        disabled: true
        cmd: llama-server
        server: box1
servers:
  box1:
    pools: {gpu0: 24GB}
`

// A disabled template takes its whole family out at once. Anything else would
// serve a dated variant from a definition the operator did not disable.
func TestDisabledGlobTemplateServesNoVariant(t *testing.T) {
	c := disabledCfg(t, templateCfg)
	mustNotResolve(t, c, "claude-opus-5-20260401")
}

// The bare spelling is a second name for one model, not a second model — so it
// cannot be the route back in.
func TestDisabledModelIsUnreachableByItsBareName(t *testing.T) {
	c := disabledCfg(t, templateCfg)
	if _, ok := c.Models["local-Qwen3.8-27B"]; !ok {
		t.Fatal("precondition: the provider's model folds in under its prefixed name")
	}
	mustNotResolve(t, c, "Qwen3.8-27B")
	mustNotResolve(t, c, "local-Qwen3.8-27B")
}

// --- the upgrade must be inert --------------------------------------------

// Every config written before this field existed must mean exactly what it
// always did: `disabled` defaults to false and nothing else changes.
func TestExistingConfigIsUnaffected(t *testing.T) {
	c := disabledCfg(t, `
models:
  a:
    type: chat
    proxy: {host: 127.0.0.1, port: 8080}
  b:
    type: chat
    proxy: {host: 127.0.0.1, port: 8081}
lanes:
  chat:
    members: [a, b]
`)
	for name, m := range c.Models {
		if m.Disabled {
			t.Errorf("model %q defaulted to disabled", name)
		}
	}
	cands, ok := c.ResolveServed("chat")
	if !ok || len(names(cands)) != 2 {
		t.Fatalf("lane = %v, want both rungs", names(cands))
	}
	if len(c.AllModels()) != 2 {
		t.Fatalf("registry = %v, want both models", c.AllModels())
	}
	if c.DisabledServed("chat") || c.DisabledServed("a") {
		t.Error("nothing is disabled in a config that never said so")
	}
	// The field is omitempty, so it does not appear in an export either.
	out, err := yaml.Marshal(c.Models["a"])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "disabled") {
		t.Errorf("export gained a `disabled:` line for a model that never set it:\n%s", got)
	}
}

// --- disabling the last live rung -----------------------------------------

// ALLOWED, and reported. Refusing would make a config edit depend on live
// state — which rungs happen to be paused, refusing or unreachable at that
// moment — so the same edit would be accepted at 10:00 and rejected at 10:05.
// It would also forbid the ordinary order of operations: take the dead rung
// out, then put the replacement in. An emptied lane answers the same way a
// lane with every rung paused already does.
func TestDisablingTheLastRungIsAllowedAndVisible(t *testing.T) {
	c := disabledCfg(t, `
models:
  only:
    type: chat
    disabled: true
    proxy: {host: 127.0.0.1, port: 8080}
lanes:
  chat:
    members: [only]
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("disabling a lane's last rung must not fail validation: %v", err)
	}
	if cands, ok := c.ResolveServed("chat"); ok {
		t.Fatalf("lane still resolved to %v", names(cands))
	}
	if !c.DisabledServed("chat") {
		t.Error("an emptied lane must be reportable as disabled, not unknown")
	}
	ladder := c.LaneLadder("chat")
	if len(ladder) != 1 || !ladder[0].Disabled {
		t.Errorf("ladder = %+v, want the rung listed and marked so the cost is countable", ladder)
	}
}
