package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// A provider whose filter is deliberately narrow, so the model these tests
// choose is one discovery would never have admitted — the whole reason
// selections exist.
const selectionCfg = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, basePath: /api}
        provides: {anchor: {type: chat, upstream: v/a}}
        discover:
          filter: {free: true, minContext: 100000}
          template: {type: chat, quality: 3}
          limit: 2
        credentials:
          - name: freekey
          - name: paidkey
      manualonly:
        proxy: {host: api.example.com, port: 443}
        manual: true
lanes:
  chat:
    members: [openrouter-anchor]
`

func selCfg(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(selectionCfg), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

func credNames(cands []Candidate) []string {
	var out []string
	for _, c := range cands {
		if c.Credential != nil {
			out = append(out, c.Credential.Name)
		}
	}
	return out
}

func mustResolve(t *testing.T, c *Config, served string) []Candidate {
	t.Helper()
	cands, ok := c.ResolveServed(served)
	if !ok {
		t.Fatalf("%s did not resolve", served)
	}
	return cands
}

// TestSelectionEnrolsAModelDiscoveryNeverSaw is the point of the feature: a
// model no filter admitted becomes servable because someone chose it.
func TestSelectionEnrolsAModelDiscoveryNeverSaw(t *testing.T) {
	c := selCfg(t)
	if _, ok := c.Discovered()["openrouter-tiny"]; ok {
		t.Fatal("precondition: nothing discovered yet")
	}
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny:free",
	}})
	m, ok := c.Discovered()["openrouter-tiny"]
	if !ok {
		t.Fatal("selection did not enrol the model")
	}
	if m.Upstream != "vendor/tiny:free" {
		t.Errorf("upstream = %q, want the provider's own id — that is what goes on the wire", m.Upstream)
	}
	if m.ProviderName != "openrouter" || m.Extension != "free" {
		t.Errorf("selection lost its provider/extension: %+v", m)
	}
	if m.Type != "chat" || m.Quality != 3 {
		t.Errorf("selection did not take the provider's discovery template: type=%q quality=%v", m.Type, m.Quality)
	}
}

// TestSelectedModelServesWithNoApprovalStep. There is no gate any more: the
// selection IS the decision, and a second yes was asking twice.
func TestSelectedModelServesWithNoApprovalStep(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny:free",
	}})
	got := credNames(mustResolve(t, c, "openrouter-tiny"))
	if len(got) != 1 || got[0] != "paidkey" {
		t.Errorf("credentials = %v, want [paidkey] — selecting it is the whole decision", got)
	}
}

// TestSelectionSurvivesADiscoveryRefresh is the invariant that makes selections
// usable at all. SetDiscoveredFor REPLACES a credential's contribution
// wholesale, so without re-application a chosen model would vanish within one
// refresh interval, looking exactly like the provider having dropped it.
func TestSelectionSurvivesADiscoveryRefresh(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny:free",
	}})
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{
		"openrouter-big": {Type: "chat", Quality: 3, ProviderName: "openrouter", Extension: "free"},
	})
	if _, ok := c.Discovered()["openrouter-tiny"]; !ok {
		t.Error("the refresh erased the chosen model")
	}
	if _, ok := c.Discovered()["openrouter-big"]; !ok {
		t.Error("re-applying selections dropped what the refresh found")
	}
}

// TestSelectionIsScopedToItsCredential: directories differ by key, so a model
// chosen on one account must not be offered on another that never saw it.
func TestSelectionIsScopedToItsCredential(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny:free",
	}})
	if !c.DiscoveredServableBy("openrouter-tiny", "paidkey") {
		t.Error("not servable by the credential it was chosen on")
	}
	if c.DiscoveredServableBy("openrouter-tiny", "freekey") {
		t.Error("offered on a credential that never saw it — that request would 404")
	}
}

// TestUnselectingRemovesIt: presence is the predicate. Installing a set that no
// longer contains a model must take it out of service, which is what makes
// DELETE the only verb needed.
func TestUnselectingRemovesIt(t *testing.T) {
	c := selCfg(t)
	sel := Selection{Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny:free"}
	c.SetSelections([]Selection{sel})
	if _, ok := c.Discovered()["openrouter-tiny"]; !ok {
		t.Fatal("precondition: should be enrolled")
	}
	c.SetSelections(nil)
	if _, ok := c.Discovered()["openrouter-tiny"]; ok {
		t.Error("unselecting left the model in service")
	}
}

// TestSelectionLanePlacement is the "and/or in a lane at a priority" half:
// choosing a model and saying where it goes is one action.
func TestSelectionLanePlacement(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "vendor/tiny",
		Lanes: []LanePlacement{{Lane: "chat", Order: 10}},
	}})
	var names []string
	for _, cd := range mustResolve(t, c, "chat") {
		names = append(names, cd.Name)
	}
	if len(names) == 0 || names[0] != "openrouter-anchor" {
		t.Errorf("declared member lost the front of the ladder: %v", names)
	}
	found := false
	for _, n := range names {
		if n == "openrouter-tiny" {
			found = true
		}
	}
	if !found {
		t.Errorf("selected model did not join its lane: %v", names)
	}
}

// TestPlacementOnlySelection: a model a FILTER contributes can still be placed
// in a lane, without the selection claiming to have created it.
func TestPlacementOnlySelection(t *testing.T) {
	c := selCfg(t)
	c.SetDiscoveredFor("openrouter", "freekey", map[string]Model{
		"openrouter-found": {Type: "chat", Quality: 3, ProviderName: "openrouter", Extension: "free"},
	})
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "freekey", Model: "openrouter-found",
		Lanes: []LanePlacement{{Lane: "chat", Order: 5}}, // no Upstream
	}})
	var found bool
	for _, cd := range mustResolve(t, c, "chat") {
		if cd.Name == "openrouter-found" {
			found = true
		}
	}
	if !found {
		t.Error("placement-only selection did not place the discovered model")
	}
}

// TestSelectionQualityOverridesTheTemplate.
func TestSelectionQualityOverridesTheTemplate(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny", Upstream: "x", Quality: 4.5,
	}})
	if got := c.Discovered()["openrouter-tiny"].Quality; got != 4.5 {
		t.Errorf("quality = %v, want the operator's 4.5", got)
	}
}

// TestSelectionOnAProviderWithNoFilter: a manual-only provider has no discovery
// template to copy, so the selection has to stand up a usable model by itself.
func TestSelectionOnAProviderWithNoFilter(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "manualonly", Credential: DefaultCredentialName,
		Model: "manualonly-thing", Upstream: "thing-v1",
	}})
	m, ok := c.Discovered()["manualonly-thing"]
	if !ok {
		t.Fatal("selection on a manual provider enrolled nothing")
	}
	if m.Type != "chat" {
		t.Errorf("type = %q, want chat", m.Type)
	}
	if m.Quality != defaultSelectionQuality {
		t.Errorf("quality = %v, want the %v default — 0 would sort it below everything", m.Quality, defaultSelectionQuality)
	}
}

// TestDeclaredModelsAreNeverOverridden: choosing something the operator already
// wrote down must not redefine it.
func TestDeclaredModelsAreNeverOverridden(t *testing.T) {
	c := selCfg(t)
	c.SetSelections([]Selection{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-anchor", Upstream: "hijacked",
	}})
	if _, ok := c.Discovered()["openrouter-anchor"]; ok {
		t.Error("a selection redefined a declared model")
	}
	if got := credNames(mustResolve(t, c, "openrouter-anchor")); len(got) != 2 {
		t.Errorf("declared model no longer serves on both credentials: %v", got)
	}
}

// TestManualProviderIsValid: `manual: true` is the third way to contribute
// models, and a provider carrying it must load.
func TestManualProviderIsValid(t *testing.T) {
	if err := selCfg(t).Validate(); err != nil {
		t.Fatalf("manual provider rejected: %v", err)
	}
}

// TestProviderContributingNothingIsStillRejected: relaxing the rule for
// `manual` must not turn a half-written provider block into a silent no-op.
func TestProviderContributingNothingIsStillRejected(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
extensions:
  free:
    providers:
      empty:
        proxy: {host: api.example.com, port: 443}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err == nil {
		t.Error("a provider with no provides, no discover and no manual was accepted")
	}
}

// TestLegacyApprovalRequiredIsIgnored: an old config still carrying the field
// must load, and must not gate anything — the mechanism is gone.
func TestLegacyApprovalRequiredIsIgnored(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(`
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides: {anchor: {type: chat, upstream: v/a}}
        credentials:
          - name: paidkey
            approvalRequired: true
`), &c); err != nil {
		t.Fatalf("a config carrying the retired field failed to load: %v", err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	if got := credNames(mustResolve(t, &c, "openrouter-anchor")); len(got) != 1 {
		t.Errorf("credentials = %v, want the account to serve normally", got)
	}
}
