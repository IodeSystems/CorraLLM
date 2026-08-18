// Package api holds corrallm's typed handlers and the gat gateway wiring. Each
// operation is registered once with gat.Register and is thereby reachable over
// REST (huma), GraphQL, and gRPC — the "register once → typed everywhere" loop.
// P0 ships the meta operations (health, version) that exercise the whole
// codegen pipeline; the inference proxy + scheduler operations land in P1+.
package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/agentdist"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/sysmem"
	"github.com/iodesystems/corrallm/internal/toolchain"
)

// Handlers carries the dependencies every operation needs. It grows as phases
// add the scheduler, residency, and cost subsystems.
type Handlers struct {
	Version string
	// Cfg is the config this Handlers was CONSTRUCTED with. It is written once,
	// before any request can reach it, and never again — so reading it needs no
	// synchronization. On reload, SetConfig stores a newer config in `live`
	// instead, and every read goes through h.config(), which prefers it.
	//
	// Two fields rather than one atomic because Handlers is built with a struct
	// literal in a dozen places; an atomic field would make every one of them a
	// two-step construction for no gain in a read-only view layer.
	Cfg   *config.Config
	live  atomic.Pointer[config.Config]
	Store *store.Store
	Mgr   *proc.Manager // residency introspection (P8)
	// Tools answers "what tool is installed where, at what version" (P25b).
	// Nil renders an empty table rather than failing — a daemon with no tools:
	// block is the normal state, not an error.
	Tools *toolchain.Registry
	// Builds runs tool builds as background jobs with one slot. Separate from
	// Tools because a survey is a question and a build is an action with a
	// lifetime — and only the latter needs somewhere to keep a log.
	Builds *toolchain.Builder
	Sched  *sched.Scheduler // live admission load (P8-beyond)
	// PublicBase is how an attaching machine reaches this daemon, used to build
	// the copy-pasteable install command.
	PublicBase string
	// ConfigPath is the config file corrallm owns and may rewrite. Empty means
	// this daemon has no writable config and enrollment is unavailable.
	ConfigPath string
	// Reload re-reads the config after a write. Nil skips it (the change
	// applies on the next restart).
	Reload func() error
	// AgentDist serves the agent binaries; consulted for the version an agent
	// would actually install. Nil when no agents are distributed.
	AgentDist *agentdist.Handler
	// Liveness tracks which agent-backed servers have reported in. Nil is valid
	// (single-host deployments never see a heartbeat).
	Liveness *agent.Liveness
	// Verified holds OBSERVED capability verdicts published by llm-bench.
	// Nil is valid (no bench has run); every reader must tolerate it.
	Verified *VerifiedStore
	// Proxy is needed to drive the exclusive calibration lease. Nil disables
	// calibration mode rather than panicking.
	Proxy *proxy.Proxy
	// Bench spawns llm-bench for UI-driven runs. Nil disables the endpoints.
	Bench *BenchRunner
	// BenchBin/BenchConfig/BenchProbeDirs locate the llm-bench binary and its
	// inputs. The binary is the same one a human runs from a shell.
	//
	// BenchProbeDirs is a LIST because a probe belongs to whatever it measures:
	// a tool keeps its probes in its own tree and this references the directory,
	// so editing them there changes what this box runs, with nothing to copy.
	// Empty = llm-bench's built-in library.
	BenchBin       string
	BenchConfig    string
	BenchProbeDirs []string
}

// --- health ---

// HealthInput has no parameters.
type HealthInput struct{}

// HealthOutput reports liveness and the build version.
type HealthOutput struct {
	Body struct {
		Status  string `json:"status" doc:"Always \"ok\" when the process is serving."`
		Version string `json:"version" doc:"Build version stamp."`
	}
}

// Health is an unauthenticated liveness probe.
func (h *Handlers) Health(_ context.Context, _ *HealthInput) (*HealthOutput, error) {
	out := &HealthOutput{}
	out.Body.Status = "ok"
	out.Body.Version = h.Version
	return out, nil
}

// --- config summary ---

// ConfigSummaryInput has no parameters.
type ConfigSummaryInput struct{}

// ConfigSummaryOutput reports the loaded config's shape — enough for the P0 UI
// to prove the gat→GraphQL→typed-client loop end to end without leaking secrets.
type ConfigSummaryOutput struct {
	Body struct {
		Servers        []string `json:"servers" doc:"Declared server names."`
		Models         []string `json:"models" doc:"Served model names."`
		PriorityGroups []string `json:"priorityGroups" doc:"Configured priority group names."`
	}
}

// ConfigSummary returns the names declared in the loaded config.
func (h *Handlers) ConfigSummary(_ context.Context, _ *ConfigSummaryInput) (*ConfigSummaryOutput, error) {
	out := &ConfigSummaryOutput{}
	out.Body.Servers = keys(h.config().Servers)
	out.Body.Models = keys(h.config().Models)
	out.Body.PriorityGroups = keys(h.config().PriorityGroups)
	return out, nil
}

// --- recent activity (P8) ---

// RecentActivityInput bounds how many records to return, optionally scoped to
// one served model (the per-model console usage tab).
type RecentActivityInput struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500" doc:"Max records, newest first."`
	Served string `query:"served" doc:"Filter to one served model; empty returns all models."`
	Key    string `query:"key" doc:"Filter to one caller key; empty returns all callers."`
	// Placement narrows to ONE way of serving the model. With a model on two
	// boxes, a figure averaged across both describes neither of them.
	Placement string `query:"placement" doc:"Filter to one placement (box + cmd); empty returns every placement."`
}

// ActivityRecord is one proxied-request row surfaced to the UI. Mirrors
// store.Activity with the P6 metering fields (dwell/tokens/$) exposed.
type ActivityRecord struct {
	ID               int64   `json:"id" doc:"Activity row id (for the detail modal)."`
	TS               int64   `json:"ts" doc:"Unix millis when the request was logged."`
	Served           string  `json:"served" doc:"Served model name."`
	Placement        string  `json:"placement" doc:"WHICH placement served it (box + cmd). Empty on rows written before this was tracked, and on pure proxies corrallm does not place."`
	Key              string  `json:"key" doc:"Caller identity."`
	SourceIP         string  `json:"sourceIp" doc:"Client IP (via X-Forwarded-For); empty if unknown."`
	Path             string  `json:"path" doc:"Request path."`
	Status           int     `json:"status" doc:"HTTP status."`
	DwellMS          int64   `json:"dwellMs" doc:"Time in request, milliseconds."`
	PromptTokens     int     `json:"promptTokens" doc:"Metered prompt tokens."`
	CompletionTokens int     `json:"completionTokens" doc:"Metered completion tokens."`
	CachedTokens     int     `json:"cachedTokens" doc:"Backend-reported cached prompt tokens (0 if none)."`
	PromptPerSec     float64 `json:"promptPerSec" doc:"Backend-reported prompt-processing speed, tokens/sec (tp/s); 0 if unavailable."`
	PredictedPerSec  float64 `json:"predictedPerSec" doc:"Backend-reported generation speed, tokens/sec (tg/s); 0 if unavailable."`
	CostUSD          float64 `json:"costUsd" doc:"Resolved request cost in USD."`
	AudioBytes       int64   `json:"audioBytes" doc:"Metered audio request bytes (STT/TTS); 0 for text."`
	Error            string  `json:"error" doc:"Proxy/backpressure failure reason, if any (empty on success)."`
	TTFBMs           int64   `json:"ttfbMs" doc:"Time to first response byte, milliseconds (0 if no body)."`
	FinishReason     string  `json:"finishReason" doc:"Why the model stopped: stop (it chose to) | length (it hit a cap and did NOT finish) | tool_calls | content_filter. Empty if the backend reported none or the reply exceeded the capture cap."`
	QueuedMS         int64   `json:"queuedMs" doc:"Time queued before admission/reject, milliseconds."`
	RetryAfterMS     int64   `json:"retryAfterMs" doc:"The Retry-After we promised this caller on a 429, milliseconds; 0 when no promise was made. ts + retryAfterMs is when we told them to come back."`
}

// RecentActivityOutput is the newest-first activity list.
type RecentActivityOutput struct {
	Body struct {
		Records []ActivityRecord `json:"records" doc:"Activity rows, newest first."`
	}
}

// RecentActivity returns the most recent proxied-request records.
func (h *Handlers) RecentActivity(_ context.Context, in *RecentActivityInput) (*RecentActivityOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := h.Store.RecentActivity(limit, in.Served, in.Key, in.Placement)
	if err != nil {
		return nil, err
	}
	out := &RecentActivityOutput{}
	out.Body.Records = make([]ActivityRecord, 0, len(rows))
	for _, a := range rows {
		out.Body.Records = append(out.Body.Records, ActivityRecord{
			ID:               a.ID,
			TS:               a.TS,
			Served:           a.Served,
			Placement:        a.Placement,
			Key:              a.Key,
			SourceIP:         a.SourceIP,
			Path:             a.Path,
			Status:           a.Status,
			DwellMS:          a.DwellMS,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			CachedTokens:     a.CachedTokens,
			PromptPerSec:     a.PromptPerSec,
			PredictedPerSec:  a.PredictedPerSec,
			CostUSD:          a.CostUSD,
			AudioBytes:       a.AudioBytes,
			Error:            a.Error,
			TTFBMs:           a.TTFBMs,
			FinishReason:     a.FinishReason,
			QueuedMS:         a.QueuedMS,
			RetryAfterMS:     a.RetryAfterMS,
		})
	}
	return out, nil
}

// ActivityDetailInput selects one activity row by id.
type ActivityDetailInput struct {
	ID int64 `query:"id" doc:"Activity row id (from recentActivity)."`
}

// ActivityDetailRecord is one activity row with the captured payloads (P10c) —
// the detail modal. Fetched on demand so the list query stays lean.
type ActivityDetailRecord struct {
	ActivityRecord
	ReqBody  string `json:"reqBody" doc:"Captured request payload (capped; binary audio summarized)."`
	RespBody string `json:"respBody" doc:"Captured response payload (capped; binary audio summarized)."`
}

// ActivityDetailOutput wraps one detail record.
type ActivityDetailOutput struct {
	Body struct {
		Record ActivityDetailRecord `json:"record"`
	}
}

// ActivityDetail returns one activity row including its captured payloads.
func (h *Handlers) ActivityDetail(_ context.Context, in *ActivityDetailInput) (*ActivityDetailOutput, error) {
	a, err := h.Store.ActivityByID(in.ID)
	if err != nil {
		return nil, huma.Error404NotFound("no such activity row")
	}
	out := &ActivityDetailOutput{}
	out.Body.Record = ActivityDetailRecord{
		ActivityRecord: ActivityRecord{
			ID: a.ID, TS: a.TS, Served: a.Served, Placement: a.Placement,
			Key: a.Key, SourceIP: a.SourceIP, Path: a.Path,
			Status: a.Status, DwellMS: a.DwellMS, PromptTokens: a.PromptTokens,
			CompletionTokens: a.CompletionTokens, CachedTokens: a.CachedTokens,
			PromptPerSec: a.PromptPerSec, PredictedPerSec: a.PredictedPerSec,
			CostUSD: a.CostUSD, AudioBytes: a.AudioBytes,
			Error: a.Error, TTFBMs: a.TTFBMs, FinishReason: a.FinishReason, QueuedMS: a.QueuedMS,
			RetryAfterMS: a.RetryAfterMS,
		},
		ReqBody:  a.ReqBody,
		RespBody: a.RespBody,
	}
	return out, nil
}

// --- retry promises (P15) ---

// RetryPromisesInput bounds the window, the row count, and optionally the caller.
type RetryPromisesInput struct {
	Limit   int    `query:"limit" default:"50" minimum:"1" maximum:"500" doc:"Max promises, newest first."`
	Minutes int    `query:"minutes" default:"60" minimum:"1" maximum:"10080" doc:"Look back this many minutes."`
	Key     string `query:"key" doc:"Filter to one caller key; empty returns all callers."`
}

// RetryPromiseRecord is one "come back later" we handed out, with the verdict on
// what the caller did about it.
type RetryPromiseRecord struct {
	ID           int64  `json:"id" doc:"Activity row the promise was made on."`
	TS           int64  `json:"ts" doc:"Unix millis we made the promise."`
	Key          string `json:"key" doc:"Caller identity; empty for an unkeyed caller."`
	SourceIP     string `json:"sourceIp" doc:"Client IP."`
	Served       string `json:"served" doc:"Model they were asking for."`
	Reason       string `json:"reason" doc:"Why we turned them away: rejected | queue-timeout | exhausted | bumped."`
	RetryAfterMS int64  `json:"retryAfterMs" doc:"What we promised, milliseconds."`
	DueMS        int64  `json:"dueMs" doc:"Unix millis we told them to come back (ts + retryAfterMs)."`
	ReturnedMS   int64  `json:"returnedMs" doc:"Unix millis of the caller's next request; 0 if they have not been back."`
	WaitedMS     int64  `json:"waitedMs" doc:"How long they actually waited before returning; 0 if they have not been back. Compare against retryAfterMs to judge the estimate."`
	State        string `json:"state" doc:"waiting (due in the future, not back yet) | honored (returned at or after the promise) | early (returned before it) | gone (due passed, never returned)."`
}

