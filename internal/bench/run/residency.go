package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/bench/task"
)

// Residency control — the reason this harness lives inside corrallm.
//
// A capability claim can only be falsified COLD. `ternary-bonsai-27b` declares
// modalities.image, /props reports vision: true, the mmproj loads, and it
// describes an attached image correctly — once warm. On the FIRST request after
// a load it silently drops the image and answers from the text alone, saying "no
// actual image attached" in its reasoning. Every warm probe passes. The config
// claimed the modality was "verified end-to-end" precisely because the one
// manual check anyone ran happened to hit a warm model.
//
// No external harness can catch that: it cannot evict, so it cannot choose which
// path it is testing. corrallm owns residency, so llm-bench can.

// RunMode selects the residency state a probe runs against.
type RunMode string

const (
	// ModeAny does not touch residency — the model may be warm, cold, or
	// mid-swap depending on what ran before. This is the DEFAULT because it is
	// the pre-existing behavior, but note that it makes a probe's result depend
	// on execution order, which is how the bonsai bug hid for so long.
	ModeAny RunMode = ""
	// ModeCold evicts the model first, so the probe's first request pays the
	// cold load. The only mode that can catch a cold-path bug.
	ModeCold RunMode = "cold"
	// ModeWarm ensures the model is resident first, so the probe measures
	// steady-state behavior with no load latency in the numbers.
	ModeWarm RunMode = "warm"
	// ModeBoth runs the probe twice, cold then warm. A DISAGREEMENT between the
	// two is the finding — it is exactly the bonsai signature.
	ModeBoth RunMode = "both"
)

// ValidRunModes lists the accepted values for error messages and validation.
var ValidRunModes = []RunMode{ModeAny, ModeCold, ModeWarm, ModeBoth}

// Valid reports whether m is a known mode.
func (m RunMode) Valid() bool {
	for _, v := range ValidRunModes {
		if m == v {
			return true
		}
	}
	return false
}

// Modes expands a mode into the concrete passes to run. ModeBoth becomes two.
func (m RunMode) Modes() []RunMode {
	if m == ModeBoth {
		return []RunMode{ModeCold, ModeWarm}
	}
	return []RunMode{m}
}

// residencyClient drives corrallm's admin control surface
// (POST /api/v1/models/{load,unload}), which is gated by the admin token.
type residencyClient struct {
	base  string
	token string
	http  *http.Client
}

