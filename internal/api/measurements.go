package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/tune"
)

// Measurement ingest — llm-bench is the authoritative measurer.
//
// corrallm has always measured VRAM opportunistically, from inside the serving
// path: manager.measure() samples a spawn's footprint, and calibrationProbe
// deliberately spawns ONE EXTRA SLOT during live serving purely to gather the
// second (slots, footprint) data point the per-slot slope needs. That works, but
// it perturbs production to take a measurement and it can only observe whatever
// residency states real traffic happens to produce.
//
// llm-bench can do better because it CONTROLS residency: it loads and unloads
// deliberately, measures in isolation, repeats for variance, and does it when
// nobody is being served. These endpoints let it publish what it found so
// corrallm's admission and eviction decisions run on measurements taken on
// purpose rather than measurements taken by accident.
//
// Ingest is additive and NEVER destructive: corrallm's own in-serving
// measurement stays as the fallback for a box where llm-bench has never run.
// A fresh install must not need a benchmark pass before it can schedule.

// TuneProfileInput publishes one measured VRAM profile.
type TuneProfileInput struct {
	Body struct {
		Model string `json:"model" doc:"Served model name."`
		GPU   string `json:"gpu,omitempty" doc:"GPU name the measurement was taken on; defaults to this host's GPU."`

		BaseMiB       int `json:"baseMiB" doc:"Footprint with KV excluded (weights + fixed overhead)."`
		PerSlotMiB    int `json:"perSlotMiB" doc:"KV cache cost per slot."`
		PeakMiB       int `json:"peakMiB,omitempty" doc:"Highest total footprint observed."`
		MeasuredSlots int `json:"measuredSlots" doc:"Slot count (--parallel) the measurement was taken at."`
		Ctx           int `json:"ctx,omitempty" doc:"n_ctx at measurement time."`
		FootprintMiB  int `json:"footprintMiB,omitempty" doc:"Total observed footprint at measuredSlots; recorded as a sample so a second point at a different slot count yields the per-slot slope."`
	}
}

// TuneProfileOutput reports the stored profile.
type TuneProfileOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		GPU     string `json:"gpu"`
		Message string `json:"message"`
	}
}

// PublishTuneProfile ingests an llm-bench VRAM measurement into the tune cache.
func (h *Handlers) PublishTuneProfile(_ context.Context, in *TuneProfileInput) (*TuneProfileOutput, error) {
	out := &TuneProfileOutput{}
	if in.Body.Model == "" {
		out.Body.Message = "model is required"
		return out, nil
	}
	gpuName := in.Body.GPU
	if gpuName == "" {
		// Default to this host's GPU rather than a blank key: a profile stored
		// under "" would never be found by the scheduler, which always looks up
		// by the probed GPU name, and the publish would silently do nothing.
		st, err := gpu.Probe()
		if err != nil {
			out.Body.Message = fmt.Sprintf("gpu not specified and probe failed: %v", err)
			return out, nil
		}
		gpuName = st.Name
	}
	existing, _ := h.Mgr.TuneProfile(gpuName, in.Body.Model)
	// Run through the SAME derivation the serving path uses — one
	// implementation of the base/per-slot split, not two that can drift.
	// kvMiB is passed as 0: a bench reports the split it measured directly, or
	// leaves it to the two-point slope across samples.
	footprint := in.Body.FootprintMiB
	if footprint == 0 {
		footprint = in.Body.BaseMiB + in.Body.PerSlotMiB*in.Body.MeasuredSlots
	}
	p := tune.Derive(existing, tune.SourceBench, footprint, 0, in.Body.MeasuredSlots, in.Body.Ctx, time.Now().Unix())
	// A bench that measured the split directly wins over the slope estimate.
	if in.Body.BaseMiB > 0 {
		p.BaseMiB = in.Body.BaseMiB
	}
	if in.Body.PerSlotMiB > 0 {
		p.PerSlotMiB = in.Body.PerSlotMiB
	}
	if in.Body.PeakMiB > p.PeakMiB {
		p.PeakMiB = in.Body.PeakMiB
	}
	if err := h.Mgr.PublishTuneProfile(gpuName, in.Body.Model, p); err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.GPU = gpuName
	out.Body.Message = fmt.Sprintf("stored profile for %s on %s", in.Body.Model, gpuName)
	return out, nil
}