// RetryPromisesOutput is the newest-first promise list.
type RetryPromisesOutput struct {
	Body struct {
		Promises []RetryPromiseRecord `json:"promises" doc:"Promises, newest first."`
		Waiting  int                  `json:"waiting" doc:"How many are still owed a slot — due in the future and not back yet."`
	}
}

// classifyPromise is the ONE definition of what became of a "come back later".
// Shared by the promises list and the utilization row so the two can never
// report different counts of the same hour.
func classifyPromise(now, dueMS, returnedMS int64) string {
	switch {
	case returnedMS == 0 && now < dueMS:
		return "waiting" // due in the future, still owed a slot
	case returnedMS == 0:
		return "gone" // due passed, never came back
	case returnedMS < dueMS:
		return "early" // came back before we said to
	default:
		return "honored"
	}
}

// RetryPromises lists the backoffs we handed out and what became of them.
//
// The list is the answer to "who did we tell to come back, and when" — a
// question the activity log could not answer before the promise was recorded,
// because a 429 row said only that a caller was refused. The state verdict is
// the second half: a run of `early` means callers are ignoring Retry-After, and
// a run of `gone` means we drove them off. Both are invisible from the 429 count
// alone.
func (h *Handlers) RetryPromises(_ context.Context, in *RetryPromisesInput) (*RetryPromisesOutput, error) {
	limit, minutes := in.Limit, in.Minutes
	if limit <= 0 {
		limit = 50
	}
	if minutes <= 0 {
		minutes = 60
	}
	now := time.Now().UnixMilli()
	rows, err := h.Store.RetryPromises(now-int64(minutes)*60_000, limit, in.Key)
	if err != nil {
		return nil, err
	}
	out := &RetryPromisesOutput{}
	out.Body.Promises = make([]RetryPromiseRecord, 0, len(rows))
	for _, p := range rows {
		due := p.TS + p.RetryAfterMS
		rec := RetryPromiseRecord{
			ID: p.ID, TS: p.TS, Key: p.Key, SourceIP: p.SourceIP, Served: p.Served,
			Reason: p.Reason, RetryAfterMS: p.RetryAfterMS, DueMS: due, ReturnedMS: p.ReturnedMS,
		}
		rec.State = classifyPromise(now, due, p.ReturnedMS)
		if rec.State == "waiting" {
			out.Body.Waiting++
		}
		if p.ReturnedMS > 0 {
			rec.WaitedMS = p.ReturnedMS - p.TS
		}
		out.Body.Promises = append(out.Body.Promises, rec)
	}
	return out, nil
}

// --- utilization (P15a) ---

// UtilizationInput bounds the window the windowed columns are measured over.
type UtilizationInput struct {
	Minutes int `query:"minutes" default:"60" minimum:"1" maximum:"10080" doc:"Window for the promise and queue-wait columns."`
}

// UtilizationRow is one served model's pressure: what it is doing right now, and
// what arriving at it has cost over the window.
//
// Deliberately per-model rather than one box-wide number. Capacity is not
// fungible — a saturated 27B and an idle embedder summed into "1/6 in use" says
// nothing true about either, and hides the only fact that matters (which thing
// is full).
type UtilizationRow struct {
	Served string `json:"served" doc:"Served model or lane name."`
	// Live, instantaneous — from the scheduler, summed over the backends this
	// served name resolves to.
	Capacity int `json:"capacity" doc:"Admission slots across this name's backends; 0 if none has been touched since start."`
	Active   int `json:"active" doc:"Slots in use right now."`
	Waiting  int `json:"waiting" doc:"Callers queued right now."`
	// Windowed — from the activity log.
	Promised   int `json:"promised" doc:"Promises made in the window that are still outstanding (due in the future, not back yet)."`
	NotHonored int `json:"notHonored" doc:"Promises whose due time passed with no return — we told them to come back and they never did."`
	Early      int `json:"early" doc:"Callers who came back BEFORE the time we gave them (ignoring Retry-After)."`
	Turned     int `json:"turnedAway" doc:"Total promises made in the window, whatever became of them."`
	// The two halves of "time to the back of the queue".
	EstWaitMS     int64 `json:"estWaitMs" doc:"What we would tell a caller arriving right now to wait — the scheduler's own estimate, live. 0 if no dwell sample yet."`
	RealWaitMS    int64 `json:"realWaitMs" doc:"Measured mean wait of requests that actually queued and were then admitted, over the window. 0 if nothing queued."`
	MaxWaitMS     int64 `json:"maxWaitMs" doc:"Longest measured queue wait in the window."`
	QueuedSamples int64 `json:"queuedSamples" doc:"How many requests queued at all — realWaitMs over 1 or 2 samples is not a trend."`

	// Service-time shape, and what queueing theory makes of it.
	ServiceMeanMS  int64   `json:"serviceMeanMs" doc:"Mean time a request actually occupied a slot (queue and cold-load excluded)."`
	ServiceCV      float64 `json:"serviceCv" doc:"Coefficient of variation of service time. Above 1 means the mean is tail-dominated and position×mean badly under-estimates a queue."`
	ServiceSamples int64   `json:"serviceSamples" doc:"Requests behind the service-time stats."`
	Rho            float64 `json:"rho" doc:"Utilization over the window: total service time / (capacity × window). 0 if capacity is unknown."`
	PKWaitMS       int64   `json:"pkWaitMs" doc:"Queueing-theory expected wait, ρ/(1−ρ)·E[S]·(1+CV²)/2 — what the measured distribution implies, versus what the scheduler estimates. 0 when not computable; see pkSaturated."`
	PKSaturated    bool    `json:"pkSaturated" doc:"Utilization reached or passed 1 over the window: arrivals outpaced service and no finite steady-state wait exists."`

	// Whether the configured queue bound can ever actually bind here.
	ConfiguredDepth  int  `json:"configuredDepth" doc:"scheduler.maxQueueDepth as configured (0 = unbounded)."`
	ReachableDepth   int  `json:"reachableDepth" doc:"How deep the queue can get before maxWait times the front waiter out: capacity × maxWait / meanService. 0 if not computable."`
	DepthUnreachable bool `json:"depthUnreachable" doc:"Configured depth exceeds what maxWait allows — the depth bound is dead config, callers always time out first."`
}

// UtilizationOutput is the per-model pressure table.
type UtilizationOutput struct {
	Body struct {
		Rows    []UtilizationRow `json:"rows" doc:"One row per served model seen in the window (or busy right now), busiest first."`
		Minutes int              `json:"minutes" doc:"The window the windowed columns cover."`
	}
}

// Utilization reports per-model pressure: slots in use, queue depth, what we
// promised callers we turned away, and both halves of the wait — what we
// ESTIMATE a new arrival faces versus what arrivals measurably faced.
//
// The estimate/measured pair is the point. They are produced by different
// machinery (a live EWMA projection vs. recorded queue time) and a persistent
// gap between them means the number we hand callers does not describe the box
// they are calling.
func (h *Handlers) Utilization(_ context.Context, in *UtilizationInput) (*UtilizationOutput, error) {
	minutes := in.Minutes
	if minutes <= 0 {
		minutes = 60
	}
	now := time.Now().UnixMilli()
	since := now - int64(minutes)*60_000

	rows := map[string]*UtilizationRow{}
	row := func(served string) *UtilizationRow {
		r := rows[served]
		if r == nil {
			r = &UtilizationRow{Served: served}
			rows[served] = r
		}
		return r
	}

	// Row set: everything asked for in the window. A model nobody called is not
	// "0% utilized", it is absent.
	seen, err := h.Store.ModelsSeenSince(since)
	if err != nil {
		return nil, err
	}
	for _, s := range seen {
		row(s)
	}

	// Live load, keyed by BACKEND, folded onto the served names that resolve to
	// it. A lane's members each contribute their own slots.
	byBackend := map[string]sched.BackendLoad{}
	for _, b := range h.Sched.Snapshot().Backends {
		byBackend[b.Backend] = b
	}
	attach := func(served string) {
		cands, ok := h.config().ResolveServed(served)
		if !ok {
			return
		}
		r := row(served)
		for _, c := range cands {
			b, ok := byBackend[c.Name]
			if !ok {
				continue // never admitted here since start; no live state to report
			}
			r.Capacity += b.Capacity
			r.Active += b.Active
			r.Waiting += b.Waiting
			// Min across candidates, matching how a spill walk reports backpressure
			// (keepSoonest): the earliest moment ANY path could take the caller.
			ms := b.EstWait.Milliseconds()
			if ms > 0 && (r.EstWaitMS == 0 || ms < r.EstWaitMS) {
				r.EstWaitMS = ms
			}
		}
	}
	for s := range rows {
		attach(s)
	}
	// A model busy RIGHT NOW but with no completed request in the window has no
	// activity row yet — it would vanish from a log-only row set precisely when
	// it is under load. Pull it in from the live snapshot.
	for name, b := range byBackend {
		if b.Active == 0 && b.Waiting == 0 {
			continue
		}
		if _, ok := rows[name]; !ok {
			row(name)
			attach(name)
		}
	}

	// Measured queue wait.
	waits, err := h.Store.QueueWaitByModel(since)
	if err != nil {
		return nil, err
	}
	for _, w := range waits {
		if r, ok := rows[w.Served]; ok {
			r.RealWaitMS, r.MaxWaitMS, r.QueuedSamples = w.MeanMS, w.MaxMS, w.Samples
		}
	}

	// Service-time shape, and what a queueing model makes of it. This is the
	// theoretical third opinion beside est (what the scheduler predicts) and real
	// (what callers measured) — produced from the distribution rather than from
	// either mechanism, so agreement between any two of them means something.
	svc, err := h.Store.ServiceStats(since, false)
	if err != nil {
		return nil, err
	}
	windowMS := float64(minutes) * 60_000
	maxWait, _ := time.ParseDuration(h.config().Scheduler.MaxWait)
	depth := h.config().Scheduler.MaxQueueDepth
	for _, s := range svc {
		r, ok := rows[s.Served]
		if !ok {
			continue
		}
		cv := s.CV()
		r.ServiceMeanMS, r.ServiceCV, r.ServiceSamples = int64(s.MeanMS), cv, s.N
		r.ConfiguredDepth = depth
		if r.Capacity > 0 {
			// Utilization measured over the window rather than from an instantaneous
			// active/capacity read: a snapshot of a bursty hour is mostly noise, and
			// ρ is the term the wait is most sensitive to near saturation.
			r.Rho = float64(s.TotalMS) / (float64(r.Capacity) * windowMS)
			if r.Rho >= 1 {
				r.PKSaturated = true
			} else if r.Rho > 0 {
				// Pollaczek–Khinchine. Assumes steady state, which a bursty hour is
				// not — this is an order-of-magnitude read, not a promise.
				r.PKWaitMS = int64(r.Rho / (1 - r.Rho) * s.MeanMS * (1 + cv*cv) / 2)
			}
			if maxWait > 0 && s.MeanMS > 0 {
				r.ReachableDepth = int(float64(r.Capacity) * float64(maxWait.Milliseconds()) / s.MeanMS)
				// Dead config: the depth bound can never bind because maxWait always
				// evicts the front of the queue first. Every rejection will be a
				// timeout, and the configured number is describing a queue that
				// cannot exist.
				r.DepthUnreachable = depth > 0 && depth > r.ReachableDepth
			}
		}
	}

	// Promise outcomes, classified by the same function the promises panel uses.
	promises, err := h.Store.RetryPromises(since, 2000, "")
	if err != nil {
		return nil, err
	}
	for _, p := range promises {
		r, ok := rows[p.Served]
		if !ok {
			r = row(p.Served)
		}
		r.Turned++
		switch classifyPromise(now, p.TS+p.RetryAfterMS, p.ReturnedMS) {
		case "waiting":
			r.Promised++
		case "gone":
			r.NotHonored++
		case "early":
			r.Early++
		}
	}

	out := &UtilizationOutput{}
	out.Body.Minutes = minutes
	out.Body.Rows = make([]UtilizationRow, 0, len(rows))
	for _, r := range rows {
		out.Body.Rows = append(out.Body.Rows, *r)
	}
	// Busiest first: queued callers, then slots in use, then how many were turned
	// away — the order an operator scans for "what is under pressure".
	sort.Slice(out.Body.Rows, func(i, j int) bool {
		a, b := out.Body.Rows[i], out.Body.Rows[j]
		if a.Waiting != b.Waiting {
			return a.Waiting > b.Waiting
		}
		if a.Active != b.Active {
			return a.Active > b.Active
		}
		if a.Turned != b.Turned {
			return a.Turned > b.Turned
		}
		return a.Served < b.Served
	})
	return out, nil
}

// --- caller service profiles (P15a) ---

// shrinkPseudoCount is the weight given to the model-level prior when blending a
// caller's own statistics with it.
//
// A caller with 20 requests does not have a reliable variance — one 400-second
// outlier can triple its CV — and letting that number stand alone means a fluke
// sets policy. Blending toward the model's aggregate with a pseudo-count of 30
// makes a small sample say roughly "mostly the model, nudged by what I've seen",
// converging to the caller's own numbers as evidence accumulates. Both the raw
// and blended figures are reported: the adjustment must be auditable, not
// silently applied.
const shrinkPseudoCount = 30.0

// shrink blends a caller's statistic toward the model-level prior by sample count.
func shrink(sample float64, n int64, prior float64) float64 {
	return (float64(n)*sample + shrinkPseudoCount*prior) / (float64(n) + shrinkPseudoCount)
}

