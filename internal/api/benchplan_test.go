package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func planFor(t *testing.T, h *Handlers, model string) BenchModelPlan {
	t.Helper()
	out, err := h.BenchPlan(context.Background(), &BenchPlanInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Body.Models {
		if p.Model == model {
			return p
		}
	}
	t.Fatalf("model %q not in plan", model)
	return BenchModelPlan{}
}

func testHandlers(models map[string]config.Model) *Handlers {
	return &Handlers{
		Cfg:      &config.Config{Models: models, Servers: map[string]config.Server{"box": {}}},
		Verified: NewVerifiedStore(),
	}
}

// A model nobody has benched must be flagged New, and the probes that produce
// the MISSING data must default ON — that is what makes a new model one click.
func TestBenchPlan_NewModelDefaultsOn(t *testing.T) {
	h := testHandlers(map[string]config.Model{
		"fresh": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	})
	p := planFor(t, h, "fresh")
	if !p.New {
		t.Error("a model with no tune profile and no verdicts is New")
	}
	byKind := map[string]BenchProbeSuggestion{}
	for _, pr := range p.Probes {
		byKind[pr.Kind] = pr
	}
	if !byKind["capability"].Default {
		t.Error("capability must default ON when nothing has been verified")
	}
	// Quality is slow and is not data corrallm consumes; defaulting it ON would
	// turn every new-model prompt into an expensive full benchmark.
	if byKind["quality"].Default {
		t.Error("quality must default OFF")
	}
	// image is declared but unverified; text is the baseline and must not be
	// flagged on every model or the real signal drowns.
	if len(p.UnverifiedModality) != 1 || p.UnverifiedModality[0] != "image" {
		t.Errorf("unverified = %v, want [image] (text excluded)", p.UnverifiedModality)
	}
}

// Once a modality is verified, capability stops defaulting ON — a covered model
// must not be silently re-measured at the cost of evicting the box.
func TestBenchPlan_VerifiedModelDefaultsOff(t *testing.T) {
	h := testHandlers(map[string]config.Model{
		"covered": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	})
	h.Verified.Record("covered", Verdict{Modality: "image", RunMode: "cold", Verified: true})
	p := planFor(t, h, "covered")
	byKind := map[string]BenchProbeSuggestion{}
	for _, pr := range p.Probes {
		byKind[pr.Kind] = pr
	}
	if byKind["capability"].Default {
		t.Error("capability must default OFF once the declared modality is verified")
	}
	if p.New {
		t.Error("a model with capability data is not New")
	}
}

// A cold/warm disagreement must re-arm the capability probe: the model is
// "verified" in one state and broken in another, which is the case most worth
// re-running, not least worth it.
func TestBenchPlan_DisagreementReArmsCapability(t *testing.T) {
	h := testHandlers(map[string]config.Model{
		"flaky": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	})
	h.Verified.Record("flaky", Verdict{Modality: "image", RunMode: "warm", Verified: true})
	h.Verified.Record("flaky", Verdict{Modality: "image", RunMode: "cold", Verified: false})
	p := planFor(t, h, "flaky")
	if len(p.Disagreements) == 0 {
		t.Error("cold/warm split must be surfaced")
	}
	byKind := map[string]BenchProbeSuggestion{}
	for _, pr := range p.Probes {
		byKind[pr.Kind] = pr
	}
	if !byKind["capability"].Default {
		t.Error("a disagreement must re-arm the capability probe")
	}
}

// A pure-proxy model consumes no local pools, so offering to measure its VRAM
// would be noise.
func TestBenchPlan_ProxyModelHasNoMeasureProbe(t *testing.T) {
	h := testHandlers(map[string]config.Model{
		"remote": {Type: "chat"}, // no Server, no Cmd
	})
	p := planFor(t, h, "remote")
	for _, pr := range p.Probes {
		if pr.Kind == "measure" {
			t.Error("a pure-proxy model must not be offered a VRAM measurement")
		}
	}
}