// VerifiedCapabilityInput publishes what a capability probe actually observed.
type VerifiedCapabilityInput struct {
	Body struct {
		Model string `json:"model" doc:"Served model name."`
		// Modality is the capability that was exercised (image | audio | text).
		Modality string `json:"modality" doc:"Modality exercised by the probe."`
		// RunMode records whether the verdict was obtained cold or warm. A
		// modality can work warm and fail cold (observed on a 27B vision model),
		// so a verdict without its residency state is not interpretable.
		RunMode  string `json:"runMode,omitempty" doc:"Residency state the probe ran against: cold | warm | \"\"."`
		Verified bool   `json:"verified" doc:"Whether the probe actually observed the capability working."`
		Probe    string `json:"probe,omitempty" doc:"Probe name that produced the verdict."`
		Detail   string `json:"detail,omitempty" doc:"Human-readable evidence or failure reason."`
		At       int64  `json:"at,omitempty" doc:"Unix seconds the verdict was taken; defaults to now."`
	}
}

// VerifiedCapabilityOutput acknowledges a verdict.
type VerifiedCapabilityOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
}

// PublishVerifiedCapability records an OBSERVED capability verdict.
//
// This is data corrallm has never had. Modalities are declared in config and
// served from that declaration verbatim — nothing ever checked them against a
// live backend, so a wrong declaration was indistinguishable from a right one.
// A verdict here does not change routing (the declaration still governs what is
// offered); it makes the discrepancy VISIBLE, which is the part that was missing
// when a model advertised vision it silently dropped on its first request.
func (h *Handlers) PublishVerifiedCapability(_ context.Context, in *VerifiedCapabilityInput) (*VerifiedCapabilityOutput, error) {
	out := &VerifiedCapabilityOutput{}
	if in.Body.Model == "" || in.Body.Modality == "" {
		out.Body.Message = "model and modality are required"
		return out, nil
	}
	v := Verdict{
		Modality: in.Body.Modality,
		RunMode:  in.Body.RunMode,
		Verified: in.Body.Verified,
		Probe:    in.Body.Probe,
		Detail:   in.Body.Detail,
		At:       in.Body.At,
	}
	h.Verified.Record(in.Body.Model, v)
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("recorded %s/%s verified=%v", in.Body.Model, in.Body.Modality, in.Body.Verified)
	return out, nil
}

// --- exclusive calibration lease --------------------------------------------

// CalibrateInput starts or extends an exclusive calibration lease.
type CalibrateInput struct {
	Body struct {
		Key string `json:"key" doc:"The caller key llm-bench will present; only this key is served while the lease holds."`
		// TTLSeconds bounds the lease. A calibration run that crashes must not
		// leave the box refusing every caller forever, so this is REQUIRED to be
		// finite and is clamped below.
		TTLSeconds int    `json:"ttlSeconds,omitempty" doc:"Lease duration in seconds (default 900, max 7200). The lease self-expires: a crashed bench cannot lock the box."`
		Reason     string `json:"reason,omitempty" doc:"Shown to turned-away callers and in the dashboard."`
	}
}

// CalibrateOutput reports the lease.
type CalibrateOutput struct {
	Body struct {
		OK        bool   `json:"ok"`
		ExpiresAt int64  `json:"expiresAt,omitempty" doc:"Unix seconds the lease self-expires."`
		Message   string `json:"message"`
		// Warning is always populated on success. Starting a lease EVICTS
		// models and turns away every other caller; a client that fires this
		// without surfacing that has misled its user.
		Warning string `json:"warning,omitempty"`
	}
}

const (
	defaultCalibrationTTL = 900  // 15m
	maxCalibrationTTL     = 7200 // 2h
)

