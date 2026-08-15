package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// A provider whose filter is deliberately narrow, so the model the tests pick
// is one discovery would never have admitted — the whole reason picks exist.
const pickCfg = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, basePath: /api}
        discover:
          filter: {free: true, minContext: 100000}
          template: {type: chat, quality: 3}
          limit: 2
        credentials:
          - name: paidkey
            approvalRequired: true
      manualonly:
        proxy: {host: api.example.com, port: 443}
        manual: true
`

func pickConfig(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(pickCfg), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	return &c
}

// TestPickEnrolsAModelDiscoveryNeverSaw is the point of the feature: a model no
// filter admitted becomes servable because someone chose it.
func TestPickEnrolsAModelDiscoveryNeverSaw(t *testing.T) {
	c := pickConfig(t)
	if _, ok := c.Discovered()["openrouter-tiny-model"]; ok {
		t.Fatal("precondition: nothing discovered yet")
	}
	c.SetPicks([]Pick{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny-model", Upstream: "vendor/tiny-model:free",
	}})
	m, ok := c.Discovered()["openrouter-tiny-model"]
	if !ok {
		t.Fatal("pick did not enrol the model")
	}
	if m.Upstream != "vendor/tiny-model:free" {
		t.Errorf("upstream = %q, want the provider's own id — that is what goes on the wire", m.Upstream)
	}
	if m.ProviderName != "openrouter" || m.Extension != "free" {
		t.Errorf("pick lost its provider/extension: %+v", m)
	}
	if m.Type != "chat" || m.Quality != 3 {
		t.Errorf("pick did not take the provider's discovery template: type=%q quality=%v", m.Type, m.Quality)
	}
}

// TestPickSurvivesADiscoveryRefresh is the invariant that makes picks usable at
// all. SetDiscoveredFor REPLACES a credential's contribution wholesale, so
// without re-application a hand-picked model would vanish within one refresh
// interval and look exactly like the provider having dropped it.
func TestPickSurvivesADiscoveryRefresh(t *testing.T) {
	c := pickConfig(t)
	c.SetPicks([]Pick{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny-model", Upstream: "vendor/tiny-model:free",
	}})
	// A refresh lands, contributing an entirely different set.
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{
		"openrouter-big": {Type: "chat", Quality: 3, ProviderName: "openrouter", Extension: "free"},
	})
	if _, ok := c.Discovered()["openrouter-tiny-model"]; !ok {
		t.Error("the refresh erased the hand-picked model")
	}
	if _, ok := c.Discovered()["openrouter-big"]; !ok {
		t.Error("re-applying picks dropped what the refresh found")
	}
}

// TestPickIsScopedToItsCredential: catalogues differ by key, so a model picked
// on one account must not be offered on another that never saw it.
func TestPickIsScopedToItsCredential(t *testing.T) {
	c := pickConfig(t)
	c.SetPicks([]Pick{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny-model", Upstream: "vendor/tiny-model:free",
	}})
	if !c.DiscoveredServableBy("openrouter-tiny-model", "paidkey") {
		t.Error("not servable by the credential it was picked on")
	}
	if c.DiscoveredServableBy("openrouter-tiny-model", "someotherkey") {
		t.Error("offered on a credential that never saw it — that request would 404")
	}
}

// TestPickedModelStillNeedsItsApproval: enrolment and the gate are separate
// mechanisms, and the caller wires them together. A pick with no approval on a
// credential that requires one must not serve — otherwise picking would be a
// way around the gate rather than a use of it.
func TestPickedModelStillNeedsItsApproval(t *testing.T) {
	c := pickConfig(t)
	c.SetPicks([]Pick{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny-model", Upstream: "vendor/tiny-model:free",
	}})
	if cands, ok := c.ResolveServed("openrouter-tiny-model"); ok && len(credNames(cands)) > 0 {
		t.Errorf("served with no approval on an approvalRequired credential: %v", credNames(cands))
	}
	c.SetApprovals(map[string]ApprovalView{
		ApprovalKey("openrouter", "paidkey", "openrouter-tiny-model"): {State: ApprovalApproved},
	})
	if got := credNames(mustResolve(t, c, "openrouter-tiny-model")); len(got) != 1 || got[0] != "paidkey" {
		t.Errorf("credentials = %v, want [paidkey] once approved", got)
	}
}

// TestPickQualityOverridesTheTemplate.
func TestPickQualityOverridesTheTemplate(t *testing.T) {
	c := pickConfig(t)
	c.SetPicks([]Pick{{
		Provider: "openrouter", Credential: "paidkey",
		Model: "openrouter-tiny-model", Upstream: "x", Quality: 4.5,
	}})
	if got := c.Discovered()["openrouter-tiny-model"].Quality; got != 4.5 {
		t.Errorf("quality = %v, want the operator's 4.5", got)
	}
}

// TestPickOnAProviderWithNoFilter: a manual-only provider has no discovery
// template to copy, so the pick has to stand up a usable model by itself.
func TestPickOnAProviderWithNoFilter(t *testing.T) {
	c := pickConfig(t)
	c.SetPicks([]Pick{{
		Provider: "manualonly", Credential: DefaultCredentialName,
		Model: "manualonly-thing", Upstream: "thing-v1",
	}})
	m, ok := c.Discovered()["manualonly-thing"]
	if !ok {
		t.Fatal("pick on a manual provider enrolled nothing")
	}
	if m.Type != "chat" {
		t.Errorf("type = %q, want chat", m.Type)
	}
	if m.Quality != defaultPickQuality {
		t.Errorf("quality = %v, want the %v default — 0 would sort it below everything", m.Quality, defaultPickQuality)
	}
}

// TestManualProviderIsValid: `manual: true` is the third way to contribute
// models, and a provider carrying it must load.
func TestManualProviderIsValid(t *testing.T) {
	if err := pickConfig(t).Validate(); err != nil {
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
