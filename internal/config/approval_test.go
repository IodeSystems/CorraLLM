package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

const approvalCfg = `
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443}
        provides: {anchor: {type: chat, upstream: v/a}}
        credentials:
          - name: freekey
          - name: paidkey
            approvalRequired: true
lanes:
  chat:
    members: [openrouter-anchor]
`

func apprCfg(t *testing.T) *Config {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(approvalCfg), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveExtensions(); err != nil {
		t.Fatal(err)
	}
	c.SetDiscoveredFor("openrouter", "freekey", map[string]Model{
		"openrouter-d": {Type: "chat", Quality: 3, ProviderName: "openrouter", Extension: "free"}})
	c.SetDiscoveredFor("openrouter", "paidkey", map[string]Model{
		"openrouter-d": {Type: "chat", Quality: 3, ProviderName: "openrouter", Extension: "free"}})
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

// TestApprovalGatesOnlyTheRequiringCredential: turning the gate on for a paid
// key must not hide a working free roster.
func TestApprovalGatesOnlyTheRequiringCredential(t *testing.T) {
	c := apprCfg(t)
	cands, ok := c.ResolveServed("openrouter-d")
	if !ok {
		t.Fatal("did not resolve")
	}
	got := credNames(cands)
	if len(got) != 1 || got[0] != "freekey" {
		t.Errorf("credentials = %v, want [freekey] — paidkey requires approval and has none", got)
	}
}

// TestApprovalAdmitsOnceApproved.
func TestApprovalAdmitsOnceApproved(t *testing.T) {
	c := apprCfg(t)
	c.SetApprovals(map[string]ApprovalView{
		ApprovalKey("openrouter", "paidkey", "openrouter-d"): {State: ApprovalApproved},
	})
	got := credNames(mustResolve(t, c, "openrouter-d"))
	if len(got) != 2 {
		t.Errorf("credentials = %v, want both once approved", got)
	}
}

// TestRejectionKeepsItOut: an explicit no is not the same as no decision, but
// both keep it off the requiring credential.
func TestRejectionKeepsItOut(t *testing.T) {
	c := apprCfg(t)
	c.SetApprovals(map[string]ApprovalView{
		ApprovalKey("openrouter", "paidkey", "openrouter-d"): {State: ApprovalRejected},
	})
	for _, n := range credNames(mustResolve(t, c, "openrouter-d")) {
		if n == "paidkey" {
			t.Error("a rejected model served on the credential that rejected it")
		}
	}
}

// TestDeclaredModelsAreNeverGated: asking an operator to approve what they just
// wrote down would be theatre.
func TestDeclaredModelsAreNeverGated(t *testing.T) {
	c := apprCfg(t)
	got := credNames(mustResolve(t, c, "openrouter-anchor"))
	if len(got) != 2 {
		t.Errorf("declared model gated by approval: credentials = %v", got)
	}
}

// TestApprovedLanePlacement is the per-model half the goal asked for: choosing
// WHICH lanes a model joins and where in the ladder, which a blanket selector
// cannot express.
func TestApprovedLanePlacement(t *testing.T) {
	c := apprCfg(t)
	c.SetApprovals(map[string]ApprovalView{
		ApprovalKey("openrouter", "paidkey", "openrouter-d"): {
			State: ApprovalApproved,
			Lanes: []LanePlacement{{Lane: "chat", Order: 10}},
		},
	})
	cands, ok := c.ResolveServed("chat")
	if !ok {
		t.Fatal("lane did not resolve")
	}
	var names []string
	for _, cd := range cands {
		names = append(names, cd.Name)
	}
	// Declared member keeps the front of the ladder; the approved one follows.
	if names[0] != "openrouter-anchor" {
		t.Errorf("declared member lost its position: %v", names)
	}
	found := false
	for _, n := range names {
		if n == "openrouter-d" {
			found = true
		}
	}
	if !found {
		t.Errorf("approved model did not join its lane: %v", names)
	}
}

// TestApprovalQualityOverridesTemplateGuess: p16 flagged the uniform template
// quality as "an ASSUMPTION applied to every discovered model, and a wrong one".
func TestApprovalQualityOverridesTemplateGuess(t *testing.T) {
	c := apprCfg(t)
	c.SetApprovals(map[string]ApprovalView{
		ApprovalKey("openrouter", "paidkey", "openrouter-d"): {
			State: ApprovalApproved, Quality: 4.5,
			Lanes: []LanePlacement{{Lane: "chat", Order: 1}},
		},
	})
	for _, cd := range mustResolve(t, c, "chat") {
		if cd.Name == "openrouter-d" && cd.Model.Quality != 4.5 {
			t.Errorf("quality = %v, want the operator's 4.5 over the template's 3", cd.Model.Quality)
		}
	}
}

func mustResolve(t *testing.T, c *Config, served string) []Candidate {
	t.Helper()
	cands, ok := c.ResolveServed(served)
	if !ok {
		t.Fatalf("%s did not resolve", served)
	}
	return cands
}