// BeginCalibration claims the box for a measurement run.
//
// While the lease holds, every caller except Key gets 429 + Retry-After. This is
// a real outage for other traffic and is deliberately not silent: the response
// carries an explicit warning, and the lease always self-expires so a crashed
// bench heals the box on its own.
func (h *Handlers) BeginCalibration(_ context.Context, in *CalibrateInput) (*CalibrateOutput, error) {
	out := &CalibrateOutput{}
	if h.Proxy == nil {
		out.Body.Message = "calibration unavailable (no proxy wired)"
		return out, nil
	}
	if in.Body.Key == "" {
		// Without a key the lease would turn away EVERYONE including the bench,
		// which is a self-inflicted outage with no upside.
		out.Body.Message = "key is required (the lease would otherwise block the calibration run itself)"
		return out, nil
	}
	ttl := in.Body.TTLSeconds
	if ttl <= 0 {
		ttl = defaultCalibrationTTL
	}
	if ttl > maxCalibrationTTL {
		ttl = maxCalibrationTTL
	}
	deadline, ok := h.Proxy.Calibration().Begin(in.Body.Key, in.Body.Reason, time.Duration(ttl)*time.Second)
	if !ok {
		out.Body.Message = fmt.Sprintf("a calibration lease is already held by another key until %s", deadline.UTC().Format(time.RFC3339))
		return out, nil
	}
	out.Body.OK = true
	out.Body.ExpiresAt = deadline.Unix()
	out.Body.Message = fmt.Sprintf("calibration lease held until %s", deadline.UTC().Format(time.RFC3339))
	out.Body.Warning = "EXCLUSIVE MODE: every caller except the calibration key now receives 429 + Retry-After, and the run will EVICT resident models to take cold measurements. The lease self-expires at expiresAt even if the run dies."
	return out, nil
}

// EndCalibration releases the lease early. Idempotent, so a bench can always
// call it from a defer without checking whether it still holds one.
func (h *Handlers) EndCalibration(_ context.Context, in *CalibrateInput) (*CalibrateOutput, error) {
	out := &CalibrateOutput{}
	if h.Proxy == nil {
		out.Body.Message = "calibration unavailable (no proxy wired)"
		return out, nil
	}
	h.Proxy.Calibration().End(in.Body.Key)
	out.Body.OK = true
	out.Body.Message = "calibration lease released; normal traffic resumes"
	return out, nil
}

// CalibrationStatusInput has no parameters.
type CalibrationStatusInput struct{}

// CalibrationStatusOutput reports whether a lease is held.
type CalibrationStatusOutput struct {
	Body struct {
		Active           bool   `json:"active"`
		Reason           string `json:"reason,omitempty"`
		RemainingSeconds int    `json:"remainingSeconds,omitempty"`
	}
}

// CalibrationStatus lets the dashboard show that the box is in exclusive mode —
// otherwise a user seeing every request 429 has no way to tell why.
func (h *Handlers) CalibrationStatus(_ context.Context, _ *CalibrationStatusInput) (*CalibrationStatusOutput, error) {
	out := &CalibrationStatusOutput{}
	if h.Proxy == nil {
		return out, nil
	}
	active, reason, remaining := h.Proxy.Calibration().Status()
	out.Body.Active = active
	out.Body.Reason = reason
	out.Body.RemainingSeconds = int(remaining.Seconds())
	return out, nil
}

// --- bench run (corrallm spawns llm-bench) -----------------------------------

// BenchRunInput selects what to run.
type BenchRunInput struct {
	Body struct {
		Models  []string `json:"models,omitempty" doc:"Models to bench; empty = every model in the bench config."`
		Classes []string `json:"classes,omitempty" doc:"Probe classes: capability | coding | tooluse | adversarial. Empty = all."`
		Reason  string   `json:"reason,omitempty" doc:"Shown to turned-away callers."`
		TTL     int      `json:"ttlSeconds,omitempty" doc:"Lease duration; the run is killed and the lease released when it expires."`
	}
}

// BenchRunOutput reports the spawned run.
type BenchRunOutput struct {
	Body struct {
		OK      bool           `json:"ok"`
		Message string         `json:"message"`
		Warning string         `json:"warning,omitempty"`
		Status  BenchRunStatus `json:"status"`
	}
}