// ServiceProfilesInput bounds the window.
type ServiceProfilesInput struct {
	Minutes int `query:"minutes" default:"1440" minimum:"1" maximum:"43200" doc:"Window to profile over. Wider than the utilization window by default — a distribution needs samples."`
}

// ServiceProfileRow is one caller's service-time profile on one model, next to
// that model's overall profile.
type ServiceProfileRow struct {
	Served  string  `json:"served" doc:"Served model name."`
	Key     string  `json:"key" doc:"Caller identity; empty for unkeyed callers."`
	N       int64   `json:"n" doc:"Requests behind these numbers."`
	MeanMS  int64   `json:"meanMs" doc:"This caller's mean service time on this model."`
	StdMS   int64   `json:"stdMs" doc:"Standard deviation of service time."`
	MaxMS   int64   `json:"maxMs" doc:"Longest single request."`
	CV      float64 `json:"cv" doc:"Coefficient of variation, stddev/mean."`
	ShareMS int64   `json:"shareMs" doc:"Total slot time this caller consumed — who the model is actually working for."`
	// Blended toward the model prior by sample count.
	ShrunkMeanMS int64   `json:"shrunkMeanMs" doc:"Mean blended toward the model-level prior by sample count — what an estimator should use instead of a small sample."`
	ShrunkCV     float64 `json:"shrunkCv" doc:"CV blended toward the model-level prior."`
	// The prior itself, so the blend is auditable.
	ModelMeanMS int64   `json:"modelMeanMs" doc:"The model's overall mean service time (the prior)."`
	ModelCV     float64 `json:"modelCv" doc:"The model's overall CV (the prior)."`
	// VarianceFactor is the (1+CV²)/2 term from Pollaczek–Khinchine: the multiple
	// by which this caller's variability alone inflates the wait of anyone queued
	// behind them, over what a mean-only estimate predicts.
	VarianceFactor float64 `json:"varianceFactor" doc:"(1+CV²)/2 — how much this caller's variability alone inflates the wait of whoever is behind them, versus a mean-only estimate. 1.0 means constant-time requests."`
}

// ServiceProfilesOutput is the per-caller profile table.
type ServiceProfilesOutput struct {
	Body struct {
		Rows    []ServiceProfileRow `json:"rows" doc:"One row per (model, caller), heaviest consumer first."`
		Minutes int                 `json:"minutes" doc:"The window profiled."`
	}
}

// ServiceProfiles reports how long each caller's requests actually occupy a slot,
// and how variable that is.
//
// The scheduler carries ONE dwell EWMA per backend, so every caller of a model is
// predicted by the same scalar. This shows what that scalar averages over: on one
// model, callers can differ several-fold in mean and several-fold again in
// variability, which is precisely the information a position×mean estimate throws
// away.
//
// Read-only. Nothing here feeds admission or the Retry-After a caller receives.
func (h *Handlers) ServiceProfiles(_ context.Context, in *ServiceProfilesInput) (*ServiceProfilesOutput, error) {
	minutes := in.Minutes
	if minutes <= 0 {
		minutes = 1440
	}
	since := time.Now().UnixMilli() - int64(minutes)*60_000

	priors, err := h.Store.ServiceStats(since, false)
	if err != nil {
		return nil, err
	}
	prior := map[string]store.ServiceStat{}
	for _, p := range priors {
		prior[p.Served] = p
	}
	perKey, err := h.Store.ServiceStats(since, true)
	if err != nil {
		return nil, err
	}

	out := &ServiceProfilesOutput{}
	out.Body.Minutes = minutes
	out.Body.Rows = make([]ServiceProfileRow, 0, len(perKey))
	for _, s := range perKey {
		p := prior[s.Served]
		cv := s.CV()
		row := ServiceProfileRow{
			Served: s.Served, Key: s.Key, N: s.N,
			MeanMS: int64(s.MeanMS), StdMS: int64(s.StdMS), MaxMS: s.MaxMS, CV: cv,
			ShareMS:      s.TotalMS,
			ShrunkMeanMS: int64(shrink(s.MeanMS, s.N, p.MeanMS)),
			ShrunkCV:     shrink(cv, s.N, p.CV()),
			ModelMeanMS:  int64(p.MeanMS), ModelCV: p.CV(),
			VarianceFactor: (1 + cv*cv) / 2,
		}
		out.Body.Rows = append(out.Body.Rows, row)
	}
	// Heaviest consumer first: whose work the box actually spent its time on.
	sort.Slice(out.Body.Rows, func(i, j int) bool {
		return out.Body.Rows[i].ShareMS > out.Body.Rows[j].ShareMS
	})
	return out, nil
}

// --- residency (P8) ---

// ResidencyInput has no parameters.
type ResidencyInput struct{}

// PoolView is one memory pool's budget/usage.
type PoolView struct {
	Pool   string `json:"pool" doc:"Pool name (gpu0, system, …)."`
	Budget int64  `json:"budget" doc:"Bytes available to spawned backends (total − reserve)."`
	Used   int64  `json:"used" doc:"Bytes currently reserved by resident backends."`
}

// ServerView is a server's per-pool residency.
type ServerView struct {
	Server string     `json:"server" doc:"Server name."`
	Pools  []PoolView `json:"pools" doc:"Per-pool budget/usage."`
}

// PoolUsageView is a resident backend's reservation against one pool.
type PoolUsageView struct {
	Pool  string `json:"pool" doc:"Pool name."`
	Bytes int64  `json:"bytes" doc:"Reserved bytes."`
}

// ResidentModelView is one loaded/loading backend.
type ResidentModelView struct {
	Name       string          `json:"name" doc:"Backend id (<servedModel>#<index>)."`
	ModelName  string          `json:"modelName" doc:"Served model name."`
	ProcKey    string          `json:"procKey" doc:"Backing process identity (extension:<name> when an extension hosts it, else the served name). Resolve a model's state through this — an extension's models share one process."`
	Remote     bool            `json:"remote" doc:"Served by a host corrallm does not run: no local process, non-loopback target. Holds no residency — never count it as loaded."`
	Server     string          `json:"server" doc:"Server, empty for pure-proxy."`
	State      string          `json:"state" doc:"absent|loading|ready|failed|evicting. Not a residency fact when remote."`
	Refs       int             `json:"refs" doc:"In-flight requests holding it."`
	Persistent bool            `json:"persistent" doc:"Pinned: exempt from eviction."`
	LastUsedMS int64           `json:"lastUsedMs" doc:"Unix millis of last use, 0 if never."`
	NCtx       int             `json:"nCtx" doc:"Context length parsed from the backend (0 if unknown)."`
	NSlots     int             `json:"nSlots" doc:"Slot count parsed from the backend (0 if unknown)."`
	HasUI      string          `json:"hasUi" doc:"unknown|yes|no — does the backend serve a web UI at / (P11b)."`
	Usage      []PoolUsageView `json:"usage" doc:"Per-pool reservation."`

	FootprintMiB  int `json:"footprintMiB" doc:"Live VRAM (MiB) of the resident process group; 0 if unavailable or not spawned."`
	BaseMiB       int `json:"baseMiB" doc:"Tune-cache: process footprint with KV excluded (weights + fixed overhead); 0 if unmeasured."`
	PerSlotMiB    int `json:"perSlotMiB" doc:"Tune-cache: measured KV cache cost per slot; 0 if unmeasured."`
	PeakMiB       int `json:"peakMiB" doc:"Tune-cache: highest total process footprint ever observed; 0 if unmeasured."`
	MeasuredSlots int `json:"measuredSlots" doc:"Tune-cache: n_slots the measurement was taken at; 0 if unmeasured."`
	TunedSlots    int `json:"tunedSlots" doc:"Slot count the auto-tuner applied at last spawn, or configSlots if untuned/not resident."`
	ConfigSlots   int `json:"configSlots" doc:"Model's configured maxConcurrent (default 1)."`
}

// DeviceMemView is MEASURED memory on a real device, as opposed to the
// scheduler's accounted pool ledger. The two answer different questions —
// "what has corrallm promised" vs "what is the box actually holding" — and a
// divergence between them (another process on the GPU, a backend that outgrew
// its declared ramUsage) is itself the signal worth seeing.
//
// Available=false means the probe FAILED and every number here is meaningless;
// readers must render nothing rather than a zeroed bar, which would read as an
// empty machine.
type DeviceMemView struct {
	Available  bool   `json:"available" doc:"False when the probe failed — ignore the other fields."`
	Name       string `json:"name" doc:"Device name (GPU model, or \"host\" for system RAM)."`
	TotalBytes int64  `json:"totalBytes" doc:"Physical total."`
	UsedBytes  int64  `json:"usedBytes" doc:"In use right now."`
	FreeBytes  int64  `json:"freeBytes" doc:"Free right now."`
	// Pool is the budget this card backs, from the server's `devices:` map, or
	// "" when nothing claims it. Without it a multi-GPU reading cannot be
	// rendered against the right ledger — which is how a 10 GiB card came to be
	// drawn under a pool budgeted for 32 GiB.
	Pool string `json:"pool,omitempty" doc:"Pool this device backs, or empty when no pool declares it."`
	// UUID is the card's stable identity, shown so an operator can copy it into
	// a `devices:` entry without going to look it up.
	UUID string `json:"uuid,omitempty" doc:"Vendor device UUID — the identity a pool binds to."`
}

// ResidencyOutput reports server pool budgets and resident backends.
type ResidencyOutput struct {
	Body struct {
		Servers []ServerView        `json:"servers" doc:"Per-server pool budget/usage (the scheduler's accounted ledger)."`
		Models  []ResidentModelView `json:"models" doc:"Currently resident backends."`
		// GPUs is EVERY card this machine can see, each tagged with the pool it
		// backs. A list rather than one reading because "the GPU" stopped being
		// a well-defined phrase the moment this box had two — and the field it
		// replaces reported nvidia-smi's index 0, a position that silently
		// moved onto the newly installed card.
		GPUs []DeviceMemView `json:"gpus" doc:"Measured VRAM per GPU on the local machine; empty if unprobeable."`
		Host DeviceMemView   `json:"host" doc:"Measured host RAM; available=false if unprobeable."`
		// Stopping holds process keys mid-teardown. They are NOT in Models —
		// their pools are already freed, so they hold no residency — but they
		// are not loadable either: an explicit load is refused until the
		// process is gone. Join on procKey to render it.
		Stopping []string `json:"stopping" doc:"Process keys whose process is still stopping; a load aimed at one is refused until it exits."`
	}
}

// Residency returns the live residency snapshot (pool budgets + what's warm).
func (h *Handlers) Residency(_ context.Context, _ *ResidencyInput) (*ResidencyOutput, error) {
	snap := h.Mgr.Snapshot()
	out := &ResidencyOutput{}
	out.Body.Stopping = snap.Stopping
	out.Body.Servers = make([]ServerView, 0, len(snap.Servers))
	for _, s := range snap.Servers {
		sv := ServerView{Server: s.Server, Pools: make([]PoolView, 0, len(s.Pools))}
		for _, p := range s.Pools {
			sv.Pools = append(sv.Pools, PoolView{Pool: p.Pool, Budget: p.Budget, Used: p.Used})
		}
		out.Body.Servers = append(out.Body.Servers, sv)
	}
	// Measured device state for every card on this machine. Best-effort: a box
	// with no nvidia-smi, or a non-Linux host, reports an empty list and the
	// dashboard omits those bars.
	//
	// Each reading is tagged with the pool it backs, resolved through the
	// server's `devices:` selectors. That tag is what lets the UI draw a
	// measured bar under the RIGHT ledger — the field this replaced reported
	// nvidia-smi's index 0 with no way to tell which card that was, and after a
	// second GPU was installed it was no longer the card the pool budgeted.
	if devs, err := gpu.ProbeAll(); err == nil {
		const mib = 1024 * 1024
		// The device probe reads THIS machine, so only a local server's pools
		// can claim one. Remote servers report their own capacity by heartbeat.
		poolOf := h.localDevicePools(devs)
		for _, d := range devs {
			out.Body.GPUs = append(out.Body.GPUs, DeviceMemView{
				Available:  true,
				Name:       d.Name,
				UUID:       d.UUID,
				Pool:       poolOf[d.UUID],
				TotalBytes: int64(d.TotalMiB) * mib,
				UsedBytes:  int64(d.UsedMiB) * mib,
				FreeBytes:  int64(d.FreeMiB) * mib,
			})
		}
	}
	if hm, err := sysmem.Probe(); err == nil {
		out.Body.Host = DeviceMemView{
			Available:  true,
			Name:       "host",
			TotalBytes: hm.TotalBytes,
			UsedBytes:  hm.UsedBytes,
			FreeBytes:  hm.AvailableBytes,
		}
	}
	out.Body.Models = make([]ResidentModelView, 0, len(snap.Models))
	for _, m := range snap.Models {
		configSlots := h.config().Models[m.ModelName].Slots()
		mv := ResidentModelView{
			Name: m.Name, ModelName: m.ModelName, ProcKey: m.ProcKey, Remote: m.Remote,
			Server: m.Server, State: m.State,
			Refs: m.Refs, Persistent: m.Persistent, LastUsedMS: m.LastUsedMS,
			NCtx: m.NCtx, NSlots: m.NSlots, HasUI: m.HasUI,
			Usage: make([]PoolUsageView, 0, len(m.Usage)),

			FootprintMiB: h.Mgr.ModelVRAM(m.ModelName),
			TunedSlots:   h.Mgr.TunedSlots(m.ModelName, configSlots),
			ConfigSlots:  configSlots,
		}
		// Resolved per model rather than from one snapshot-wide probe: on a
		// two-card box "the first GPU" is not the card this model ran on, and a
		// profile filed under the other one would simply never be found.
		if p, ok := h.Mgr.TuneProfileForModel(m.ModelName); ok {
			mv.BaseMiB, mv.PerSlotMiB, mv.PeakMiB, mv.MeasuredSlots = p.BaseMiB, p.PerSlotMiB, p.PeakMiB, p.MeasuredSlots
		}
		for _, u := range m.Usage {
			mv.Usage = append(mv.Usage, PoolUsageView{Pool: u.Pool, Bytes: u.Bytes})
		}
		out.Body.Models = append(out.Body.Models, mv)
	}
	return out, nil
}