// newResidencyClient builds a client, or nil when no admin token is available.
// A nil client is not an error: probes that never ask for cold/warm do not need
// one, and requiring a token to run a plain quality benchmark would be a
// regression for every existing probe.
func newResidencyClient(cfg Config) *residencyClient {
	tok := ""
	if p := cfg.LLM.AdminTokenFile; p != "" {
		if b, err := os.ReadFile(p); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	if tok == "" && cfg.LLM.AdminTokenEnv != "" {
		tok = strings.TrimSpace(os.Getenv(cfg.LLM.AdminTokenEnv))
	}
	if tok == "" {
		return nil
	}
	return &residencyClient{
		base:  strings.TrimRight(cfg.LLM.BaseURL, "/"),
		token: tok,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

type residencyResult struct {
	OK      bool   `json:"ok"`
	Evicted int    `json:"evicted"`
	Message string `json:"message"`
}

func (c *residencyClient) post(ctx context.Context, path, model string) (residencyResult, error) {
	var out residencyResult
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return out, err
	}
	base := strings.TrimSuffix(c.base, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s -> HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// UnloadAll evicts every evictable resident, freeing the GPU entirely.
func (c *residencyClient) UnloadAll(ctx context.Context) (residencyResult, error) {
	return c.post(ctx, "/api/v1/models/unload-all", "")
}

// Unload evicts a model's resident backends.
func (c *residencyClient) Unload(ctx context.Context, model string) (residencyResult, error) {
	return c.post(ctx, "/api/v1/models/unload", model)
}

// Load spawns/warms a model.
func (c *residencyClient) Load(ctx context.Context, model string) (residencyResult, error) {
	return c.post(ctx, "/api/v1/models/load", model)
}

// prepareResidency puts the model into the state mode asks for, and reports
// what actually happened.
//
// The returned note is recorded on every row of the pass. It is NOT cosmetic: a
// cold probe that silently ran warm is worse than one that failed, because its
// pass is then evidence for a claim it never tested. corrallm refuses to evict
// pinned or in-flight models, so "cold" is a request, not a guarantee — persistent
// models can never go cold and this is where that surfaces.
func prepareResidency(ctx context.Context, c *residencyClient, mode RunMode, model string, exclusive bool) string {
	if mode == ModeAny {
		return ""
	}
	if c == nil {
		return fmt.Sprintf("WARNING: %s requested but no admin token configured — residency NOT controlled, result does not prove the %s path", mode, mode)
	}
	switch mode {
	case ModeCold:
		if exclusive {
			// Under an exclusive lease, clear the whole GPU. A footprint read
			// with a neighbour still resident measures the neighbour too, and
			// a "cold" load that had to evict someone mid-request is not the
			// clean cold path we meant to test.
			res, err := c.UnloadAll(ctx)
			if err != nil {
				return fmt.Sprintf("WARNING: exclusive cold requested but unload-all failed (%v) — other models may still be resident", err)
			}
			log.Printf("llm-bench: exclusive cold pass — evicted %d backend(s)", res.Evicted)
			return fmt.Sprintf("cold (exclusive): evicted %d backend(s)", res.Evicted)
		}
		res, err := c.Unload(ctx, model)
		if err != nil {
			return fmt.Sprintf("WARNING: cold requested but unload failed (%v) — model may still be resident", err)
		}
		if !res.OK {
			// Pinned/persistent/in-flight. Say so plainly rather than letting a
			// warm run masquerade as a cold one.
			return fmt.Sprintf("WARNING: cold requested but NOT evicted (%s) — this pass ran against a possibly-warm model", res.Message)
		}
		log.Printf("llm-bench: cold pass — evicted %d backend(s) of %s", res.Evicted, model)
		return fmt.Sprintf("cold: evicted %d backend(s)", res.Evicted)
	case ModeWarm:
		res, err := c.Load(ctx, model)
		if err != nil {
			return fmt.Sprintf("WARNING: warm requested but load failed (%v) — first request will pay the cold load", err)
		}
		if !res.OK {
			return fmt.Sprintf("WARNING: warm requested but load reported: %s", res.Message)
		}
		log.Printf("llm-bench: warm pass — %s resident", model)
		return "warm: model resident before the probe"
	}
	return ""
}

// --- measurement publishing -------------------------------------------------
//
// llm-bench is the authoritative measurer: it controls residency, so it can
// measure a model in isolation, cold, with nothing else contending for the GPU.
// corrallm's own in-serving measurement stays as a fallback, but it can only
// observe whatever states live traffic happens to produce, and its calibration
// probe has to spawn an extra slot on a live server to get a second data point.

// residentView is the subset of corrallm's residency op llm-bench needs.
type residentView struct {
	// The residency op names this "modelName" (a backend's "name" is
	// "<model>#<index>"). Decoding it as "model" silently matched nothing, so
	// every footprint read returned ok=false and published a 0 MiB profile — a
	// wrong measurement that looked like an absent one.
	Model         string `json:"modelName"`
	FootprintMiB  int    `json:"footprintMiB"`
	BaseMiB       int    `json:"baseMiB"`
	PerSlotMiB    int    `json:"perSlotMiB"`
	PeakMiB       int    `json:"peakMiB"`
	MeasuredSlots int    `json:"measuredSlots"`
	TunedSlots    int    `json:"tunedSlots"`
	ConfigSlots   int    `json:"configSlots"`
}

// Residency reads corrallm's live residency view.
func (c *residencyClient) Residency(ctx context.Context) ([]residentView, error) {
	base := strings.TrimSuffix(c.base, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/residency", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("residency -> HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []residentView `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Models, nil
}

// MeasureResident reads the model's live footprint. Returns ok=false when the
// model is not currently resident — measuring a model that is not loaded would
// record a zero footprint as though it were a real measurement.
func (c *residencyClient) MeasureResident(ctx context.Context, model string) (residentView, bool) {
	views, err := c.Residency(ctx)
	if err != nil {
		return residentView{}, false
	}
	for _, v := range views {
		if v.Model == model && v.FootprintMiB > 0 {
			return v, true
		}
	}
	return residentView{}, false
}

// PublishTune sends a measured VRAM profile back to corrallm.
func (c *residencyClient) PublishTune(ctx context.Context, model string, v residentView) error {
	base := strings.TrimSuffix(c.base, "/v1")
	slots := v.TunedSlots
	if slots <= 0 {
		slots = v.ConfigSlots
	}
	payload := map[string]any{
		"model":         model,
		"baseMiB":       v.BaseMiB,
		"perSlotMiB":    v.PerSlotMiB,
		"peakMiB":       v.PeakMiB,
		"measuredSlots": slots,
		"footprintMiB":  v.FootprintMiB,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/measurements/tune", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("publish tune -> HTTP %d", resp.StatusCode)
	}
	return nil
}

// PublishCapability sends an OBSERVED capability verdict back to corrallm.
//
// The verdict carries its runMode because a modality can work warm and fail
// cold; a verdict without its residency state is not interpretable, and
// publishing only the warm one is how a broken cold path stays invisible.
func (c *residencyClient) PublishCapability(ctx context.Context, model, modality, runMode, probe, detail string, verified bool) error {
	base := strings.TrimSuffix(c.base, "/v1")
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"modality": modality,
		"runMode":  runMode,
		"verified": verified,
		"probe":    probe,
		"detail":   detail,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/measurements/capability", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("publish capability -> HTTP %d", resp.StatusCode)
	}
	return nil
}

// publishMeasurements feeds corrallm what this pass observed: the model's live
// VRAM footprint, and — for a capability probe — whether the declared modality
// actually worked.
//
// Best-effort by design. A publish failure must never fail a benchmark: the
// measurement is a by-product of the run, not its purpose, and losing it is
// strictly better than losing the run. Failures are logged, not swallowed
// silently, because a measurement pipeline that quietly stops publishing looks
// exactly like one with nothing to report.
// CapabilityObservation is one repeat's verdict on one capability probe.
type CapabilityObservation struct {
	Verified bool
	Detail   string
}

// publishMeasurements reads VRAM while the model is still resident and returns
// the footprint plus this pass's capability observation (nil when the probe says
// nothing about a modality).
//
// The capability verdict is NOT published here any more. One pass is one sample,
// and llm-bench sends no temperature or seed, so a model configured with
// --temp 0.7 is sampled: publishing per pass turned a coin flip into an
// authoritative "this modality does not work". Observed on
// ternary-bonsai-27b/capability-vision, which failed cold and passed warm on
// identical input and was recorded as a cold-path capability failure. The caller
// collects observations across repeats and publishes once via
// publishCapabilityVerdict.
func publishMeasurements(ctx context.Context, c *residencyClient, model string, mode RunMode, tsk *task.Task, rows []Row) (int, *CapabilityObservation) {
	if c == nil || len(rows) == 0 {
		return 0, nil
	}

	footprint := 0
	// VRAM: read it while the model is still resident from this pass.
	if v, ok := c.MeasureResident(ctx, model); ok {
		footprint = v.FootprintMiB
		if err := c.PublishTune(ctx, model, v); err != nil {
			log.Printf("llm-bench: publish tune profile for %s failed: %v", model, err)
		} else {
			log.Printf("llm-bench: published VRAM profile for %s (footprint %d MiB, %d slots)",
				model, v.FootprintMiB, v.MeasuredSlots)
		}
	}

	// Capability verdict: only a capability-class probe declaring a modality
	// says anything about a modality. A coding task passing proves nothing
	// about vision, and publishing a verdict from one would be worse than
	// publishing none — it would assert, with authority, something never tested.
	if tsk.Class != "capability" || tsk.Requires.Modality == "" {
		return footprint, nil
	}
	obs := &CapabilityObservation{Verified: true}
	for _, r := range rows {
		if !r.Pass {
			obs.Verified = false
			for _, ck := range r.Checks {
				if !ck.Pass {
					obs.Detail = ck.Desc + " \u2014 " + ck.Detail
					break
				}
			}
			break
		}
	}
	return footprint, obs
}

// publishCapabilityVerdict publishes ONE verdict for a probe from every repeat
// of it, and reports flakiness rather than hiding it.
//
// A capability is claimed as verified if ANY repeat observed it working: the
// question is whether the model can do this at all, and a model that described
// the image correctly once demonstrably can. Reliability is a different axis and
// belongs in the detail, not in a boolean that routing reads.
//
// The asymmetry is deliberate. Publishing verified=false needs EVERY repeat to
// agree, because a false verdict asserts a capability is absent and one sampled
// miss is not evidence of that. Publishing verified=true needs one success,
// because a success cannot be a false positive in the way a failure can be a
// false negative — the pixels either arrived or they did not.
func publishCapabilityVerdict(ctx context.Context, c *residencyClient, model string, mode RunMode, tsk *task.Task, obs []CapabilityObservation) {
	if c == nil || len(obs) == 0 {
		return
	}
	passed := 0
	detail := ""
	for _, o := range obs {
		if o.Verified {
			passed++
		} else if detail == "" {
			detail = o.Detail
		}
	}
	verified := passed > 0
	switch {
	case passed == len(obs):
		detail = ""
	case passed > 0:
		// Flaky: say so in the detail so a reader is not told a shaky capability
		// is solid. Without repeats (--runs 1) this branch cannot be reached, so
		// a single run still reads exactly as it did before.
		detail = fmt.Sprintf("FLAKY: observed in %d of %d runs \u2014 %s", passed, len(obs), detail)
	}
	if err := c.PublishCapability(ctx, model, tsk.Requires.Modality, string(mode), tsk.Name, detail, verified); err != nil {
		log.Printf("llm-bench: publish capability verdict for %s failed: %v", model, err)
		return
	}
	log.Printf("llm-bench: %s modality=%s runMode=%q verified=%v (%d/%d runs)",
		model, tsk.Requires.Modality, mode, verified, passed, len(obs))
}

// Skip records a probe that was never a candidate for a model, and why. It is
// not a Row: no stage ran, so it has no metrics and must not enter any average.
type Skip struct {
	Model      string
	Task       string
	Class      string
	Capability string
	Reason     string
}

// PublishProbeResults sends per-probe detail to corrallm at the end of a run.
//
// PublishResults collapses a model's whole matrix into one pass rate, which is
// only comparable across models that ran comparable probes — and they don't.
// Probes a model cannot serve are skipped, so an STT model is scored on four
// audio probes and a chat model on twenty mixed ones, and the flat table ranks
// the former above the latter. These rows carry the probe's capability, so the
// dashboard can compare chat to chat, and its name, so "how did the model do"
// has an answer beyond a percentage.
//
// Rows are folded to one record per (model, probe, runMode): the probe, not the
// stage, is the unit a reader reasons about, and cold-vs-warm stays split
// because a disagreement between them is itself the finding.
func PublishProbeResults(ctx context.Context, c *residencyClient, runID, outDir string, rows []Row, skips []Skip) {
	if c == nil || runID == "" {
		return
	}
	// The key is the ARM, not just the probe. Toolset and tool format are the
	// A/B dimensions a run varies on purpose; folding them together averages
	// the arms into one number and destroys the comparison they exist to make.
	type key struct{ model, probe, mode, toolset, format string }
	type agg struct {
		class, capability         string
		stages, passed            int
		checksPassed, checksTotal int
		newPrompt, completion     int
		wall                      int64
		note                      string
	}
	byProbe := map[key]*agg{}
	var order []key
	for _, r := range rows {
		k := key{r.Model, r.Task, r.RunMode, r.Toolset, r.ToolFormat}
		a := byProbe[k]
		if a == nil {
			a = &agg{class: r.Class, capability: r.Capability}
			byProbe[k] = a
			order = append(order, k)
		}
		a.stages++
		if r.Pass {
			a.passed++
		}
		a.checksPassed += r.ChecksPassed
		a.checksTotal += r.ChecksTotal
		// NewPromptTokens, not PromptTokens: the cached prefix is re-sent every
		// turn and evaluated once, so summing the prompt charges the tool schema
		// per turn — which made a ~12% real gap between two arms look like 2.2x.
		a.newPrompt += r.NewPromptTokens
		a.completion += r.CompletionTokens
		a.wall += r.WallMs
		// First note wins: the earliest failing stage is the one that explains
		// the probe: later stages fail downstream of it.
		if a.note == "" && !r.Pass && r.Note != "" {
			a.note = r.Note
		}
	}
	type record struct {
		Model            string `json:"model"`
		Probe            string `json:"probe"`
		Class            string `json:"class,omitempty"`
		Capability       string `json:"capability,omitempty"`
		RunMode          string `json:"runMode,omitempty"`
		Toolset          string `json:"toolset,omitempty"`
		ToolFormat       string `json:"toolFormat,omitempty"`
		Stages           int    `json:"stages"`
		StagesPassed     int    `json:"stagesPassed"`
		ChecksPassed     int    `json:"checksPassed"`
		ChecksTotal      int    `json:"checksTotal"`
		Pass             bool   `json:"pass"`
		WallMS           int64  `json:"wallMs"`
		NewPromptTokens  int    `json:"newPromptTokens"`
		CompletionTokens int    `json:"completionTokens"`
		Skipped          bool   `json:"skipped,omitempty"`
		SkipReason       string `json:"skipReason,omitempty"`
		Note             string `json:"note,omitempty"`
	}
	recs := make([]record, 0, len(order)+len(skips))
	for _, k := range order {
		a := byProbe[k]
		recs = append(recs, record{
			Model: k.model, Probe: k.probe, Class: a.class, Capability: a.capability,
			RunMode: k.mode, Toolset: k.toolset, ToolFormat: k.format,
			Stages: a.stages, StagesPassed: a.passed,
			ChecksPassed: a.checksPassed, ChecksTotal: a.checksTotal,
			Pass: a.stages > 0 && a.passed == a.stages, WallMS: a.wall,
			NewPromptTokens: a.newPrompt, CompletionTokens: a.completion,
			Note: a.note,
		})
	}
	for _, s := range skips {
		recs = append(recs, record{
			Model: s.Model, Probe: s.Task, Class: s.Class, Capability: s.Capability,
			Skipped: true, SkipReason: s.Reason,
		})
	}
	if len(recs) == 0 {
		return
	}

	// Per-stage detail and per-check verdicts: the evidence behind the score.
	// Published in the same call as the summary so a reader who sees a bad probe
	// can always ask why — a two-call design would leave runs whose second call
	// failed showing a score with no explanation.
	type stageRec struct {
		Model               string  `json:"model"`
		Probe               string  `json:"probe"`
		RunMode             string  `json:"runMode,omitempty"`
		Toolset             string  `json:"toolset,omitempty"`
		ToolFormat          string  `json:"toolFormat,omitempty"`
		Stage               int     `json:"stage"`
		Prompt              string  `json:"prompt,omitempty"`
		Pass                bool    `json:"pass,omitempty"`
		LimitBreached       bool    `json:"limitBreached,omitempty"`
		Note                string  `json:"note,omitempty"`
		Turns               int     `json:"turns,omitempty"`
		ToolCalls           int     `json:"toolCalls,omitempty"`
		NewPromptTokens     int     `json:"newPromptTokens,omitempty"`
		CompletionTokens    int     `json:"completionTokens,omitempty"`
		InvalidArgRetries   int     `json:"invalidArgRetries,omitempty"`
		JSONErrors          int     `json:"jsonErrors,omitempty"`
		RepeatedCalls       int     `json:"repeatedCalls,omitempty"`
		BaitCalls           int     `json:"baitCalls,omitempty"`
		BrokenIntermediates int     `json:"brokenIntermediates,omitempty"`
		Compactions         int     `json:"compactions,omitempty"`
		TokPerSec           float64 `json:"tokPerSec,omitempty"`
		WallMS              int64   `json:"wallMs,omitempty"`
	}
	type checkRec struct {
		Model      string `json:"model"`
		Probe      string `json:"probe"`
		RunMode    string `json:"runMode,omitempty"`
		Toolset    string `json:"toolset,omitempty"`
		ToolFormat string `json:"toolFormat,omitempty"`
		Stage      int    `json:"stage"`
		Idx        int    `json:"idx"`
		Kind       string `json:"kind,omitempty"`
		Desc       string `json:"desc,omitempty"`
		Pass       bool   `json:"pass,omitempty"`
		Detail     string `json:"detail,omitempty"`
	}
	stageRecs := make([]stageRec, 0, len(rows))
	var checkRecs []checkRec
	for _, r := range rows {
		stageRecs = append(stageRecs, stageRec{
			Model: r.Model, Probe: r.Task, RunMode: r.RunMode, Toolset: r.Toolset,
			ToolFormat: r.ToolFormat, Stage: r.Stage, Prompt: r.Prompt, Pass: r.Pass,
			LimitBreached: r.LimitBreached, Note: r.Note, Turns: r.Turns,
			ToolCalls: r.ToolCalls, NewPromptTokens: r.NewPromptTokens,
			CompletionTokens: r.CompletionTokens, InvalidArgRetries: r.InvalidArgRetries,
			JSONErrors: r.JSONErrors, RepeatedCalls: r.RepeatedCalls,
			BaitCalls: r.BaitCalls, BrokenIntermediates: r.BrokenIntermediates,
			Compactions: r.Compactions, TokPerSec: r.TokPerSec, WallMS: r.WallMs,
		})
		for i, ck := range r.Checks {
			checkRecs = append(checkRecs, checkRec{
				Model: r.Model, Probe: r.Task, RunMode: r.RunMode, Toolset: r.Toolset,
				ToolFormat: r.ToolFormat, Stage: r.Stage, Idx: i, Kind: ck.Kind,
				Desc: ck.Desc, Pass: ck.Pass, Detail: ck.Detail,
			})
		}
	}

	host, _ := os.Hostname()
	body, err := json.Marshal(map[string]any{
		"runId": runID, "results": recs,
		"stages": stageRecs, "checks": checkRecs,
		// Where the transcripts and journals landed. corrallm cannot infer it:
		// llm-bench's --out is relative to ITS cwd, so the path depends on how
		// the run was launched, and the host matters because artifacts written
		// on another box are not readable from this one.
		"outDir": absOutDir(outDir), "host": host,
	})
	if err != nil {
		return
	}
	base := strings.TrimSuffix(c.base, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/measurements/probes", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("llm-bench: publish probe results failed: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("llm-bench: publish probe results -> HTTP %d", resp.StatusCode)
		return
	}
	log.Printf("llm-bench: published %d probe result(s), %d stage(s), %d check(s) (%d skipped)",
		len(recs), len(stageRecs), len(checkRecs), len(skips))
}

// absOutDir resolves the run directory to an absolute path so the server can
// read it regardless of its own cwd. Falls back to the original on failure —
// a relative path the server may not resolve still beats no path at all.
func absOutDir(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// PublishResults sends per-model aggregates to corrallm at the end of a run.
//
// This is what makes cross-model comparison possible at all: without it the
// numbers live only in out/<ts>/summary.csv on the bench host, and corrallm —
// the thing with the dashboard — has never seen them.
//
// Aggregated here rather than server-side because the rows are already in hand
// and the shape is the run's own summary; shipping every stage row so corrallm
// could re-derive it would move more data to compute the same thing.
func PublishResults(ctx context.Context, c *residencyClient, runID string, rows []Row, footprints map[string]int) {
	if c == nil || runID == "" {
		return
	}
	type agg struct {
		stages, passed             int
		prompt, cached, completion int
		wall                       int64
		classes                    map[string]bool
	}
	byModel := map[string]*agg{}
	for _, r := range rows {
		a := byModel[r.Model]
		if a == nil {
			a = &agg{classes: map[string]bool{}}
			byModel[r.Model] = a
		}
		a.stages++
		if r.Pass {
			a.passed++
		}
		a.prompt += r.PromptTokens
		a.cached += r.CachedTokens
		a.completion += r.CompletionTokens
		a.wall += r.WallMs
		if r.Class != "" {
			a.classes[r.Class] = true
		}
	}
	for model, a := range byModel {
		var classes []string
		for c := range a.classes {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		tps := 0.0
		if a.wall > 0 {
			tps = float64(a.prompt+a.completion) / (float64(a.wall) / 1000)
		}
		body, err := json.Marshal(map[string]any{
			"runId": runID, "model": model, "classes": strings.Join(classes, ","),
			"stages": a.stages, "stagesPassed": a.passed,
			"promptTokens": a.prompt, "cachedTokens": a.cached, "completionTokens": a.completion,
			"wallMs": a.wall, "tokPerSec": tps, "footprintMiB": footprints[model],
		})
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(c.base, "/v1")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/measurements/result", bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			log.Printf("llm-bench: publish result for %s failed: %v", model, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("llm-bench: publish result for %s -> HTTP %d", model, resp.StatusCode)
			continue
		}
		log.Printf("llm-bench: published result for %s (%d/%d stages)", model, a.passed, a.stages)
	}
}