// StartBenchRun spawns llm-bench under an exclusive lease.
//
// It runs the SAME binary with the same flags a human would type — the Args in
// the status are the literal invocation, so any run started here is
// reproducible from a shell, and llm-bench stays a first-class CLI rather than
// an implementation detail of the dashboard.
func (h *Handlers) StartBenchRun(_ context.Context, in *BenchRunInput) (*BenchRunOutput, error) {
	out := &BenchRunOutput{}
	if h.Bench == nil || h.Proxy == nil {
		out.Body.Message = "bench runs unavailable (runner or proxy not wired)"
		return out, nil
	}
	calib := h.Proxy.Calibration()
	st, err := h.Bench.Start(BenchStartOptions{
		Bin:        h.BenchBin,
		ConfigPath: h.BenchConfig,
		ProbesDir:  h.BenchProbes,
		Models:     in.Body.Models,
		Classes:    in.Body.Classes,
		TTLSeconds: in.Body.TTL,
		Reason:     in.Body.Reason,
	}, calib.Begin, calib.End)
	if err != nil {
		out.Body.Message = err.Error()
		out.Body.Status = st
		return out, nil
	}
	out.Body.OK = true
	out.Body.Message = "bench run started"
	out.Body.Warning = "EXCLUSIVE MODE: models will be EVICTED to take cold measurements, and every caller except this run receives 429 + Retry-After until it finishes."
	out.Body.Status = st
	return out, nil
}

// BenchStatusInput has no parameters.
type BenchStatusInput struct{}

// BenchStatusOutput reports the current or last run.
type BenchStatusOutput struct {
	Body BenchRunStatus
}

// BenchStatus reports the in-flight (or most recent) bench run.
func (h *Handlers) BenchStatus(_ context.Context, _ *BenchStatusInput) (*BenchStatusOutput, error) {
	out := &BenchStatusOutput{}
	if h.Bench != nil {
		out.Body = h.Bench.Status()
	}
	return out, nil
}

// CancelBenchInput has no parameters.
type CancelBenchInput struct{}

// CancelBenchRun stops an in-flight run; the lease is released by the run's own
// exit path, so cancelling can never strand the lockout.
func (h *Handlers) CancelBenchRun(_ context.Context, _ *CancelBenchInput) (*BenchStatusOutput, error) {
	out := &BenchStatusOutput{}
	if h.Bench != nil {
		h.Bench.Cancel()
		out.Body = h.Bench.Status()
	}
	return out, nil
}

// UnloadAllInput has no parameters.
type UnloadAllInput struct{}

// UnloadAllOutput reports a mass eviction.
type UnloadAllOutput struct {
	Body struct {
		OK      bool              `json:"ok"`
		Evicted int               `json:"evicted"`
		Skipped map[string]string `json:"skipped,omitempty" doc:"Residents that could NOT be evicted, and why (pinned / in-flight)."`
		Message string            `json:"message"`
	}
}

// UnloadAllModels frees the GPU for a calibration run.
//
// Skipped residents are reported rather than treated as failure: a pinned
// embedder cannot be evicted, and refusing the whole call because of it would
// make calibration impossible on any box with a preloaded model. The caller
// decides whether the remaining occupancy invalidates its measurement.
func (h *Handlers) UnloadAllModels(_ context.Context, _ *UnloadAllInput) (*UnloadAllOutput, error) {
	out := &UnloadAllOutput{}
	if h.Mgr == nil {
		out.Body.Message = "no manager"
		return out, nil
	}
	n, skipped := h.Mgr.UnloadAll()
	out.Body.OK = true
	out.Body.Evicted = n
	out.Body.Skipped = skipped
	out.Body.Message = fmt.Sprintf("evicted %d backend(s), %d could not be evicted", n, len(skipped))
	return out, nil
}

// --- bench results ----------------------------------------------------------

// BenchResultInput publishes one model's aggregate outcome from a run.
type BenchResultInput struct {
	Body struct {
		RunID            string  `json:"runId" doc:"The run's timestamp id (llm-bench out/<ts>)."`
		Model            string  `json:"model"`
		Classes          string  `json:"classes,omitempty" doc:"Probe classes that ran, comma separated."`
		Stages           int     `json:"stages"`
		StagesPassed     int     `json:"stagesPassed"`
		PromptTokens     int     `json:"promptTokens"`
		CachedTokens     int     `json:"cachedTokens" doc:"Prompt tokens served from cache; excluded from 'processed' so cache hits don't flatter whichever model ran second."`
		CompletionTokens int     `json:"completionTokens"`
		WallMS           int64   `json:"wallMs"`
		TokPerSec        float64 `json:"tokPerSec"`
		FootprintMiB     int     `json:"footprintMiB,omitempty"`
		At               int64   `json:"at,omitempty"`
	}
}