// --- usage rollup (P8) ---

// UsageRollupInput bounds the rollup window.
type UsageRollupInput struct {
	WindowHours int `query:"windowHours" default:"24" minimum:"0" maximum:"8760" doc:"Trailing window in hours; 0 = all time."`
}

// RollupRow is aggregated usage for one served model.
type RollupRow struct {
	Served             string  `json:"served" doc:"Served model name."`
	Requests           int64   `json:"requests" doc:"Request count."`
	PromptTokens       int64   `json:"promptTokens" doc:"Total prompt tokens."`
	CompletionTokens   int64   `json:"completionTokens" doc:"Total completion tokens."`
	DwellMS            int64   `json:"dwellMs" doc:"Total dwell, milliseconds."`
	CostUSD            float64 `json:"costUsd" doc:"Total cost, USD."`
	CachedTokens       int64   `json:"cachedTokens" doc:"Prompt tokens the backend served from cache instead of reprocessing."`
	CacheReports       int64   `json:"cacheReports" doc:"Requests that reported any cache hit. ZERO MEANS UNKNOWN, NOT 0% — a backend with no prompt cache (embeddings, most remote providers) never reports, and showing that as a measured miss rate invents a problem."`
	CacheHitRate       float64 `json:"cacheHitRate" doc:"cachedTokens / promptTokens, 0..1. Only meaningful when cacheReports > 0."`
	CachedSecondsSaved float64 `json:"cachedSecondsSaved" doc:"Estimated prompt-processing time avoided: cachedTokens / mean observed prompt tokens-per-second. An estimate — the speed varies with batch and context."`
}

// UsageRollupOutput is per-model usage plus a grand total over the window.
type UsageRollupOutput struct {
	Body struct {
		WindowHours int         `json:"windowHours" doc:"Window applied (0 = all time)."`
		Rows        []RollupRow `json:"rows" doc:"Per-model usage, costliest first."`
		Total       RollupRow   `json:"total" doc:"Grand total across all models (served=\"\")."`
	}
}

// UsageRollup aggregates metered activity by served model over a window.
func (h *Handlers) UsageRollup(_ context.Context, in *UsageRollupInput) (*UsageRollupOutput, error) {
	var sinceMS int64
	if in.WindowHours > 0 {
		sinceMS = time.Now().Add(-time.Duration(in.WindowHours) * time.Hour).UnixMilli()
	}
	rows, err := h.Store.RollupByModel(sinceMS)
	if err != nil {
		return nil, err
	}
	out := &UsageRollupOutput{}
	out.Body.WindowHours = in.WindowHours
	out.Body.Rows = make([]RollupRow, 0, len(rows))
	for _, r := range rows {
		row := RollupRow{
			Served: r.Served, Requests: r.Requests,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			DwellMS: r.DwellMS, CostUSD: r.CostUSD,
			CachedTokens: r.CachedTokens, CacheReports: r.CacheReports,
		}
		if r.PromptTokens > 0 {
			row.CacheHitRate = float64(r.CachedTokens) / float64(r.PromptTokens)
		}
		if r.PromptPerSec > 0 {
			row.CachedSecondsSaved = float64(r.CachedTokens) / r.PromptPerSec
		}
		out.Body.Rows = append(out.Body.Rows, row)
		out.Body.Total.Requests += row.Requests
		out.Body.Total.PromptTokens += row.PromptTokens
		out.Body.Total.CompletionTokens += row.CompletionTokens
		out.Body.Total.DwellMS += row.DwellMS
		out.Body.Total.CostUSD += row.CostUSD
		out.Body.Total.CachedTokens += row.CachedTokens
		out.Body.Total.CacheReports += row.CacheReports
		out.Body.Total.CachedSecondsSaved += row.CachedSecondsSaved
	}
	// The grand total's rate is recomputed from the totals, not averaged across
	// rows: a model with 12 requests must not weigh as much as one with 14,000.
	if out.Body.Total.PromptTokens > 0 {
		out.Body.Total.CacheHitRate =
			float64(out.Body.Total.CachedTokens) / float64(out.Body.Total.PromptTokens)
	}
	return out, nil
}

// --- per-key usage rollup (P8-beyond) ---

// UsageByKeyInput bounds the rollup window.
type UsageByKeyInput struct {
	WindowHours int `query:"windowHours" default:"24" minimum:"0" maximum:"8760" doc:"Trailing window in hours; 0 = all time."`
}

// KeyUsageRow is aggregated usage for one caller key, including energy derived
// from cost (energy = cost / costPerKwh; meaningful for energy-priced local
// types — exact for an all-local deployment).
type KeyUsageRow struct {
	Key              string  `json:"key" doc:"Caller key (empty = unkeyed)."`
	Requests         int64   `json:"requests" doc:"Request count."`
	PromptTokens     int64   `json:"promptTokens" doc:"Total prompt tokens."`
	CompletionTokens int64   `json:"completionTokens" doc:"Total completion tokens."`
	DwellMS          int64   `json:"dwellMs" doc:"Total time in request, milliseconds."`
	CostUSD          float64 `json:"costUsd" doc:"Total cost, USD."`
	EnergyKwh        float64 `json:"energyKwh" doc:"Energy in kWh (cost / costPerKwh; 0 if rate unset)."`
	CachedTokens     int64   `json:"cachedTokens" doc:"Prompt tokens served from cache rather than reprocessed."`
	CacheReports     int64   `json:"cacheReports" doc:"Requests that reported any cache hit. Zero means unknown, not 0%."`
	CacheHitRate     float64 `json:"cacheHitRate" doc:"cachedTokens / promptTokens, 0..1. Only meaningful when cacheReports > 0. A property of how the caller prompts: a stable system prefix reuses cache, a shuffled one never does."`
}

// UsageByKeyOutput is per-key usage over the window, costliest first.
type UsageByKeyOutput struct {
	Body struct {
		WindowHours int           `json:"windowHours" doc:"Window applied (0 = all time)."`
		Rows        []KeyUsageRow `json:"rows" doc:"Per-key usage, costliest first."`
	}
}

// UsageByKey aggregates metered activity by caller key over a window — the data
// behind the per-key cost/requests/energy/time view.
func (h *Handlers) UsageByKey(_ context.Context, in *UsageByKeyInput) (*UsageByKeyOutput, error) {
	var sinceMS int64
	if in.WindowHours > 0 {
		sinceMS = time.Now().Add(-time.Duration(in.WindowHours) * time.Hour).UnixMilli()
	}
	rows, err := h.Store.RollupByKey(sinceMS)
	if err != nil {
		return nil, err
	}
	rate := h.config().CostPerKwh
	out := &UsageByKeyOutput{}
	out.Body.WindowHours = in.WindowHours
	out.Body.Rows = make([]KeyUsageRow, 0, len(rows))
	for _, r := range rows {
		row := KeyUsageRow{
			Key: r.Key, Requests: r.Requests,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			DwellMS: r.DwellMS, CostUSD: r.CostUSD,
			CachedTokens: r.CachedTokens, CacheReports: r.CacheReports,
		}
		if r.PromptTokens > 0 {
			row.CacheHitRate = float64(r.CachedTokens) / float64(r.PromptTokens)
		}
		if rate > 0 {
			row.EnergyKwh = r.CostUSD / rate
		}
		out.Body.Rows = append(out.Body.Rows, row)
	}
	return out, nil
}

// --- per-key usage time-series (P8-beyond) ---

// UsageSeriesInput sets the time window and bucket granularity.
type UsageSeriesInput struct {
	WindowHours   int `query:"windowHours" default:"24" minimum:"1" maximum:"8760" doc:"Trailing window in hours."`
	BucketMinutes int `query:"bucketMinutes" default:"60" minimum:"1" maximum:"1440" doc:"Bucket width in minutes."`
}

// SeriesPoint is one bucket's metrics for a key (aligned to UsageSeriesOutput.Buckets).
type SeriesPoint struct {
	Requests  int64   `json:"requests" doc:"Requests in this bucket."`
	CostUSD   float64 `json:"costUsd" doc:"Cost in this bucket, USD."`
	EnergyKwh float64 `json:"energyKwh" doc:"Energy in this bucket, kWh (cost/costPerKwh)."`
	DwellMS   int64   `json:"dwellMs" doc:"Total dwell in this bucket, ms."`
}

// KeySeries is one caller key's dense time series.
type KeySeries struct {
	Key    string        `json:"key" doc:"Caller key (empty = unkeyed)."`
	Points []SeriesPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// UsageSeriesOutput is a shared time axis plus one dense series per key.
type UsageSeriesOutput struct {
	Body struct {
		BucketMinutes int         `json:"bucketMinutes" doc:"Effective bucket width (may be coarsened)."`
		Buckets       []int64     `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Keys          []KeySeries `json:"keys" doc:"Per-key dense series, costliest first."`
	}
}

const maxSeriesBuckets = 600

// seriesAxis builds the dense, shared time axis every series chart plots against:
// bucket starts ascending, plus the position index used to place a row.
//
// Shared by the per-key and per-model series so the two charts on a page cannot
// end up on subtly different axes — same coarsening rule, same alignment, same
// number of points, so they can be read against each other.
func seriesAxis(windowHours, bucketMinutes int, now int64) (buckets []int64, index map[int64]int, bucketMS, sinceMS int64) {
	if windowHours <= 0 {
		windowHours = 24
	}
	if bucketMinutes <= 0 {
		bucketMinutes = 60
	}
	windowMS := int64(windowHours) * 3600_000
	bucketMS = int64(bucketMinutes) * 60_000
	for windowMS/bucketMS > maxSeriesBuckets {
		bucketMS *= 2
	}
	end := (now / bucketMS) * bucketMS
	start := ((now - windowMS) / bucketMS) * bucketMS
	index = map[int64]int{}
	for b := start; b <= end; b += bucketMS {
		index[b] = len(buckets)
		buckets = append(buckets, b)
	}
	return buckets, index, bucketMS, now - windowMS
}

// --- per-model usage series (P20) ---

// UsageSeriesByModelInput sets the window, granularity, and optional caller scope.
type UsageSeriesByModelInput struct {
	WindowHours   int    `query:"windowHours" default:"24" minimum:"1" maximum:"8760" doc:"Trailing window in hours."`
	BucketMinutes int    `query:"bucketMinutes" default:"60" minimum:"1" maximum:"1440" doc:"Bucket width in minutes."`
	Key           string `query:"key" doc:"Narrow to one caller key; empty covers all callers."`
}

// ModelSeries is one served model's dense time series.
type ModelSeries struct {
	Served string        `json:"served" doc:"Served model name."`
	Points []SeriesPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// UsageSeriesByModelOutput is a shared time axis plus one dense series per model.
type UsageSeriesByModelOutput struct {
	Body struct {
		BucketMinutes int           `json:"bucketMinutes" doc:"Effective bucket width (may be coarsened)."`
		Buckets       []int64       `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Models        []ModelSeries `json:"models" doc:"Per-model dense series, costliest first."`
	}
}

// UsageSeriesByModel returns per-model time series over a window, optionally
// scoped to one caller — the "on what" axis to UsageSeries's "by whom".
func (h *Handlers) UsageSeriesByModel(_ context.Context, in *UsageSeriesByModelInput) (*UsageSeriesByModelOutput, error) {
	buckets, index, bucketMS, sinceMS := seriesAxis(in.WindowHours, in.BucketMinutes, time.Now().UnixMilli())
	rows, err := h.Store.RollupSeriesByModel(sinceMS, bucketMS, in.Key)
	if err != nil {
		return nil, err
	}
	rate := h.config().CostPerKwh
	type acc struct {
		points    []SeriesPoint
		totalCost float64
	}
	byModel := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue // outside the dense axis (clock skew); skip
		}
		a := byModel[r.Served]
		if a == nil {
			a = &acc{points: make([]SeriesPoint, len(buckets))}
			byModel[r.Served] = a
		}
		energy := 0.0
		if rate > 0 {
			energy = r.CostUSD / rate
		}
		a.points[pos] = SeriesPoint{
			Requests: r.Requests, CostUSD: r.CostUSD, EnergyKwh: energy, DwellMS: r.DwellMS,
		}
		a.totalCost += r.CostUSD
	}

	out := &UsageSeriesByModelOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Models = make([]ModelSeries, 0, len(byModel))
	for m, a := range byModel {
		out.Body.Models = append(out.Body.Models, ModelSeries{Served: m, Points: a.points})
	}
	sort.Slice(out.Body.Models, func(i, j int) bool {
		return byModel[out.Body.Models[i].Served].totalCost > byModel[out.Body.Models[j].Served].totalCost
	})
	return out, nil
}

