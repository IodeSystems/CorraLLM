package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// testHandlers wires a probe directory, because capability coverage is now DATA:
// the plan only offers a capability probe when one exists for the model's
// serving surface. Without a probe dir there is nothing to offer and every
// suggestion is correctly false.
func testHandlers(t *testing.T, models map[string]config.Model, probes ...string) *Handlers {
	t.Helper()
	dir := t.TempDir()
	for _, p := range probes {
		sub := filepath.Join(dir, p)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		// A chat-surface capability probe requiring the image modality — the
		// shape of probes/capability-vision.
		body := "---\nname: " + p + "\nclass: capability\nrequires: { modality: image }\n---\n\n## Prompt\n\nhi\n\n## Checks\n\n- response_contains: red\n"
		if err := os.WriteFile(filepath.Join(sub, "probe.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Handlers{
		Cfg:         &config.Config{Models: models, Servers: map[string]config.Server{"box": {}}},
		Verified:    NewVerifiedStore(),
		BenchProbes: dir,
	}
}

// A model nobody has benched must be flagged New, and the probes that produce
// the MISSING data must default ON — that is what makes a new model one click.
func TestBenchPlan_NewModelDefaultsOn(t *testing.T) {
	h := testHandlers(t, map[string]config.Model{
		"fresh": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	}, "capability-vision")
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
	h := testHandlers(t, map[string]config.Model{
		"covered": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	}, "capability-vision")
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
	h := testHandlers(t, map[string]config.Model{
		"flaky": {Server: "box", Cmd: "run me", Type: "chat", Modalities: map[string]config.ModalitySpec{
			"text": {}, "image": {},
		}},
	}, "capability-vision")
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
	h := testHandlers(t, map[string]config.Model{
		"remote": {Type: "chat"}, // no Server, no Cmd
	})
	p := planFor(t, h, "remote")
	for _, pr := range p.Probes {
		if pr.Kind == "measure" {
			t.Error("a pure-proxy model must not be offered a VRAM measurement")
		}
	}
}

// The bug this closes: a UI run defaulted capability ON for stt/tts/stt-diarize/
// realtime-stt, so thirteen CHAT probes ran against audio endpoints, scored
// 1/21 apiece, and published results that meant nothing — while the models
// still read "audio unverified" afterwards.
//
// Modality alone cannot prevent that: an STT backend declares text too. The
// plan now asks whether a probe exists for the model's SERVING SURFACE.
func TestBenchPlan_NoCapabilityProbeForAudioWhenOnlyChatProbesExist(t *testing.T) {
	h := testHandlers(t, map[string]config.Model{
		"stt": {Server: "box", Cmd: "run me", Type: "stt", Modalities: map[string]config.ModalitySpec{
			"audio": {},
		}},
	}, "capability-vision") // a CHAT probe; nothing covers audio.stt
	p := planFor(t, h, "stt")
	byKind := map[string]BenchProbeSuggestion{}
	for _, pr := range p.Probes {
		byKind[pr.Kind] = pr
	}
	if byKind["capability"].Default {
		t.Error("must not offer a capability run when no probe can exercise this surface")
	}
	if !strings.Contains(byKind["capability"].Reason, "no capability probe exists") {
		t.Errorf("reason should say why: %q", byKind["capability"].Reason)
	}
}