// BenchResultOutput acknowledges a published result.
type BenchResultOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
}

// PublishBenchResult records one model's aggregate from a bench run.
//
// Upserted by (runId, model), so a retried publish cannot double-count a run.
func (h *Handlers) PublishBenchResult(ctx context.Context, in *BenchResultInput) (*BenchResultOutput, error) {
	out := &BenchResultOutput{}
	if h.Store == nil {
		out.Body.Message = "no store"
		return out, nil
	}
	if in.Body.RunID == "" || in.Body.Model == "" {
		out.Body.Message = "runId and model are required"
		return out, nil
	}
	at := in.Body.At
	if at == 0 {
		at = time.Now().Unix()
	}
	err := h.Store.SaveBenchResult(ctx, store.BenchResult{
		RunID: in.Body.RunID, Model: in.Body.Model, At: at, Classes: in.Body.Classes,
		Stages: in.Body.Stages, StagesPassed: in.Body.StagesPassed,
		PromptTokens: in.Body.PromptTokens, CachedTokens: in.Body.CachedTokens,
		CompletionTokens: in.Body.CompletionTokens, WallMS: in.Body.WallMS,
		TokPerSec: in.Body.TokPerSec, FootprintMiB: in.Body.FootprintMiB,
	})
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("recorded %s for run %s", in.Body.Model, in.Body.RunID)
	return out, nil
}

// --- per-probe bench detail -------------------------------------------------

// BenchProbeRecord is one probe's outcome for one model, at one residency mode.
type BenchProbeRecord struct {
	Model        string `json:"model"`
	Probe        string `json:"probe" doc:"Probe (task) name."`
	Class        string `json:"class,omitempty" doc:"coding | tooluse | adversarial | capability."`
	Capability   string `json:"capability,omitempty" doc:"Serving surface the PROBE required: chat, audio.stt, …"`
	RunMode      string `json:"runMode,omitempty" doc:"Residency the pass ran against: cold | warm | empty."`
	Stages       int    `json:"stages"`
	StagesPassed int    `json:"stagesPassed"`
	ChecksPassed int    `json:"checksPassed"`
	ChecksTotal  int    `json:"checksTotal"`
	Pass         bool   `json:"pass"`
	WallMS       int64  `json:"wallMs"`
	Skipped      bool   `json:"skipped,omitempty" doc:"Probe was never a candidate for this model — a configuration fact, not a failure."`
	SkipReason   string `json:"skipReason,omitempty"`
	Note         string `json:"note,omitempty" doc:"First failing check, or the combo error."`
}

// BenchProbePublish is one probe's outcome as PUBLISHED.
//
// Deliberately not BenchProbeRecord: every measurement field here is optional,
// because a skipped probe legitimately carries none of them. Huma derives
// "required" from the absence of omitempty, so reusing the read struct made a
// skip record — the exact shape this feature exists to persist — fail
// validation with 422.
type BenchProbePublish struct {
	Model        string `json:"model"`
	Probe        string `json:"probe" doc:"Probe (task) name."`
	Class        string `json:"class,omitempty"`
	Capability   string `json:"capability,omitempty"`
	RunMode      string `json:"runMode,omitempty"`
	Stages       int    `json:"stages,omitempty"`
	StagesPassed int    `json:"stagesPassed,omitempty"`
	ChecksPassed int    `json:"checksPassed,omitempty"`
	ChecksTotal  int    `json:"checksTotal,omitempty"`
	Pass         bool   `json:"pass,omitempty"`
	WallMS       int64  `json:"wallMs,omitempty"`
	Skipped      bool   `json:"skipped,omitempty"`
	SkipReason   string `json:"skipReason,omitempty"`
	Note         string `json:"note,omitempty"`
}