// UsageSeries returns per-key time series (requests/cost/energy/dwell) over a
// window, bucketed for charting. Buckets are dense (0-filled) so every key's
// Points align to the shared Buckets axis.
func (h *Handlers) UsageSeries(_ context.Context, in *UsageSeriesInput) (*UsageSeriesOutput, error) {
	buckets, index, bucketMS, sinceMS := seriesAxis(in.WindowHours, in.BucketMinutes, time.Now().UnixMilli())
	rows, err := h.Store.RollupSeries(sinceMS, bucketMS)
	if err != nil {
		return nil, err
	}

	rate := h.config().CostPerKwh
	// Per key: a dense slice of points + a running total cost for ordering.
	type acc struct {
		points    []SeriesPoint
		totalCost float64
	}
	byKey := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue // outside the dense axis (clock skew); skip
		}
		a := byKey[r.Key]
		if a == nil {
			a = &acc{points: make([]SeriesPoint, len(buckets))}
			byKey[r.Key] = a
		}
		energy := 0.0
		if rate > 0 {
			energy = r.CostUSD / rate
		}
		a.points[pos] = SeriesPoint{
			Requests: r.Requests, CostUSD: r.CostUSD, EnergyKwh: energy, DwellMS: r.DwellMS,
		}
		a.totalCost += r.CostUSD
	}

	out := &UsageSeriesOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Keys = make([]KeySeries, 0, len(byKey))
	for k, a := range byKey {
		out.Body.Keys = append(out.Body.Keys, KeySeries{Key: k, Points: a.points})
	}
	sort.Slice(out.Body.Keys, func(i, j int) bool {
		return byKey[out.Body.Keys[i].Key].totalCost > byKey[out.Body.Keys[j].Key].totalCost
	})
	return out, nil
}

// --- model logs (P8-beyond control plane) ---

// ModelLogsInput names the backend whose logs to fetch.
type ModelLogsInput struct {
	Backend string `query:"backend" doc:"Backend id (<servedModel>#<index>), as in residency."`
	Tail    int    `query:"tail" default:"200" minimum:"1" maximum:"2000" doc:"Max trailing lines."`
}

// ModelLogsOutput is the captured stdout/stderr tail of a spawned backend.
type ModelLogsOutput struct {
	Body struct {
		Backend string   `json:"backend" doc:"Backend id."`
		Lines   []string `json:"lines" doc:"Captured lines, oldest first (empty for pure-proxy/absent)."`
	}
}

// ModelLogs returns a spawned backend's recent stdout/stderr.
func (h *Handlers) ModelLogs(_ context.Context, in *ModelLogsInput) (*ModelLogsOutput, error) {
	lines := h.Mgr.Logs(in.Backend)
	tail := in.Tail
	if tail <= 0 {
		tail = 200
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	out := &ModelLogsOutput{}
	out.Body.Backend = in.Backend
	out.Body.Lines = lines
	if out.Body.Lines == nil {
		out.Body.Lines = []string{}
	}
	return out, nil
}

// --- per-group usage time-series (P8-beyond): spot priority starvation ---

// GroupSeriesPoint is one bucket's metrics for a priority group.
type GroupSeriesPoint struct {
	Requests int64   `json:"requests" doc:"Requests in this bucket."`
	CostUSD  float64 `json:"costUsd" doc:"Cost in this bucket, USD."`
	DwellMS  int64   `json:"dwellMs" doc:"Total dwell in this bucket, ms."`
	Rejected int64   `json:"rejected" doc:"Requests backpressured (429) — queue-pressure signal."`
	QueuedMS int64   `json:"queuedMs" doc:"Total time queued before admit/reject, ms."`
}

// GroupSeries is one priority group's dense time series.
type GroupSeries struct {
	Group  string             `json:"group" doc:"Priority group name."`
	Points []GroupSeriesPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// UsageSeriesByGroupOutput is a shared time axis plus one dense series per group
// — the data behind the stacked-area priority view (interactive-starvation watch).
type UsageSeriesByGroupOutput struct {
	Body struct {
		BucketMinutes int           `json:"bucketMinutes" doc:"Effective bucket width."`
		Buckets       []int64       `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Groups        []GroupSeries `json:"groups" doc:"Per-group dense series, busiest first."`
	}
}

// UsageSeriesByGroup buckets activity by (priority group, time), resolving each
// caller key to its group. Stacked over time it reveals whether a high-priority
// lane (e.g. interactive) is being starved under contention.
func (h *Handlers) UsageSeriesByGroup(_ context.Context, in *UsageSeriesInput) (*UsageSeriesByGroupOutput, error) {
	windowHours := in.WindowHours
	if windowHours <= 0 {
		windowHours = 24
	}
	bucketMin := in.BucketMinutes
	if bucketMin <= 0 {
		bucketMin = 60
	}
	windowMS := int64(windowHours) * 3600_000
	bucketMS := int64(bucketMin) * 60_000
	for windowMS/bucketMS > maxSeriesBuckets {
		bucketMS *= 2
	}

	now := time.Now().UnixMilli()
	end := (now / bucketMS) * bucketMS
	start := ((now - windowMS) / bucketMS) * bucketMS
	var buckets []int64
	index := map[int64]int{}
	for b := start; b <= end; b += bucketMS {
		index[b] = len(buckets)
		buckets = append(buckets, b)
	}

	rows, err := h.Store.RollupSeries(now-windowMS, bucketMS)
	if err != nil {
		return nil, err
	}

	type acc struct {
		points []GroupSeriesPoint
		total  int64
	}
	byGroup := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue
		}
		grp, _ := h.config().ResolveGroup(r.Key)
		a := byGroup[grp]
		if a == nil {
			a = &acc{points: make([]GroupSeriesPoint, len(buckets))}
			byGroup[grp] = a
		}
		p := &a.points[pos]
		p.Requests += r.Requests
		p.CostUSD += r.CostUSD
		p.DwellMS += r.DwellMS
		p.Rejected += r.Rejected
		p.QueuedMS += r.QueuedMS
		a.total += r.Requests // COUNT(*) already includes rejected attempts
	}

	out := &UsageSeriesByGroupOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Groups = make([]GroupSeries, 0, len(byGroup))
	for g, a := range byGroup {
		out.Body.Groups = append(out.Body.Groups, GroupSeries{Group: g, Points: a.points})
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool {
		return byGroup[out.Body.Groups[i].Group].total > byGroup[out.Body.Groups[j].Group].total
	})
	return out, nil
}

// --- sampled queue depth over time (P8-beyond) ---

// DepthPoint is one bucket's sampled load for a lane.
type DepthPoint struct {
	AvgActive  float64 `json:"avgActive" doc:"Mean in-flight slots in this bucket."`
	AvgWaiting float64 `json:"avgWaiting" doc:"Mean queued requests in this bucket."`
	MaxWaiting int64   `json:"maxWaiting" doc:"Peak queued requests in this bucket."`
}

// LaneDepthSeries is one priority group's dense queue-depth time series.
type LaneDepthSeriesView struct {
	Group  string       `json:"group" doc:"Priority group name."`
	Points []DepthPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// QueueDepthOutput is a shared time axis plus one dense depth series per lane.
type QueueDepthOutput struct {
	Body struct {
		BucketMinutes int                   `json:"bucketMinutes" doc:"Effective bucket width."`
		Buckets       []int64               `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Lanes         []LaneDepthSeriesView `json:"lanes" doc:"Per-lane sampled depth, busiest first."`
	}
}

// QueueDepth returns sampled per-lane queue depth (active/waiting) over time —
// instantaneous pressure that the completion-driven activity log can't show.
func (h *Handlers) QueueDepth(_ context.Context, in *UsageSeriesInput) (*QueueDepthOutput, error) {
	windowHours := in.WindowHours
	if windowHours <= 0 {
		windowHours = 24
	}
	bucketMin := in.BucketMinutes
	if bucketMin <= 0 {
		bucketMin = 60
	}
	windowMS := int64(windowHours) * 3600_000
	bucketMS := int64(bucketMin) * 60_000
	for windowMS/bucketMS > maxSeriesBuckets {
		bucketMS *= 2
	}

	now := time.Now().UnixMilli()
	end := (now / bucketMS) * bucketMS
	start := ((now - windowMS) / bucketMS) * bucketMS
	var buckets []int64
	index := map[int64]int{}
	for b := start; b <= end; b += bucketMS {
		index[b] = len(buckets)
		buckets = append(buckets, b)
	}

	rows, err := h.Store.LaneDepthSeries(now-windowMS, bucketMS)
	if err != nil {
		return nil, err
	}

	type acc struct {
		points []DepthPoint
		peak   int64
	}
	byGroup := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue
		}
		a := byGroup[r.Group]
		if a == nil {
			a = &acc{points: make([]DepthPoint, len(buckets))}
			byGroup[r.Group] = a
		}
		a.points[pos] = DepthPoint{AvgActive: r.AvgActive, AvgWaiting: r.AvgWaiting, MaxWaiting: r.MaxWaiting}
		if r.MaxWaiting > a.peak {
			a.peak = r.MaxWaiting
		}
	}

	out := &QueueDepthOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Lanes = make([]LaneDepthSeriesView, 0, len(byGroup))
	for g, a := range byGroup {
		out.Body.Lanes = append(out.Body.Lanes, LaneDepthSeriesView{Group: g, Points: a.points})
	}
	sort.Slice(out.Body.Lanes, func(i, j int) bool {
		return byGroup[out.Body.Lanes[i].Group].peak > byGroup[out.Body.Lanes[j].Group].peak
	})
	return out, nil
}

// --- groups / live admission load (P8-beyond) ---

// GroupsInput has no parameters.
type GroupsInput struct{}

// GroupView is a priority group's policy plus its aggregated live load.
type GroupView struct {
	Name          string `json:"name" doc:"Priority group name."`
	Weight        int    `json:"weight" doc:"Fairshare weight (effective)."`
	ShareCurrency string `json:"shareCurrency" doc:"requests|dwell|cost."`
	Interruptible bool   `json:"interruptible" doc:"May a higher group preempt it?"`
	Active        int    `json:"active" doc:"In-flight slots across all backends."`
	Waiting       int    `json:"waiting" doc:"Queued requests across all backends."`
}

// GroupLoadView is one group's load on one backend.
type GroupLoadView struct {
	Group   string `json:"group" doc:"Priority group name."`
	Active  int    `json:"active" doc:"In-flight slots."`
	Waiting int    `json:"waiting" doc:"Queued requests."`
}

// BackendLoadView is a backend's live admission load with a group breakdown.
type BackendLoadView struct {
	Backend  string          `json:"backend" doc:"Backend id."`
	Capacity int             `json:"capacity" doc:"Configured slots."`
	Active   int             `json:"active" doc:"Slots in use."`
	Waiting  int             `json:"waiting" doc:"Queued requests."`
	Groups   []GroupLoadView `json:"groups" doc:"Per-group breakdown."`
}

// GroupsOutput reports priority groups and per-backend admission load.
type GroupsOutput struct {
	Body struct {
		Groups   []GroupView       `json:"groups" doc:"Priority groups with aggregated load."`
		Backends []BackendLoadView `json:"backends" doc:"Per-backend live load."`
	}
}

// Groups returns priority-group policy joined with live admission load — the
// scheduler's per-backend slots/inflight/waiting, aggregated per group.
func (h *Handlers) Groups(_ context.Context, _ *GroupsInput) (*GroupsOutput, error) {
	snap := h.Sched.Snapshot()

	// Aggregate live active/waiting per group across backends.
	type load struct{ active, waiting int }
	agg := map[string]*load{}
	out := &GroupsOutput{}
	out.Body.Backends = make([]BackendLoadView, 0, len(snap.Backends))
	for _, b := range snap.Backends {
		bv := BackendLoadView{
			Backend: b.Backend, Capacity: b.Capacity, Active: b.Active, Waiting: b.Waiting,
			Groups: make([]GroupLoadView, 0, len(b.Groups)),
		}
		for _, g := range b.Groups {
			bv.Groups = append(bv.Groups, GroupLoadView{Group: g.Group, Active: g.Active, Waiting: g.Waiting})
			l := agg[g.Group]
			if l == nil {
				l = &load{}
				agg[g.Group] = l
			}
			l.active += g.Active
			l.waiting += g.Waiting
		}
		out.Body.Backends = append(out.Body.Backends, bv)
	}

	// Union of configured groups and any group seen live (e.g. synthesized default).
	names := map[string]struct{}{}
	for name := range h.config().PriorityGroups {
		names[name] = struct{}{}
	}
	for name := range agg {
		names[name] = struct{}{}
	}
	out.Body.Groups = make([]GroupView, 0, len(names))
	for name := range names {
		pg := h.config().PriorityGroups[name] // zero value if unlisted (e.g. default)
		gv := GroupView{
			Name:          name,
			Weight:        pg.EffectiveWeight(),
			ShareCurrency: h.Sched.ShareCurrency(name),
			Interruptible: pg.Interruptible,
		}
		if l := agg[name]; l != nil {
			gv.Active, gv.Waiting = l.active, l.waiting
		}
		out.Body.Groups = append(out.Body.Groups, gv)
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool { return out.Body.Groups[i].Name < out.Body.Groups[j].Name })
	return out, nil
}

