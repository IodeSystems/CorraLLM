package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/iodesystems/corrallm/internal/bench/task"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/gpu"
)

// Bench planning — "this model has never been measured, do you want to?"
//
// corrallm knows two things about a model it cannot learn on its own: how much
// VRAM it really needs (the tune profile), and whether its DECLARED modalities
// actually work (capability verdicts). Both come from llm-bench.
//
// A model added to the config has NEITHER. Until it has been benched, corrallm
// is scheduling on a declared ramUsage nobody verified and advertising
// modalities nobody exercised — which is how a model that silently drops images
// on its first request stayed "verified end-to-end" in a comment for weeks.
//
// This endpoint answers: which models are unmeasured, and what should be
// checked by default? The rule is deliberately simple — a probe defaults ON when
// the data it produces is MISSING, and OFF when it already exists. That makes
// the common case (a new model appears) a single click, without silently
// re-running expensive work on models that are already covered.

// BenchProbeSuggestion is one probe the UI should offer, pre-checked or not.
type BenchProbeSuggestion struct {
	Kind string `json:"kind" doc:"capability | measure | quality"`
	// Default reports whether the UI should pre-check this probe: true when the
	// data it produces is missing for this model.
	Default bool   `json:"default"`
	Reason  string `json:"reason" doc:"Why it is (or is not) pre-checked."`
}

// BenchModelPlan is a model's measurement coverage and what to run.
type BenchModelPlan struct {
	Model string `json:"model"`
	// New reports that NOTHING is known about this model — no tune profile and
	// no capability verdicts. This is the "click bench" prompt condition.
	New                bool                   `json:"new"`
	HasTuneProfile     bool                   `json:"hasTuneProfile"`
	HasCapabilityData  bool                   `json:"hasCapabilityData"`
	DeclaredModalities []string               `json:"declaredModalities,omitempty"`
	UnverifiedModality []string               `json:"unverifiedModalities,omitempty" doc:"Modalities the config DECLARES but no probe has ever confirmed."`
	Disagreements      []Verdict              `json:"disagreements,omitempty" doc:"Modalities that verified in one residency state and failed in another (cold vs warm)."`
	Probes             []BenchProbeSuggestion `json:"probes"`

	// Profile is the MEASURED VRAM shape, present whether or not the model is
	// currently resident. The console previously read these numbers from the
	// residency op, which lists only resident backends — so evicting a model
	// blanked its Memory table to "—" even though the measurement persists in
	// the tune cache. A measurement outlives residency; the UI should too.
	Profile *TuneProfileView `json:"profile,omitempty"`
}

// TuneProfileView is a model's measured VRAM shape.
type TuneProfileView struct {
	BaseMiB       int    `json:"baseMiB" doc:"Footprint with KV excluded (weights + fixed overhead)."`
	PerSlotMiB    int    `json:"perSlotMiB" doc:"KV cost per slot; 0 when not yet derivable."`
	PeakMiB       int    `json:"peakMiB" doc:"Highest total footprint ever observed."`
	MeasuredSlots int    `json:"measuredSlots"`
	Ctx           int    `json:"ctx" doc:"n_ctx the measurement was taken at."`
	Source        string `json:"source" doc:"bench (deliberate, isolated) | serving (opportunistic, possibly contended)."`
	MeasuredAt    int64  `json:"measuredAt"`
}

// BenchPlanInput has no parameters.
type BenchPlanInput struct{}

// BenchPlanOutput lists every configured model's measurement coverage.
type BenchPlanOutput struct {
	Body struct {
		GPU    string           `json:"gpu,omitempty"`
		Models []BenchModelPlan `json:"models"`
		// NewModels is the count of never-benched models — what the dashboard
		// badges to prompt a run.
		NewModels int `json:"newModels"`
	}
}