// BenchProbeResultsInput publishes a run's per-probe detail in one batch.
type BenchProbeResultsInput struct {
	Body struct {
		RunID   string              `json:"runId"`
		At      int64               `json:"at,omitempty"`
		Results []BenchProbePublish `json:"results"`
	}
}

// BenchProbeResultsOutput acknowledges a published batch.
type BenchProbeResultsOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Saved   int    `json:"saved"`
		Message string `json:"message"`
	}
}

// PublishBenchProbeResults records a run's per-probe rows.
//
// Separate from PublishBenchResult rather than folded into it: the aggregate is
// one row and this is a batch, and the aggregate must keep working unchanged for
// runs published by an older llm-bench that knows nothing about probe detail.
func (h *Handlers) PublishBenchProbeResults(ctx context.Context, in *BenchProbeResultsInput) (*BenchProbeResultsOutput, error) {
	out := &BenchProbeResultsOutput{}
	if h.Store == nil {
		out.Body.Message = "no store"
		return out, nil
	}
	if in.Body.RunID == "" {
		out.Body.Message = "runId is required"
		return out, nil
	}
	at := in.Body.At
	if at == 0 {
		at = time.Now().Unix()
	}
	rows := make([]store.BenchProbeResult, 0, len(in.Body.Results))
	for _, r := range in.Body.Results {
		if r.Model == "" || r.Probe == "" {
			continue
		}
		rows = append(rows, store.BenchProbeResult{
			RunID: in.Body.RunID, Model: r.Model, At: at, Probe: r.Probe,
			Class: r.Class, Capability: r.Capability, RunMode: r.RunMode,
			Stages: r.Stages, StagesPassed: r.StagesPassed,
			ChecksPassed: r.ChecksPassed, ChecksTotal: r.ChecksTotal,
			Pass: r.Pass, WallMS: r.WallMS, Skipped: r.Skipped,
			SkipReason: r.SkipReason, Note: r.Note,
		})
	}
	if err := h.Store.SaveBenchProbeResults(ctx, rows); err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Saved = len(rows)
	out.Body.Message = fmt.Sprintf("recorded %d probe result(s) for run %s", len(rows), in.Body.RunID)
	return out, nil
}

// BenchCapabilityView is one capability's score for a model in one run — the
// unit that is actually comparable across models.
type BenchCapabilityView struct {
	Capability   string             `json:"capability"`
	Stages       int                `json:"stages"`
	StagesPassed int                `json:"stagesPassed"`
	Score        float64            `json:"score" doc:"Stage pass rate 0..1 within this capability."`
	Probes       []BenchProbeRecord `json:"probes"`
	// SkippedProbes counts probes in this capability the model was not a
	// candidate for. A capability whose probes ALL skipped has no score, and the
	// UI must say "not applicable" rather than render 0% or omit it silently.
	SkippedProbes int `json:"skippedProbes"`
}

// BenchProbesInput scopes probe detail to one model, optionally one run.
type BenchProbesInput struct {
	Model string `query:"model" required:"true" doc:"Model whose probe detail to return."`
	RunID string `query:"runId" doc:"Scope to one run; omit for the model's most recent run."`
}

// BenchProbesOutput is one model's last (or named) run, grouped by capability.
type BenchProbesOutput struct {
	Body struct {
		RunID        string                `json:"runId"`
		Model        string                `json:"model"`
		At           int64                 `json:"at"`
		Capabilities []BenchCapabilityView `json:"capabilities"`
	}
}