// --- reservations: live slot leases (interactive headroom) ---

type ReservationsInput struct{}

// ReservationView is one live reservation: a lane's short lease on N slots of a
// model's primary backend, held free so interactive work has headroom.
type ReservationView struct {
	Model     string `json:"model" doc:"Model whose primary backend is reserved."`
	Backend   string `json:"backend" doc:"Scheduler backend id (model#0)."`
	Lane      string `json:"lane" doc:"Priority group the slots are held for."`
	Slots     int    `json:"slots" doc:"Slots held free."`
	ExpiresAt string `json:"expiresAt" doc:"RFC3339 lease expiry; renewed by heartbeat re-POST."`
}

// ReservationsOutput lists live slot reservations.
type ReservationsOutput struct {
	Body struct {
		Reservations []ReservationView `json:"reservations" doc:"Live reservations (expired ones pruned)."`
	}
}

// Reservations returns the scheduler's live slot leases for the dashboard.
func (h *Handlers) Reservations(_ context.Context, _ *ReservationsInput) (*ReservationsOutput, error) {
	out := &ReservationsOutput{}
	live := h.Sched.Reservations()
	out.Body.Reservations = make([]ReservationView, 0, len(live))
	for _, r := range live {
		out.Body.Reservations = append(out.Body.Reservations, ReservationView{
			Model:     strings.TrimSuffix(r.Backend, "#0"),
			Backend:   r.Backend,
			Lane:      r.Lane,
			Slots:     r.Slots,
			ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// --- active requests: what the proxy is serving RIGHT NOW ---

type ActiveRequestsInput struct{}

// ActiveRequestView is one in-flight request. The activity log only holds
// FINISHED requests, so a long completion or a cold load is invisible there
// until it ends; this is the live half.
type ActiveRequestView struct {
	ID        int64  `json:"id" doc:"Process-local id (not the activity row id)."`
	Served    string `json:"served" doc:"Requested model/lane name."`
	Backend   string `json:"backend" doc:"Backend admitted on (empty while still choosing)."`
	Group     string `json:"group" doc:"Priority group (lane) the caller resolved to."`
	Key       string `json:"key" doc:"Caller key (empty = unkeyed/default lane)."`
	SourceIP  string `json:"sourceIp" doc:"Client IP."`
	Path      string `json:"path" doc:"Request path."`
	Streaming bool   `json:"streaming" doc:"Client asked for a streamed response."`
	BytesOut  int64  `json:"bytesOut" doc:"Response bytes relayed so far. Live — grows while the request runs."`
	Chunks    int64  `json:"chunks" doc:"Response writes so far; for a streamed reply roughly the token count. A reply still growing at tens of thousands is looping, not slow."`
	Retryable bool   `json:"retryable" doc:"Caller volunteered this request for preemption (X-Corrallm-Retryable). A request may only widen its interruptibility, never narrow it."`
	State     string `json:"state" doc:"queued (awaiting a slot) | loading (awaiting backend) | streaming (proxying)."`
	StartedAt string `json:"startedAt" doc:"RFC3339 arrival time."`
	ElapsedMS int64  `json:"elapsedMs" doc:"Milliseconds in flight so far."`
}

// ActiveRequestsOutput lists live requests, oldest first.
type ActiveRequestsOutput struct {
	Body struct {
		Requests []ActiveRequestView `json:"requests" doc:"In-flight requests, oldest first."`
	}
}

// ActiveRequests returns the proxy's in-flight request registry. A nil Proxy
// (schema dump, tests) yields an empty list rather than an error.
func (h *Handlers) ActiveRequests(_ context.Context, _ *ActiveRequestsInput) (*ActiveRequestsOutput, error) {
	out := &ActiveRequestsOutput{}
	if h.Proxy == nil {
		out.Body.Requests = []ActiveRequestView{}
		return out, nil
	}
	live := h.Proxy.Inflight()
	out.Body.Requests = make([]ActiveRequestView, 0, len(live))
	for _, r := range live {
		out.Body.Requests = append(out.Body.Requests, ActiveRequestView{
			ID: r.ID, Served: r.Served, Backend: r.Backend, Group: r.Group,
			Key: r.Key, SourceIP: r.SourceIP, Path: r.Path, Streaming: r.Streaming,
			Retryable: r.Retryable, BytesOut: r.BytesOut, Chunks: r.Chunks,
			State: r.State, StartedAt: r.StartedAt.UTC().Format(time.RFC3339),
			ElapsedMS: r.ElapsedMS,
		})
	}
	return out, nil
}

// --- overview: model/lane definitions + capacity (P8-beyond) ---

// PoolDef is a server pool's declared total and reserved headroom.
type PoolDef struct {
	Pool         string `json:"pool" doc:"Pool name."`
	TotalBytes   int64  `json:"totalBytes" doc:"Declared pool size."`
	ReserveBytes int64  `json:"reserveBytes" doc:"Headroom kept free."`
}

// ServerDef is a server's declared capacity.
type ServerDef struct {
	Server          string    `json:"server" doc:"Server name."`
	MaxConcurrent   int       `json:"maxConcurrent" doc:"Optional host concurrency cap (0 = none)."`
	AgentEndpoints  []string  `json:"agentEndpoints" doc:"Candidate addresses of the agent backing this server, preference order. Empty means the server is this machine. Several are normal — a LAN address, a VPN address and an external one can all be valid at once."`
	DevicePool      string    `json:"devicePool" doc:"Pool holding accelerator memory on this server — the one a measured device reading describes. Unified-memory hosts point it at their single system pool."`
	AgentStatus     string    `json:"agentStatus" doc:"up | down | unknown | local. 'unknown' means an agent is configured but has never reported in; 'down' means it stopped."`
	AgentLastSeen   int64     `json:"agentLastSeen" doc:"Unix millis of the last heartbeat; 0 if never."`
	NoProcessMemory bool      `json:"noProcessMemory" doc:"This host cannot attribute memory per process (macOS), so a model's ramUsage is required and authoritative rather than advisory."`
	Notes           string    `json:"notes" doc:"Free text kept with this server."`
	Pools           []PoolDef `json:"pools" doc:"Declared memory pools."`
}

// ModelDef is a served model's single serving path + residency policy.
// Spawnable models carry their cmd; pure-proxy models have an empty cmd and
// forward to Target. Auth headers on remote targets are NOT exposed.
type ModelDef struct {
	Name       string         `json:"name" doc:"Served model name."`
	Persistent bool           `json:"persistent" doc:"Pinned (preloaded, never evicted)."`
	TTL        string         `json:"ttl" doc:"Eviction PRIORITY (sticky): once idle this long the model sorts ahead of warmer ones as an eviction victim. It never unloads anything on its own — see idleUnload."`
	IdleUnload string         `json:"idleUnload" doc:"Quiet period after which the backend unloads itself, with nothing else needing its memory (sticky). Empty = never. Counts only while no request is in flight."`
	EvictCost  string         `json:"evictCost" doc:"Eviction resistance (sticky): the tiebreak among candidates once TTL ordering is applied."`
	Spawnable  bool           `json:"spawnable" doc:"True if a local process backs it — its own cmd, or its hosting extension's."`
	Remote     bool           `json:"remote" doc:"True if served by a host corrallm does not run (no local process, non-loopback target). Never counted as loaded."`
	ProcKey    string         `json:"procKey" doc:"Backing process identity; an extension's models share one."`
	Modalities []ModalityView `json:"modalities" doc:"Accepted input modalities (text|image|audio) with optional per-modality metadata."`
	Capability string         `json:"capability" doc:"chat|embeddings|audio.stt|audio.realtime|audio.tts|rerank (delivery surfaces kept distinct)."`
	// Placements are the ways this model can be served: a box and the command
	// that runs it there. Controls belong on THESE rather than on the model —
	// loading, pausing, logs and the backend's own UI are all properties of one
	// process on one box, and a model with two placements has two of each.
	Placements []PlacementView `json:"placements" doc:"Ways this model can be served: one per (box, cmd)."`
	// ContextPerRequest is the window one request may use. Surfaced because it
	// is the difference between a model that can read a document and one that
	// cannot, and it was previously invisible outside the config file.
	ContextPerRequest int     `json:"contextPerRequest" doc:"Context window guaranteed per request (0 = whatever the backend was launched with, undivided)."`
	Type              string  `json:"type" doc:"Cost class (chat | embed | openrouter | …)."`
	Quality           float64 `json:"quality" doc:"Relative quality rank. Fractional tiers are legal — a model that belongs between two existing tiers is 1.5, not a renumbered ladder."`
	Server            string  `json:"server" doc:"Server it draws capacity from (spawned only)."`
	Target            string  `json:"target" doc:"Forward URL (scheme://host:port; headers redacted)."`
	MaxConcurrent     int     `json:"maxConcurrent" doc:"Admission slots."`
	MaxTokens         int     `json:"maxTokens" doc:"max_tokens clamp when degraded onto (0 = none)."`
	Cmd               string  `json:"cmd" doc:"Spawn command (empty for pure-proxy)."`
	Notes             string  `json:"notes" doc:"Free text kept with this model — why it is configured the way it is. Carried in config and editable in the dashboard."`
	Upstream          string  `json:"upstream" doc:"The id the BACKEND knows this model by, when it differs from the served name. Empty means the backend uses the served name. Outbound only — the inbound counterpart is 'aliases', which are extra names CALLERS may use for this model."`
	// Pause state rides on the model definition rather than on the residency
	// snapshot because a paused model has no process to snapshot — that is the
	// whole point of it — and would therefore be invisible there.
	Paused            bool   `json:"paused" doc:"Out of service by operator order: never spawned until resumed."`
	PauseScope        string `json:"pauseScope" doc:"model | extension — whether this model was paused on its own or by its hosting extension. Empty when not paused."`
	PausedByExtension string `json:"pausedByExtension" doc:"The extension whose pause covers this model, when pauseScope is 'extension'."`
	PauseReason       string `json:"pauseReason" doc:"Why it is paused (operator-supplied)."`
	PausedAtMS        int64  `json:"pausedAtMs" doc:"Unix millis the pause was set (0 if not paused)."`
	PauseResumeMS     int64  `json:"pauseResumeMs" doc:"Unix millis the pause lifts on its own; 0 = indefinite."`
}

// ModalityView is one accepted input modality plus optional client-facing
// metadata — the GraphQL-friendly list form of the config's modalities map
// (GraphQL has no map type). Metadata fields are per-modality: image sets
// maxResolution/formats, audio sets formats, text may set maxTokens.
// PlacementView is one way of serving a model, with what was PROBED about it
// rather than what was declared.
//
// Capabilities sit here and not on the model because they differ per placement:
// the same weights expose vision on a cmd that loaded a projector and not on
// one that did not. Two boxes are not assumed to agree, which is why each is
// probed separately.
type PlacementView struct {
	Name          string   `json:"name" doc:"Stable id: what the process, profile and capability record are filed under."`
	Server        string   `json:"server" doc:"The box."`
	Cmd           string   `json:"cmd" doc:"What runs there."`
	Target        string   `json:"target" doc:"Resolved forward destination for this placement."`
	State         string   `json:"state" doc:"absent|loading|ready|failed|evicting|draining for THIS placement's process."`
	HasUI         bool     `json:"hasUI" doc:"Whether this backend serves a web UI (probed)."`
	Probed        bool     `json:"probed" doc:"Whether this placement has ever been probed."`
	ProbedStale   bool     `json:"probedStale" doc:"The recorded capabilities were taken from a DIFFERENT cmd than the one configured now."`
	Modalities    []string `json:"modalities" doc:"Probed input modalities for this placement."`
	Tools         bool     `json:"tools" doc:"Probed tool-calling support."`
	ContextLength int      `json:"contextLength" doc:"Context this placement actually got when probed."`
	Slots         int      `json:"slots" doc:"Concurrency this placement reported."`
	MemoryMiB     int      `json:"memoryMiB" doc:"Peak measured footprint on this placement."`
	Upstream      string   `json:"upstream" doc:"Id the backend answers to."`
}

type ModalityView struct {
	Modality      string   `json:"modality" doc:"text|image|audio."`
	MaxResolution int      `json:"maxResolution,omitempty" doc:"image: longest-edge pixel cap."`
	Formats       []string `json:"formats,omitempty" doc:"image/audio: accepted encodings."`
	MaxTokens     int      `json:"maxTokens,omitempty" doc:"text: generation-length cap."`
}

// modalityViews converts a config modalities map to a stable, key-sorted list
// for the GraphQL surface.
// placementViews describes each way a model can be served, with what was
// probed about that way.
//
// Built here rather than on the client because it joins three sources the UI
// has no business knowing about: the config's placement list, the live process
// table, and the probe/measurement records keyed by placement name.
func (h *Handlers) placementViews(name string, m config.Model) []PlacementView {
	out := []PlacementView{}
	for _, pl := range m.PlacementList() {
		v := PlacementView{Name: pl.Name, Server: pl.Server, Cmd: pl.Cmd}
		if t, err := h.config().TargetFor(name, m.ForPlacement(pl)); err == nil && t != nil {
			v.Target = t.BaseURLString()
		}
		if h.Mgr != nil {
			key := m.PlacementProcKey(name, pl)
			v.State = string(h.Mgr.StateOf(key))
			v.MemoryMiB = h.Mgr.PlacementPeak(name, pl)
			if caps, ok := h.Mgr.Capabilities(pl.Name, name); ok {
				v.Probed = true
				// The recorded capabilities describe the cmd they were taken
				// from. If that has since changed, saying so beats presenting a
				// stale answer as current.
				v.ProbedStale = caps.StaleFor(pl.Cmd)
				v.Modalities = caps.Modalities
				v.Tools = caps.Tools
				v.ContextLength = caps.ContextLength
				v.Slots = caps.Slots
				v.HasUI = caps.HasUI
				v.Upstream = caps.Upstream
			}
		}
		out = append(out, v)
	}
	return out
}

func modalityViews(m map[string]config.ModalitySpec) []ModalityView {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ModalityView, 0, len(keys))
	for _, k := range keys {
		s := m[k]
		out = append(out, ModalityView{Modality: k, MaxResolution: s.MaxResolution, Formats: s.Formats, MaxTokens: s.MaxTokens})
	}
	return out
}

// LaneMemberDef is one lane member: a model name + optional sticky override.
type LaneMemberDef struct {
	Model      string `json:"model" doc:"Member model name."`
	TTL        string `json:"ttl" doc:"Eviction-priority override when loaded via this lane (empty = model's own)."`
	IdleUnload string `json:"idleUnload" doc:"Self-unload quiet-period override when loaded via this lane (empty = model's own)."`
	EvictCost  string `json:"evictCost" doc:"Eviction-resistance override (empty = model's own)."`
}

// LaneDef is a named ordered fallback list over models: requesting the lane
// name allows substitution across members; requesting a model name pins it.
type LaneDef struct {
	Name string `json:"name" doc:"Lane name (requestable as a model id)."`
	// Members is what the CONFIG declares, in declared order.
	Members []LaneMemberDef `json:"members" doc:"Members written in config, in declared order."`
	// Ladder is what the lane actually resolves to right now, in the order the
	// walk will try it, each rung tagged with where it came from.
	//
	// Reported separately from Members because they answer different questions
	// and the panel needs the second one: a lane can gain rungs from a selector,
	// from models chosen off a directory, and from a virtual extension's pool,
	// none of which appear in the config's member list. Showing only Members
	// meant `free` displayed two entries while resolving to twelve.
	Ladder []LaneRungDef `json:"ladder" doc:"Resolved membership in fallback order, with each rung's origin."`
}

// LaneRungDef is one resolved rung of a lane.
type LaneRungDef struct {
	Model  string `json:"model"`
	Origin string `json:"origin" doc:"declared | selector | selection | pool."`
	Pool   string `json:"pool" doc:"The virtual extension it came from, when origin is pool."`
}

// StageView summarizes a group's saturation policy for one backend type.
type StageView struct {
	Type   string `json:"type" doc:"Backend type (or \"default\")."`
	Policy string `json:"policy" doc:"Human-readable stage summary."`
}

// GroupDef is a priority group's (lane's) policy.
type GroupDef struct {
	Name          string      `json:"name" doc:"Group name."`
	Weight        int         `json:"weight" doc:"Fairshare weight."`
	ShareCurrency string      `json:"shareCurrency" doc:"requests | dwell | cost."`
	Interruptible bool        `json:"interruptible" doc:"May a higher group preempt it?"`
	AcceptDegrade bool        `json:"acceptDegrade" doc:"Opts into quality-degrade fall-through."`
	QualityFloor  float64     `json:"qualityFloor" doc:"Lowest accepted quality when degrading."`
	Stages        []StageView `json:"stages" doc:"Per-type saturation policy."`
}

// KeyDef maps a caller key to its group.
type KeyDef struct {
	Key   string `json:"key" doc:"Caller key."`
	Group string `json:"group" doc:"Priority group it resolves to."`
}

// OverviewInput has no parameters.
type OverviewInput struct{}

// OverviewOutput is the loaded config rendered for the Overview control plane.
type OverviewOutput struct {
	Body struct {
		Include    []string       `json:"include" doc:"Config files merged into the top-level one, weakest first. A generated file (agent-contributed models) lives here so the hand-written config is never rewritten."`
		Servers    []ServerDef    `json:"servers" doc:"Declared host capacity."`
		Models     []ModelDef     `json:"models" doc:"Served models (one serving path each)."`
		Lanes      []LaneDef      `json:"lanes" doc:"Named fallback lists over models."`
		Extensions []ExtensionDef `json:"extensions" doc:"Integrations that serve several models from one process."`
		Groups     []GroupDef     `json:"groups" doc:"Priority-group policies."`
		Keys       []KeyDef       `json:"keys" doc:"Caller key → group mappings."`
	}
}

// ExtensionDef is an integration that serves several models from one process.
type ExtensionDef struct {
	Name     string   `json:"name"`
	Cmd      string   `json:"cmd" doc:"Spawn command; empty for a remote integration with no local process."`
	Server   string   `json:"server"`
	Provides []string `json:"provides" doc:"Served model names it contributes."`
	Notes    string   `json:"notes"`
	// Live process state rides along so the dashboard's extension panel needs
	// one query, not two. An extension IS the process for the models it hosts,
	// so this is the only place their residency is addressable as one thing.
	State         string `json:"state" doc:"absent|loading|ready|failed|evicting|draining."`
	Draining      bool   `json:"draining" doc:"An unload is waiting on in-flight requests."`
	InFlight      int    `json:"inFlight" doc:"Requests currently holding the process."`
	Pinned        bool   `json:"pinned" doc:"Persistent: preloaded and exempt from eviction."`
	Paused        bool   `json:"paused" doc:"Out of service by operator order; every model it provides is refused."`
	PauseReason   string `json:"pauseReason" doc:"Why it is paused (operator-supplied)."`
	PausedAtMS    int64  `json:"pausedAtMs" doc:"Unix millis the pause was set (0 if not paused)."`
	PauseResumeMS int64  `json:"pauseResumeMs" doc:"Unix millis the pause lifts on its own; 0 = indefinite."`
}

// stageSummary renders a saturation Stage as a short human-readable policy.
func stageSummary(s config.Stage) string {
	var parts []string
	if s.Preempt {
		parts = append(parts, "preempt")
	}
	if s.Queue {
		parts = append(parts, "queue")
	}
	if s.Spill || s.FallThrough {
		parts = append(parts, "spill")
	}
	if s.Reject {
		parts = append(parts, "reject")
	}
	if s.Then != "" {
		parts = append(parts, "then "+s.Then)
	}
	for dim, lim := range s.Limits {
		parts = append(parts, "limit "+dim+"="+lim)
	}
	if len(parts) == 0 {
		return "reject"
	}
	return strings.Join(parts, ", ")
}

// Overview returns model/lane definitions and declared system capacity.
func (h *Handlers) Overview(_ context.Context, _ *OverviewInput) (*OverviewOutput, error) {
	out := &OverviewOutput{}
	out.Body.Include = h.config().Include

	for name, srv := range h.config().Servers {
		sd := ServerDef{Server: name, MaxConcurrent: srv.MaxConcurrent, DevicePool: h.config().DevicePoolFor(name)}
		sd.Notes = srv.Notes
		sd.NoProcessMemory = srv.NoProcessMemory
		sd.AgentStatus = "local"
		if srv.Agent != nil {
			sd.AgentEndpoints = srv.Agent.Endpoints
			// Reachability is what the dashboard actually needs: an agent that
			// is configured but silent looks identical to one that is working,
			// and the difference decides whether anything can be spawned there.
			sd.AgentStatus = string(h.Liveness.Status(name, time.Now()))
			if t, ok := h.Liveness.LastSeen(name); ok {
				sd.AgentLastSeen = t.UnixMilli()
			}
		}
		totals, _ := config.ParseSizes(srv.Pools)
		reserve, _ := config.ParseSizes(srv.Reserve)
		for pool, total := range totals {
			sd.Pools = append(sd.Pools, PoolDef{Pool: pool, TotalBytes: total, ReserveBytes: reserve[pool]})
		}
		sort.Slice(sd.Pools, func(i, j int) bool { return sd.Pools[i].Pool < sd.Pools[j].Pool })
		out.Body.Servers = append(out.Body.Servers, sd)
	}
	sort.Slice(out.Body.Servers, func(i, j int) bool { return out.Body.Servers[i].Server < out.Body.Servers[j].Server })

	costModel := cost.NewModel(h.config()) // modality inference (P9d): audio cost class ⇒ audio model
	// Keyed by PROCESS, matching how the manager holds them: an extension's
	// models all resolve to the same entry, so every one of them reports paused
	// when their extension is.
	pauses := map[string]proc.Pause{}
	if h.Mgr != nil {
		for _, p := range h.Mgr.Pauses() {
			pauses[p.Key] = p
		}
	}
	for name, m := range h.config().Models {
		md := ModelDef{
			Name: name, Persistent: m.Persistent, Capability: config.ModelCapability(m),
			ContextPerRequest: m.ContextPerRequest,
			Placements:        h.placementViews(name, m),
			Modalities:        modalityViews(m.EffectiveModalities(name, costModel.IsAudioType(m.Type))),
			// Spawnable off m.Cmd alone was wrong for an extension's models: their
			// cmd lives on the extension, so oidio-stt (a real local process)
			// reported spawnable:false and the UI labelled it a proxy.
			Type: m.Type, Quality: m.Quality, Spawnable: m.LocalProcess(), Remote: m.Remote(),
			ProcKey: m.ProcKey(name), Server: m.Server, Upstream: m.Upstream, Notes: m.Notes,
			MaxConcurrent: m.Slots(), MaxTokens: m.MaxTokens, Cmd: m.Cmd,
		}
		if m.Sticky != nil {
			md.TTL, md.IdleUnload, md.EvictCost = m.Sticky.TTL, m.Sticky.IdleUnload, m.Sticky.EvictCost
		}
		if p, ok := pauses[md.ProcKey]; ok {
			md.Paused, md.PauseReason, md.PausedAtMS = true, p.Reason, p.At.UnixMilli()
			// PauseScope tells the UI whether this model was paused on its own
			// or swept up by its extension — without it a hosted model reads as
			// individually paused and "Resume" looks like it affects only it.
			md.PauseScope = "model"
			if ext, isExt := p.Extension(); isExt {
				md.PauseScope, md.PausedByExtension = "extension", ext
			}
			if !p.ResumeAt.IsZero() {
				md.PauseResumeMS = p.ResumeAt.UnixMilli()
			}
		}
		if t, err := h.config().TargetFor(name, m); err == nil {
			md.Target = t.URL.String() // headers (auth) intentionally omitted
		}
		out.Body.Models = append(out.Body.Models, md)
	}
	sort.Slice(out.Body.Models, func(i, j int) bool { return out.Body.Models[i].Name < out.Body.Models[j].Name })

	extState := map[string]proc.ExtensionState{}
	if h.Mgr != nil {
		for _, st := range h.Mgr.ExtensionStates() {
			extState[st.Name] = st
		}
	}
	for name, ext := range h.config().Extensions {
		ed := ExtensionDef{
			Name: name, Cmd: ext.Cmd, Server: ext.Server, Notes: ext.Notes,
			Provides: h.config().ExtensionModels(name),
			State:    string(proc.StateAbsent), Pinned: ext.Persistent,
		}
		if st, ok := extState[name]; ok {
			ed.State, ed.Draining, ed.InFlight = string(st.State), st.Draining, st.InFlight
			ed.Paused, ed.PauseReason = st.Paused, st.PauseReason
			ed.PausedAtMS, ed.PauseResumeMS = st.PausedAtMS, st.PauseResumeMS
		}
		out.Body.Extensions = append(out.Body.Extensions, ed)
	}
	sort.Slice(out.Body.Extensions, func(i, j int) bool { return out.Body.Extensions[i].Name < out.Body.Extensions[j].Name })

	for name, lane := range h.config().Lanes {
		ld := LaneDef{Name: name, Ladder: []LaneRungDef{}}
		for _, mem := range lane.Members {
			lm := LaneMemberDef{Model: mem.Model}
			if mem.Sticky != nil {
				lm.TTL, lm.IdleUnload, lm.EvictCost = mem.Sticky.TTL, mem.Sticky.IdleUnload, mem.Sticky.EvictCost
			}
			ld.Members = append(ld.Members, lm)
		}
		for _, e := range h.config().LaneLadder(name) {
			ld.Ladder = append(ld.Ladder, LaneRungDef{Model: e.Name, Origin: e.Origin, Pool: e.Pool})
		}
		out.Body.Lanes = append(out.Body.Lanes, ld)
	}
	sort.Slice(out.Body.Lanes, func(i, j int) bool { return out.Body.Lanes[i].Name < out.Body.Lanes[j].Name })

	for name, g := range h.config().PriorityGroups {
		gd := GroupDef{
			Name: name, Weight: g.EffectiveWeight(), ShareCurrency: shareCurrencyOf(g),
			Interruptible: g.Interruptible, AcceptDegrade: g.AcceptDegrade, QualityFloor: g.QualityFloor,
		}
		for typ, st := range g.OnSaturated {
			gd.Stages = append(gd.Stages, StageView{Type: typ, Policy: stageSummary(st)})
		}
		sort.Slice(gd.Stages, func(i, j int) bool { return gd.Stages[i].Type < gd.Stages[j].Type })
		out.Body.Groups = append(out.Body.Groups, gd)
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool { return out.Body.Groups[i].Name < out.Body.Groups[j].Name })

	for k, grp := range h.config().Keys {
		out.Body.Keys = append(out.Body.Keys, KeyDef{Key: k, Group: grp})
	}
	sort.Slice(out.Body.Keys, func(i, j int) bool { return out.Body.Keys[i].Key < out.Body.Keys[j].Key })

	return out, nil
}

// shareCurrencyOf returns a group's configured share currency, defaulting to requests.
func shareCurrencyOf(g config.PriorityGroup) string {
	switch g.ShareCurrency {
	case "dwell", "cost":
		return g.ShareCurrency
	default:
		return "requests"
	}
}

// --- load / unload mutations (P8-beyond control plane) ---

// ModelActionInput names the served model to load/unload.
type ModelActionInput struct {
	Body struct {
		Model string `json:"model" doc:"Served model name."`
	}
}

// ModelActionOutput reports the result of a load/unload.
type ModelActionOutput struct {
	Body struct {
		OK      bool   `json:"ok" doc:"Whether the action succeeded."`
		Message string `json:"message" doc:"Human-readable result or error."`
		Backend string `json:"backend" doc:"Backend loaded (load only)."`
		Evicted int    `json:"evicted" doc:"Backends evicted (unload only)."`
	}
}

// LoadModel warms a model on demand (spawns its first spawnable backend).
func (h *Handlers) LoadModel(ctx context.Context, in *ModelActionInput) (*ModelActionOutput, error) {
	out := &ModelActionOutput{}
	name, err := h.Mgr.LoadModel(ctx, in.Body.Model)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Backend = name
	out.Body.Message = "loaded " + name
	return out, nil
}

// UnloadModel evicts a model's resident backends (refuses pinned / in-flight).
func (h *Handlers) UnloadModel(_ context.Context, in *ModelActionInput) (*ModelActionOutput, error) {
	out := &ModelActionOutput{}
	n, err := h.Mgr.UnloadModel(in.Body.Model)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Evicted = n
	out.Body.Message = fmt.Sprintf("evicted %d backend(s)", n)
	return out, nil
}

// --- pause / resume ---

// PauseModelInput takes a model out of service, optionally until a moment.
type PauseModelInput struct {
	Body struct {
		Model string `json:"model" doc:"Served model name."`
		// RFC3339 rather than a duration: the operator picks a wall-clock
		// moment in the dashboard ("back at 09:00 tomorrow"), and translating
		// that to a duration in the browser would bake in the client's clock
		// skew. Empty = paused until explicitly resumed.
		ResumeAt string `json:"resumeAt,omitempty" doc:"RFC3339 time the pause lifts on its own. Must be in the future. Empty = indefinite."`
		Reason   string `json:"reason,omitempty" doc:"Why it is paused; shown in the dashboard."`
	}
}

// PauseExtensionInput takes an extension out of service by its own name.
type PauseExtensionInput struct {
	Body struct {
		Extension string `json:"extension" doc:"Extension name (e.g. oidio)."`
		ResumeAt  string `json:"resumeAt,omitempty" doc:"RFC3339 time the pause lifts on its own. Must be in the future. Empty = indefinite."`
		Reason    string `json:"reason,omitempty" doc:"Why it is paused; shown in the dashboard."`
	}
}

// PauseModelOutput reports what the pause did to the running process.
type PauseModelOutput struct {
	Body struct {
		OK      bool   `json:"ok" doc:"Whether the pause was applied."`
		Message string `json:"message" doc:"Human-readable result or error."`
		// Target and Affected exist because a pause on an extension-hosted
		// model does NOT stop at that model: it is one process, so it pauses
		// the extension and every sibling. Saying so is the difference between
		// a control the operator can trust and one that surprises them.
		Target   string   `json:"target" doc:"Process key actually paused: a model name, or \"extension:<name>\"."`
		Affected []string `json:"affected" doc:"Every served model this pause took out of service."`
		Evicted  int      `json:"evicted" doc:"Backends evicted immediately."`
		Draining bool     `json:"draining" doc:"In-flight requests are finishing before the process goes."`
	}
}

// parseResumeAt reads the optional RFC3339 resume time shared by both pause ops.
func parseResumeAt(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad resumeAt %q: %w", s, err)
	}
	return t, nil
}

// describePause renders a PauseResult for a human, naming the blast radius when
// it is wider than the thing that was asked for.
func describePause(asked string, res proc.PauseResult) string {
	msg := "paused"
	if ext, ok := strings.CutPrefix(res.Target, "extension:"); ok && asked != ext {
		msg = fmt.Sprintf("paused extension %q (%s is hosted by it) — %d model(s) out of service: %v",
			ext, asked, len(res.Affected), res.Affected)
	}
	switch {
	case res.Skipped != "":
		return msg + "; " + res.Skipped
	case res.Draining:
		return msg + "; draining in-flight requests before unload"
	default:
		return fmt.Sprintf("%s; evicted %d backend(s)", msg, res.Evicted)
	}
}

func pauseOutput(asked string, res proc.PauseResult) *PauseModelOutput {
	out := &PauseModelOutput{}
	out.Body.OK = true
	out.Body.Target, out.Body.Affected = res.Target, res.Affected
	out.Body.Evicted, out.Body.Draining = res.Evicted, res.Draining
	out.Body.Message = describePause(asked, res)
	return out
}

// PauseModel unloads a model and keeps it unloaded until it is resumed. An
// extension-hosted model pauses its whole extension — one process, one pause.
func (h *Handlers) PauseModel(_ context.Context, in *PauseModelInput) (*PauseModelOutput, error) {
	resumeAt, err := parseResumeAt(in.Body.ResumeAt)
	if err != nil {
		out := &PauseModelOutput{}
		out.Body.Message = err.Error()
		return out, nil
	}
	res, err := h.Mgr.PauseModel(in.Body.Model, in.Body.Reason, resumeAt)
	if err != nil {
		out := &PauseModelOutput{}
		out.Body.Message = err.Error()
		return out, nil
	}
	return pauseOutput(in.Body.Model, res), nil
}

// PauseExtension takes an extension and every model it provides out of service.
func (h *Handlers) PauseExtension(_ context.Context, in *PauseExtensionInput) (*PauseModelOutput, error) {
	resumeAt, err := parseResumeAt(in.Body.ResumeAt)
	if err != nil {
		out := &PauseModelOutput{}
		out.Body.Message = err.Error()
		return out, nil
	}
	res, err := h.Mgr.PauseExtension(in.Body.Extension, in.Body.Reason, resumeAt)
	if err != nil {
		out := &PauseModelOutput{}
		out.Body.Message = err.Error()
		return out, nil
	}
	return pauseOutput(in.Body.Extension, res), nil
}

// UnpauseModel returns a model to service (reloading it if it is pinned). For a
// hosted model that resumes its whole extension.
func (h *Handlers) UnpauseModel(ctx context.Context, in *ModelActionInput) (*ModelActionOutput, error) {
	out := &ModelActionOutput{}
	was, err := h.Mgr.UnpauseModel(ctx, in.Body.Model)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	if was {
		out.Body.Message = "resumed " + in.Body.Model
	} else {
		out.Body.Message = in.Body.Model + " was not paused"
	}
	return out, nil
}

// UnpauseExtension returns an extension and all its models to service.
func (h *Handlers) UnpauseExtension(ctx context.Context, in *ExtensionActionInput) (*ExtensionActionOutput, error) {
	out := &ExtensionActionOutput{}
	was, err := h.Mgr.UnpauseExtension(ctx, in.Body.Extension)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	if was {
		out.Body.Message = "resumed " + in.Body.Extension
	} else {
		out.Body.Message = in.Body.Extension + " was not paused"
	}
	return out, nil
}

// ExtensionActionInput addresses an extension by name.
type ExtensionActionInput struct {
	Body struct {
		Extension string `json:"extension" doc:"Extension name (e.g. oidio)"`
	}
}

// ExtensionActionOutput reports the result of a load/unload.
type ExtensionActionOutput struct {
	Body struct {
		OK bool `json:"ok"`
		// Draining is true when an unload is waiting on in-flight requests. The
		// process is still up and still serving them; it admits nothing new.
		Draining bool   `json:"draining,omitempty"`
		Evicted  int    `json:"evicted,omitempty"`
		Message  string `json:"message"`
	}
}

// ExtensionsOutput lists every declared extension and its process state.
type ExtensionsOutput struct {
	Body struct {
		Extensions []proc.ExtensionState `json:"extensions"`
	}
}

// LoadExtension warms an extension's process, addressed by the extension rather
// than by one of the models it provides.
func (h *Handlers) LoadExtension(ctx context.Context, in *ExtensionActionInput) (*ExtensionActionOutput, error) {
	out := &ExtensionActionOutput{}
	name, err := h.Mgr.LoadExtension(ctx, in.Body.Extension)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Message = "loaded extension " + name
	return out, nil
}

// UnloadExtension stops an extension, taking every model it provides with it.
// In-flight requests drain rather than being killed, so a reply of "draining"
// means the process is still up and will go when the last one finishes.
func (h *Handlers) UnloadExtension(_ context.Context, in *ExtensionActionInput) (*ExtensionActionOutput, error) {
	out := &ExtensionActionOutput{}
	n, err := h.Mgr.UnloadExtension(in.Body.Extension)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Evicted = n
	if n == 0 {
		for _, st := range h.Mgr.ExtensionStates() {
			if st.Name == in.Body.Extension && st.Draining {
				out.Body.Draining = true
				out.Body.Message = fmt.Sprintf("draining %s (%d in flight); it stops when they finish", st.Name, st.InFlight)
				return out, nil
			}
		}
		out.Body.Message = "extension not resident"
		return out, nil
	}
	out.Body.Message = "stopped extension " + in.Body.Extension
	return out, nil
}

// Extensions lists declared extensions and whether each process is up.
func (h *Handlers) Extensions(_ context.Context, _ *struct{}) (*ExtensionsOutput, error) {
	out := &ExtensionsOutput{}
	out.Body.Extensions = h.Mgr.ExtensionStates()
	return out, nil
}

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// config returns the newest config this Handlers knows: the one SetConfig
// installed, else the one it was constructed with. Read through this, never
// h.Cfg directly, or a reload goes unnoticed here.
// localDevicePools maps a card's UUID to the pool that budgets it, across every
// LOCAL server — the machines whose cards this process's probe can actually see.
//
// Agent-backed servers are excluded on purpose. Their pools describe hardware in
// another box, and matching one of their selectors against a card in THIS one
// would draw a remote budget under a local device reading.
//
// Absent from the map means no pool claims that card: it is in the machine and
// nothing budgets it, which the dashboard should show as exactly that rather
// than hiding.
func (h *Handlers) localDevicePools(devs []gpu.Stats) map[string]string {
	out := map[string]string{}
	cfg := h.config()
	if cfg == nil {
		return out
	}
	for _, srv := range cfg.Servers {
		if srv.Agent != nil {
			continue
		}
		for pool, sel := range srv.Devices {
			st, err := gpu.Select(devs, sel)
			if err != nil {
				continue
			}
			out[st.UUID] = pool
		}
	}
	return out
}

// Config is the live config: the reloaded one when there is one, otherwise the
// one this Handlers was constructed with. Exported for the reload path, which
// needs the OUTGOING config to carry its runtime overlay onto the incoming one.
func (h *Handlers) Config() *config.Config { return h.config() }

func (h *Handlers) config() *config.Config {
	if c := h.live.Load(); c != nil {
		return c
	}
	return h.Cfg
}

// SetConfig installs a reloaded config for every subsequent read.
func (h *Handlers) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	h.live.Store(cfg)
}

// --- cancel an in-flight request ---

// CancelRequestInput selects one live request by its process-local id.
type CancelRequestInput struct {
	Body struct {
		ID int64 `json:"id" doc:"Live request id, from activeRequests."`
	}
}

// CancelRequestOutput reports whether the request was found and cancelled.
type CancelRequestOutput struct {
	Body struct {
		OK      bool   `json:"ok" doc:"Whether a live request with that id was cancelled."`
		Message string `json:"message" doc:"Human-readable result."`
	}
}

// CancelRequest aborts one in-flight request.
//
// The operator's only way to stop work already running. Everything else that
// ends a request needs the CLIENT to go away, and that is not always
// observable: with an edge proxy in front, corrallm's peer is the proxy, which
// holds the upstream connection open long after the caller behind it is gone.
// A greedy decode with no token cap then runs for tens of minutes against a GPU
// nobody is waiting on, and until now nothing could stop it.
func (h *Handlers) CancelRequest(_ context.Context, in *CancelRequestInput) (*CancelRequestOutput, error) {
	out := &CancelRequestOutput{}
	if h.Proxy == nil {
		out.Body.Message = "proxy unavailable"
		return out, nil
	}
	if h.Proxy.CancelInflight(in.Body.ID) {
		out.Body.OK = true
		out.Body.Message = fmt.Sprintf("cancelled request %d", in.Body.ID)
		return out, nil
	}
	// Not an error: a request that finished on its own between the operator
	// reading the list and clicking is the common case, and it is the outcome
	// they wanted anyway.
	out.Body.Message = fmt.Sprintf("no live request with id %d (it may have just finished)", in.Body.ID)
	return out, nil
}
