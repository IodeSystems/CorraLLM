package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	Model         string `json:"model"`
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
func publishMeasurements(ctx context.Context, c *residencyClient, model string, mode RunMode, tsk *task.Task, rows []Row) {
	if c == nil || len(rows) == 0 {
		return
	}

	// VRAM: read it while the model is still resident from this pass.
	if v, ok := c.MeasureResident(ctx, model); ok {
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
		return
	}
	verified := true
	var detail string
	for _, r := range rows {
		if !r.Pass {
			verified = false
			for _, ck := range r.Checks {
				if !ck.Pass {
					detail = ck.Desc + " — " + ck.Detail
					break
				}
			}
			break
		}
	}
	if err := c.PublishCapability(ctx, model, tsk.Requires.Modality, string(mode), tsk.Name, detail, verified); err != nil {
		log.Printf("llm-bench: publish capability verdict for %s failed: %v", model, err)
	} else {
		log.Printf("llm-bench: %s modality=%s runMode=%q verified=%v", model, tsk.Requires.Modality, mode, verified)
	}
}