// BenchProbeDetail returns a model's per-probe results grouped by capability.
//
// Grouped server-side because the grouping IS the fix: a flat list re-invites
// the reader to average an STT model's audio probes against a chat model's chat
// probes, which is the comparison that made a speech model look like it could
// hold a conversation.
func (h *Handlers) BenchProbeDetail(ctx context.Context, in *BenchProbesInput) (*BenchProbesOutput, error) {
	out := &BenchProbesOutput{}
	out.Body.Model = in.Model
	out.Body.Capabilities = []BenchCapabilityView{}
	if h.Store == nil || in.Model == "" {
		return out, nil
	}
	rows, err := h.Store.BenchProbeResultsFor(ctx, in.Model, in.RunID)
	if err != nil {
		return out, err
	}
	byCap := map[string]*BenchCapabilityView{}
	var order []string
	for _, r := range rows {
		out.Body.RunID = r.RunID
		if r.At > out.Body.At {
			out.Body.At = r.At
		}
		capName := r.Capability
		if capName == "" {
			capName = "chat"
		}
		v := byCap[capName]
		if v == nil {
			v = &BenchCapabilityView{Capability: capName, Probes: []BenchProbeRecord{}}
			byCap[capName] = v
			order = append(order, capName)
		}
		v.Probes = append(v.Probes, BenchProbeRecord{
			Model: r.Model, Probe: r.Probe, Class: r.Class, Capability: capName,
			RunMode: r.RunMode, Stages: r.Stages, StagesPassed: r.StagesPassed,
			ChecksPassed: r.ChecksPassed, ChecksTotal: r.ChecksTotal, Pass: r.Pass,
			WallMS: r.WallMS, Skipped: r.Skipped, SkipReason: r.SkipReason, Note: r.Note,
		})
		// Skipped probes contribute to neither numerator nor denominator: they
		// produced no measurement, and counting them either way would restate
		// a configuration fact as a score.
		if r.Skipped {
			v.SkippedProbes++
			continue
		}
		v.Stages += r.Stages
		v.StagesPassed += r.StagesPassed
	}
	sort.Strings(order)
	for _, name := range order {
		v := byCap[name]
		if v.Stages > 0 {
			v.Score = float64(v.StagesPassed) / float64(v.Stages)
		}
		out.Body.Capabilities = append(out.Body.Capabilities, *v)
	}
	return out, nil
}

// BenchCapabilityModelView is one model's score WITHIN one capability.
type BenchCapabilityModelView struct {
	Model         string  `json:"model"`
	RunID         string  `json:"runId"`
	At            int64   `json:"at"`
	Stages        int     `json:"stages"`
	StagesPassed  int     `json:"stagesPassed"`
	Score         float64 `json:"score" doc:"Stage pass rate 0..1 within this capability only."`
	Probes        int     `json:"probes" doc:"Probes that actually ran."`
	SkippedProbes int     `json:"skippedProbes"`
}

// BenchCapabilityMatrixEntry ranks the models that were measured on one
// capability against each other.
type BenchCapabilityMatrixEntry struct {
	Capability string                     `json:"capability"`
	Models     []BenchCapabilityModelView `json:"models"`
}

// BenchCapabilityMatrixOutput is the cross-model comparison, split by surface.
type BenchCapabilityMatrixOutput struct {
	Body struct {
		Capabilities []BenchCapabilityMatrixEntry `json:"capabilities"`
	}
}

