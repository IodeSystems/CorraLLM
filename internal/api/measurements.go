package api

import (
	"context"
	"fmt"
	"time"

	"github.com/iodesystems/corrallm/internal/gpu"
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