// BenchPlan reports which models lack measurement data and what to run for them.
func (h *Handlers) BenchPlan(_ context.Context, _ *BenchPlanInput) (*BenchPlanOutput, error) {
	out := &BenchPlanOutput{}
	if h.Cfg == nil {
		return out, nil
	}
	gpuName := ""
	if st, err := gpu.Probe(); err == nil {
		gpuName = st.Name
	}
	out.Body.GPU = gpuName

	// What can actually be PROBED, read from the probe directory. Without this
	// the plan defaults `capability` ON for any unverified modality, including
	// ones no probe can exercise — which is how the four audio models ended up
	// running 13 chat probes apiece and publishing 1/21 scores that mean
	// nothing. Coverage is data, not a hardcoded list, so adding an audio probe
	// makes audio offerable with no code change here.
	cov := h.probeCoverage()
	costModel := cost.NewModel(h.Cfg)

	for name, m := range h.Cfg.Models {
		plan := BenchModelPlan{Model: name}

		// A pure-proxy model consumes no local pools, so a VRAM profile is
		// meaningless for it — offering to measure one would be noise.
		measurable := m.Server != "" && m.Cmd != ""

		if h.Mgr != nil && measurable && gpuName != "" {
			if p, ok := h.Mgr.TuneProfile(gpuName, name); ok && p.BaseMiB > 0 {
				plan.HasTuneProfile = true
				plan.Profile = &TuneProfileView{
					BaseMiB: p.BaseMiB, PerSlotMiB: p.PerSlotMiB, PeakMiB: p.PeakMiB,
					MeasuredSlots: p.MeasuredSlots, Ctx: p.Ctx,
					Source: p.Source, MeasuredAt: p.MeasuredAt,
				}
			}
		}

		declared := m.EffectiveModalities(costModel.IsAudioType(m.Type))
		for k := range declared {
			plan.DeclaredModalities = append(plan.DeclaredModalities, k)
		}
		sort.Strings(plan.DeclaredModalities)

		verdicts := h.Verified.For(name)
		plan.HasCapabilityData = len(verdicts) > 0
		verifiedMod := map[string]bool{}
		for _, v := range verdicts {
			if v.Verified {
				verifiedMod[v.Modality] = true
			}
		}
		for _, mod := range plan.DeclaredModalities {
			// text is the baseline every chat model has; flagging it as
			// "unverified" on every model would drown the signal that matters.
			if mod == "text" {
				continue
			}
			if !verifiedMod[mod] {
				plan.UnverifiedModality = append(plan.UnverifiedModality, mod)
			}
		}
		plan.Disagreements = h.Verified.Disagreements(name)

		plan.New = !plan.HasTuneProfile && !plan.HasCapabilityData

		// Defaults: ON when the data is missing, OFF when it exists. A new
		// model is therefore one click, and a covered model is not silently
		// re-measured at the cost of evicting the box.
		if measurable {
			plan.Probes = append(plan.Probes, BenchProbeSuggestion{
				Kind: "measure", Default: !plan.HasTuneProfile,
				Reason: reasonFor(plan.HasTuneProfile,
					"no VRAM profile — corrallm is scheduling on the declared ramUsage, unverified",
					"VRAM profile already measured"),
			})
		}
		// Offer a capability probe only when one EXISTS for this model's serving
		// surface and for something it declares. Otherwise the checkbox invites
		// a run that cannot say anything.
		modelCap := config.ModelCapability(m)
		coverable := cov.covers(modelCap, plan.UnverifiedModality) ||
			(len(plan.Disagreements) > 0 && cov.hasCapability(modelCap))
		capReason := "all declared modalities verified"
		switch {
		case !cov.hasCapability(modelCap):
			capReason = fmt.Sprintf("no capability probe exists for a %s model — nothing to run", modelCap)
		case coverable:
			capReason = "declared modalities have never been exercised against the live backend"
		case len(plan.UnverifiedModality) > 0:
			capReason = fmt.Sprintf("unverified %v, but no probe covers %s on a %s model",
				plan.UnverifiedModality, plan.UnverifiedModality, modelCap)
		}
		plan.Probes = append(plan.Probes, BenchProbeSuggestion{
			Kind: "capability", Default: coverable, Reason: capReason,
		})
		// Quality is never pre-checked: it is slow, it is not data corrallm
		// itself consumes, and defaulting it ON would make every new-model
		// prompt an expensive full benchmark.
		plan.Probes = append(plan.Probes, BenchProbeSuggestion{
			Kind: "quality", Default: false,
			Reason: "opt-in: slow, and not data corrallm uses for scheduling",
		})

		if plan.New {
			out.Body.NewModels++
		}
		out.Body.Models = append(out.Body.Models, plan)
	}
	sort.Slice(out.Body.Models, func(i, j int) bool {
		// Never-benched models first: they are the ones needing action.
		if out.Body.Models[i].New != out.Body.Models[j].New {
			return out.Body.Models[i].New
		}
		return out.Body.Models[i].Model < out.Body.Models[j].Model
	})
	return out, nil
}

func reasonFor(have bool, missing, present string) string {
	if have {
		return present
	}
	return missing
}

// probeSet is what the probe directory can actually exercise.
type probeSet struct {
	// capability -> set of modalities that capability's probes require. A probe
	// with no modality requirement contributes the empty string, meaning "this
	// capability is probeable at all".
	byCapability map[string]map[string]bool
}

func (p probeSet) hasCapability(capability string) bool {
	_, ok := p.byCapability[capability]
	return ok
}

// covers reports whether a probe exists for this capability targeting any of
// the given modalities.
func (p probeSet) covers(capability string, modalities []string) bool {
	mods, ok := p.byCapability[capability]
	if !ok {
		return false
	}
	for _, m := range modalities {
		if mods[m] {
			return true
		}
	}
	return false
}

// probeCoverage scans the probe directory. A missing or unreadable directory
// yields empty coverage, which suppresses the capability checkbox everywhere
// rather than offering runs that cannot happen.
func (h *Handlers) probeCoverage() probeSet {
	set := probeSet{byCapability: map[string]map[string]bool{}}
	if h.BenchProbes == "" {
		return set
	}
	ents, err := os.ReadDir(h.BenchProbes)
	if err != nil {
		return set
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		t, err := task.LoadDir(filepath.Join(h.BenchProbes, e.Name()))
		if err != nil || t.Class != "capability" {
			continue
		}
		capability := t.Requires.EffectiveCapability()
		if set.byCapability[capability] == nil {
			set.byCapability[capability] = map[string]bool{}
		}
		set.byCapability[capability][t.Requires.Modality] = true
	}
	return set
}