// BenchCapabilityMatrix ranks models within each capability, using each model's
// most recent run.
//
// The flat cross-model table this supplements ranks on a run-wide pass rate,
// which is not a comparable quantity: probes a model cannot serve are skipped,
// so each model is scored on a different set. An STT model passing its four
// audio probes outranked a chat model that passed eighteen of twenty. Ranking
// only WITHIN a capability is what makes the ordering mean something — a model
// appears in a row only if it was actually measured on that surface.
func (h *Handlers) BenchCapabilityMatrix(ctx context.Context, _ *struct{}) (*BenchCapabilityMatrixOutput, error) {
	out := &BenchCapabilityMatrixOutput{}
	out.Body.Capabilities = []BenchCapabilityMatrixEntry{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.LatestBenchProbeResults(ctx)
	if err != nil {
		return out, err
	}
	type acc struct {
		view  BenchCapabilityModelView
		order int
	}
	byCap := map[string]map[string]*acc{}
	var capOrder []string
	for _, r := range rows {
		capName := r.Capability
		if capName == "" {
			capName = "chat"
		}
		models := byCap[capName]
		if models == nil {
			models = map[string]*acc{}
			byCap[capName] = models
			capOrder = append(capOrder, capName)
		}
		a := models[r.Model]
		if a == nil {
			a = &acc{view: BenchCapabilityModelView{Model: r.Model, RunID: r.RunID, At: r.At}}
			models[r.Model] = a
		}
		if r.Skipped {
			a.view.SkippedProbes++
			continue
		}
		a.view.Probes++
		a.view.Stages += r.Stages
		a.view.StagesPassed += r.StagesPassed
	}
	sort.Strings(capOrder)
	for _, capName := range capOrder {
		entry := BenchCapabilityMatrixEntry{Capability: capName, Models: []BenchCapabilityModelView{}}
		for _, a := range byCap[capName] {
			// A model whose every probe on this surface was skipped was never
			// measured on it, so it must not appear in the ranking at all —
			// listing it at 0% would assert a failure that never happened.
			if a.view.Probes == 0 {
				continue
			}
			if a.view.Stages > 0 {
				a.view.Score = float64(a.view.StagesPassed) / float64(a.view.Stages)
			}
			entry.Models = append(entry.Models, a.view)
		}
		if len(entry.Models) == 0 {
			continue
		}
		// Best first, then by name so equal scores have a stable order rather
		// than SQLite's map iteration deciding the ranking.
		sort.Slice(entry.Models, func(i, j int) bool {
			if entry.Models[i].Score != entry.Models[j].Score {
				return entry.Models[i].Score > entry.Models[j].Score
			}
			return entry.Models[i].Model < entry.Models[j].Model
		})
		out.Body.Capabilities = append(out.Body.Capabilities, entry)
	}
	return out, nil
}

// BenchResultView is one model's comparable outcome.
type BenchResultView struct {
	RunID string `json:"runId"`
	Model string `json:"model"`
	At    int64  `json:"at"`
	// Score is the stage pass rate 0..1 — the quality axis of a comparison.
	Score           float64 `json:"score"`
	Stages          int     `json:"stages"`
	StagesPassed    int     `json:"stagesPassed"`
	Classes         string  `json:"classes,omitempty"`
	TokensProcessed int     `json:"tokensProcessed" doc:"Prompt tokens EXCLUDING cache hits."`
	TokensGenerated int     `json:"tokensGenerated"`
	CachedTokens    int     `json:"cachedTokens"`
	WallMS          int64   `json:"wallMs"`
	TokPerSec       float64 `json:"tokPerSec"`
	FootprintMiB    int     `json:"footprintMiB"`
}

// BenchResultsInput optionally scopes to one model.
type BenchResultsInput struct {
	Model string `query:"model" doc:"Only this model's history; omit for the latest per model."`
	Limit int    `query:"limit" doc:"History depth when model is set (default 20)."`
}

// BenchResultsOutput lists comparable results.
type BenchResultsOutput struct {
	Body struct {
		Results []BenchResultView `json:"results"`
	}
}

// BenchResults returns the latest result per model, or one model's history.
func (h *Handlers) BenchResults(ctx context.Context, in *BenchResultsInput) (*BenchResultsOutput, error) {
	out := &BenchResultsOutput{}
	if h.Store == nil {
		return out, nil
	}
	var (
		rows []store.BenchResult
		err  error
	)
	if in.Model != "" {
		rows, err = h.Store.BenchResultsFor(ctx, in.Model, in.Limit)
	} else {
		rows, err = h.Store.LatestBenchResults(ctx)
	}
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		score := 0.0
		if r.Stages > 0 {
			score = float64(r.StagesPassed) / float64(r.Stages)
		}
		// Cache hits are subtracted, not counted: a model that ran second over
		// the same fixtures gets cheap prompt tokens through no merit of its own,
		// and comparing raw prompt totals would reward the running order.
		processed := r.PromptTokens - r.CachedTokens
		if processed < 0 {
			processed = 0
		}
		out.Body.Results = append(out.Body.Results, BenchResultView{
			RunID: r.RunID, Model: r.Model, At: r.At, Score: score,
			Stages: r.Stages, StagesPassed: r.StagesPassed, Classes: r.Classes,
			TokensProcessed: processed, TokensGenerated: r.CompletionTokens,
			CachedTokens: r.CachedTokens, WallMS: r.WallMS,
			TokPerSec: r.TokPerSec, FootprintMiB: r.FootprintMiB,
		})
	}
	return out, nil
}
