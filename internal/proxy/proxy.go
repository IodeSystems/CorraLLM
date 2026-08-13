// Package proxy is corrallm's OpenAI-compatible passthrough. It resolves the
// served model and caller group from a request, acquires a fairshare slot
// (sched), ensures the backend is ready (proc), reverse-proxies to it, and logs
// the request. Saturation yields 429 + informative backoff.
//
// It routes a served model to its FIRST backend only; ordered-list fall-through
// across types is P3 — this package is the request edge those phases wrap.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/freeroster"
	"github.com/iodesystems/corrallm/internal/metrics"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// Proxy is the inference edge handler.
type Proxy struct {
	// cfg is swapped wholesale on reload; read it through p.config(), never as a
	// bare field, or the atomic is defeated. See proc.Manager.cfg.
	cfg    atomic.Pointer[config.Config]
	mgr    *proc.Manager
	sched  *sched.Scheduler
	store  *store.Store
	cost   *cost.Model
	events *events.Broker // optional: live UI events (P8-beyond)

	started int64 // unix seconds at construction — the catalog's "created"

	requestTimeout     time.Duration        // 0 = no corrallm-imposed deadline (defer to client + backend)
	capturePayloads    bool                 // capture req/resp payloads onto the activity row (P10b)
	realtimeIdle       time.Duration        // 0 = no idle reap of a realtime ws session (P9e)
	realtimeMaxSession time.Duration        // 0 = no max-duration reap of a realtime ws session (P9e)
	convertEnabled     bool                 // master switch for chat attachment ingestion (P13)
	convertGlobal      config.ConvertConfig // global default; per-model `convert:` overrides it

	rrMu sync.Mutex
	rr   map[string]uint64 // per-served-model round-robin counter

	// inflight is the live-request registry (see inflight.go): what the box is
	// doing right now, as opposed to the activity log's finished requests.
	inflightMu  sync.Mutex
	inflight    map[int64]*inflightEntry
	inflightSeq int64

	// quota is the P16 free-tier budget ledger: it learns each remote backend's
	// remaining rate-limit budget from the X-Ratelimit-* headers on its responses.
	quota *quota.Ledger
	// roster holds each provider's currently-free model set (P16e), refreshed
	// periodically so a churned-out free model is skipped proactively.
	roster *freeroster.Roster
}

// RosterSnapshot returns each provider's currently-free model roster (P16e).
func (p *Proxy) RosterSnapshot() []freeroster.ProviderView {
	if p.roster == nil {
		return nil
	}
	return p.roster.Snapshot()
}

// HasRosterRefresh reports whether any backend opted into roster refresh, so the
// poller is only started when it has work.
func (p *Proxy) HasRosterRefresh() bool {
	for _, m := range p.config().Models {
		if m.FreeTier != nil && m.FreeTier.Refresh {
			return true
		}
	}
	// Discovery rides the same loop. Checking only declared models missed the
	// case where a provider contributes EVERY model by discovery: with nothing
	// declared there was nothing to refresh, so the loop never started and the
	// provider silently served nothing.
	return len(p.config().DiscoverTargets()) > 0
}

// RefreshRoster does one refresh pass: for each refresh-opted backend, pull its
// provider's /v1/models, record the free set, and mark the backend stale if its
// own model has churned out of it (so the selector routes around it). A fetch
// error leaves the prior roster and staleness untouched — a transient failure
// must not strand every free model.
func (p *Proxy) RefreshRoster(ctx context.Context) {
	if p.roster == nil {
		return
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	for name, m := range p.config().Models {
		if m.FreeTier == nil || !m.FreeTier.Refresh {
			continue
		}
		tgt, err := m.ProxyTarget()
		if err != nil {
			continue
		}
		provider := m.FreeTier.Provider
		if provider == "" {
			provider = name
		}
		modelsURL := strings.TrimRight(tgt.URL.String(), "/") + tgt.BasePath + "/v1/models"
		free, ferr := freeroster.FetchFree(ctx, hc, modelsURL, tgt.Headers)
		p.roster.Set(provider, free, ferr, time.Now())
		if ferr != nil {
			slog.Warn("roster refresh failed", "provider", provider, "err", ferr)
			continue
		}
		isFree, known := p.roster.Has(provider, tgt.Model)
		if known && !isFree {
			p.quota.SetStale(name, true)
			slog.Warn("free model churned out of roster — marking stale", "backend", name, "model", tgt.Model)
		} else {
			p.quota.SetStale(name, false)
		}
	}
	p.refreshDiscovery(ctx, hc)
}

// refreshDiscovery pulls each discover-opted provider's catalog and contributes
// the rows that pass its filter. A fetch error leaves the previous set in place:
// a transient outage must not deregister every model a provider contributed.
func (p *Proxy) refreshDiscovery(ctx context.Context, hc *http.Client) {
	for _, dt := range p.config().DiscoverTargets() {
		modelsURL := strings.TrimRight(dt.Target.URL.String(), "/") + dt.Target.BasePath + "/v1/models"
		cat, err := freeroster.FetchCatalog(ctx, hc, modelsURL, dt.Target.Headers)
		if err != nil {
			slog.Warn("discovery fetch failed", "extension", dt.Extension, "provider", dt.Provider, "err", err)
			continue
		}
		kept := selectDiscovered(cat, dt)
		p.config().SetDiscovered(dt.Provider, kept)
		slog.Info("discovered models", "extension", dt.Extension, "provider", dt.Provider,
			"kept", len(kept), "of", len(cat))
	}
}

// selectDiscovered applies a provider's filter and template to its catalog. Pure
// and deterministic, so it is unit-tested without any network.
func selectDiscovered(cat []freeroster.Entry, dt config.DiscoverTarget) map[string]config.Model {
	f := dt.Spec.Filter
	pass := make([]freeroster.Entry, 0, len(cat))
entries:
	for _, e := range cat {
		if f.Free && !e.Free {
			continue
		}
		if f.InputModality != "" && e.InputModality != f.InputModality {
			continue
		}
		if f.OutputModality != "" && e.OutputModality != f.OutputModality {
			continue
		}
		if f.MinContext > 0 && e.ContextLength < f.MinContext {
			continue
		}
		for _, x := range f.Exclude {
			if x != "" && strings.Contains(e.ID, x) {
				continue entries
			}
		}
		pass = append(pass, e)
	}
	// Largest context first, so a Limit keeps the most useful models and the
	// result is stable across refreshes regardless of the provider's ordering.
	sort.SliceStable(pass, func(i, j int) bool {
		if pass[i].ContextLength != pass[j].ContextLength {
			return pass[i].ContextLength > pass[j].ContextLength
		}
		return pass[i].ID < pass[j].ID
	})
	if dt.Spec.Limit > 0 && len(pass) > dt.Spec.Limit {
		pass = pass[:dt.Spec.Limit]
	}
	out := make(map[string]config.Model, len(pass))
	for _, e := range pass {
		m := dt.Spec.Template // value copy: the template is never mutated
		m.Extension, m.ProviderName = dt.Extension, dt.Provider
		m.Proxy = dt.ProxyNode
		m.Upstream = e.ID // the provider's own id, never the served name
		out[config.ServedName(dt.Provider, e.ID)] = m
	}
	return out
}

// QuotaSnapshot returns the current free-tier budget ledger (P16), for the
// observability API. Empty until a remote backend has been called.
func (p *Proxy) QuotaSnapshot() []quota.Entry {
	if p.quota == nil {
		return nil
	}
	return p.quota.Snapshot()
}

// New constructs a Proxy.
func New(cfg *config.Config, mgr *proc.Manager, sc *sched.Scheduler, st *store.Store) *Proxy {
	p := &Proxy{mgr: mgr, sched: sc, store: st, cost: cost.NewModel(cfg),
		started: time.Now().Unix(), rr: map[string]uint64{}, capturePayloads: true,
		convertEnabled: true, convertGlobal: config.DefaultConvert(),
		quota: quota.New(), roster: freeroster.New()}
	p.cfg.Store(cfg)
	// Attach durable storage for the falloff counters BEFORE seeding limits, so a
	// counter-mode window resumes its persisted level across a restart instead of
	// resetting to zero and over-sending against the provider's real daily cap.
	if st != nil {
		p.quota.UseStore(quotaCounterStore{st})
	}
	// Seed the ledger from each free-tier backend's config (P16): a self-cap for
	// header-tracked backends, and the provider limits for counter-mode ones (no
	// rate-limit headers, so budget is counted locally).
	for name, m := range cfg.Models {
		if m.FreeTier == nil {
			continue
		}
		if m.FreeTier.Cap.Requests > 0 || m.FreeTier.Cap.Tokens > 0 {
			p.quota.SetCap(name, m.FreeTier.Cap.Requests, m.FreeTier.Cap.Tokens)
		}
		if m.FreeTier.Limits.RPM > 0 || m.FreeTier.Limits.RPD > 0 {
			p.quota.SetLimits(name, m.FreeTier.Limits.RPM, m.FreeTier.Limits.RPD)
		}
	}
	return p
}

// quotaCounterStore adapts *store.Store to quota.CounterStore, converting the
// unix-millis timestamps the store persists to the time.Time the ledger uses.
type quotaCounterStore struct{ st *store.Store }

func (q quotaCounterStore) SaveQuotaCounter(backend, label string, used float64, atMS int64) error {
	return q.st.SaveQuotaCounter(backend, label, used, atMS)
}

func (q quotaCounterStore) LoadQuotaCounters() ([]quota.PersistedCounter, error) {
	rows, err := q.st.LoadQuotaCounters()
	if err != nil {
		return nil, err
	}
	out := make([]quota.PersistedCounter, len(rows))
	for i, r := range rows {
		out[i] = quota.PersistedCounter{Backend: r.Backend, Label: r.Label, Used: r.Used, At: time.UnixMilli(r.AtMS)}
	}
	return out, nil
}

// payloadCap bounds a captured RESPONSE payload (P10b) — enough to see a reply
// head or an error; binary audio is summarized to a size, never stored raw.
const payloadCap = 4 << 10 // 4 KiB

// reqBodyCap bounds a captured REQUEST payload. Much larger than payloadCap so a
// full agentic request (system + tool schemas + multi-turn history + tool
// results) stays VALID JSON and can be replayed in the console — a 4 KiB
// truncation left it unparseable, so replay fell back to dumping raw text. A
// request over this cap still truncates (replay then degrades to raw), so the
// cap is generous. Env override: CORRALLM_REQBODY_CAP (bytes).
var reqBodyCap = envInt("CORRALLM_REQBODY_CAP", 256<<10) // 256 KiB

// envInt reads a positive integer env var, else the default.
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// SetCapturePayloads toggles per-request payload capture (P10b). Payloads are
// admin-gated and pruned with the activity log, but they are user data — disable
// this where prompts must not be persisted.
func (p *Proxy) SetCapturePayloads(on bool) { p.capturePayloads = on }

// SetConvert configures chat attachment ingestion (P13): `enabled` is the master
// switch; `global` is the default ConvertConfig (built from flags + the config's
// top-level `convert:`), which a model's own `convert:` block overrides per field.
func (p *Proxy) SetConvert(enabled bool, global config.ConvertConfig) {
	p.convertEnabled = enabled
	p.convertGlobal = global
}

// capturePayload renders a payload for storage: binary bodies become a
// "<content-type, N bytes>" summary (never stored raw); text is truncated to
// `cap` bytes. Returns "" when capture is disabled.
func (p *Proxy) capturePayload(data []byte, binary bool, contentType string, cap int) string {
	if !p.capturePayloads {
		return ""
	}
	if binary {
		ct, _, _ := strings.Cut(contentType, ";")
		if ct = strings.TrimSpace(ct); ct == "" {
			ct = "binary"
		}
		return fmt.Sprintf("<%s, %d bytes>", ct, len(data))
	}
	if len(data) > cap {
		return fmt.Sprintf("%s…(+%d bytes truncated)", data[:cap], len(data)-cap)
	}
	return string(data)
}

// SetBroker attaches an events broker so the request path can push live updates
// (a new activity record, a "state changed" ping). Optional; nil disables it.
func (p *Proxy) SetBroker(b *events.Broker) { p.events = b }

// SetRequestTimeout sets the max wall-clock corrallm allows one proxied request
// before it cancels the upstream (logged 504). 0 (default) imposes NO corrallm
// deadline — the request lives as long as the client holds the connection and the
// backend keeps it open (the backend's own timeout governs). A short cap here
// turns long-but-valid requests (big prompts, image data) into spurious failures,
// so prefer 0 unless you specifically want a ceiling.
func (p *Proxy) SetRequestTimeout(d time.Duration) { p.requestTimeout = d }

// SetRealtimeTimeouts bounds realtime (/v1/realtime) WebSocket sessions (P9e):
// idle reaps a session after that long with no bytes either way; maxSession caps
// total duration. Either 0 disables that check. A reaped session frees its slot
// and logs 408 with the reason.
func (p *Proxy) SetRealtimeTimeouts(idle, maxSession time.Duration) {
	p.realtimeIdle, p.realtimeMaxSession = idle, maxSession
}

// Mount registers the OpenAI-compatible inference routes plus the untracked
// non-inference passthrough on r. The route set mirrors the OpenAI surface
// corrallm fronts (chat/completions, completions, embeddings, rerank, audio,
// models). The audio routes (P9a) carry a multipart/form-data body whose model
// is a form field, not JSON — handleInference forks on content-type.
func (p *Proxy) Mount(mux interface {
	Handle(pattern string, h http.Handler)
}) {
	for _, path := range []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/rerank",
		"/v1/audio/transcriptions", // STT (parakeet); multipart in, JSON/SSE out
		"/v1/audio/translations",   // STT → English; same shape
		"/v1/audio/speech",         // TTS (kokoro); JSON in, binary audio out
	} {
		mux.Handle(path, http.HandlerFunc(p.handleInference))
	}
	// /v1/realtime (P9e): live transcription over WebSocket. A SEPARATE edge from
	// handleInference — it must NOT buffer the body; it upgrades the connection and
	// proxies bytes both ways for the session's lifetime. Model comes from ?model=.
	mux.Handle("/v1/realtime", http.HandlerFunc(p.handleRealtime))
	// /v1/models is a catalog response synthesized from config, not proxied.
	mux.Handle("/v1/models", http.HandlerFunc(p.handleModels))
	// /v1/messages/count_tokens (Anthropic's shape) is metadata, not inference —
	// mounted outside handleInference so it never takes an admission slot. See
	// handleCountTokens.
	mux.Handle("/v1/messages/count_tokens", http.HandlerFunc(p.handleCountTokens))
	// /v1/capabilities is a public, self-describing manifest (endpoints + models by
	// capability + lanes + examples) — point an LLM/client at it to build a
	// compatible client. Synthesized from config; never exposes API keys.
	mux.Handle("/v1/capabilities", http.HandlerFunc(p.handleCapabilities))
	// /v1/reservations lets a keyed caller lease slots on a model for its lane so
	// interactive work has headroom against saturating batch. Short, renewable,
	// auto-expiring. POST create/renew, DELETE release, GET list.
	mux.Handle("/v1/reservations", http.HandlerFunc(p.handleReservations))
	// Non-inference UI/passthrough: /upstream/<model>/… serves UNTRACKED once
	// the backend is up — it must not consume admission/concurrency (the
	// gatedPaths lesson, structural here). No activity log, no scheduling.
	// Wildcard so chi matches the whole subtree.
	mux.Handle("/upstream/*", http.HandlerFunc(p.handleUpstream))
}

// handleInference resolves the served model from the JSON body's "model" field,
// ensures its first backend is ready, and reverse-proxies the (buffered) body.
func (p *Proxy) handleInference(w http.ResponseWriter, r *http.Request) {
	// Audio routes (P9a/P9b). STT (transcriptions/translations) takes a multipart
	// upload — raise the body cap for the audio file (parakeet caps it at 25 MiB).
	// TTS (speech) is JSON-in/binary-out, so it keeps the default cap but still
	// meters as audio (by output bytes — see below).
	audio := strings.HasPrefix(r.URL.Path, "/v1/audio/")
	tts := r.URL.Path == "/v1/audio/speech"
	maxBody := int64(32 << 20)
	if audio && !tts {
		maxBody = 64 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// Resolve the served model + stream flag. JSON bodies carry both as top-level
	// fields; multipart audio carries them as form fields. The buffered body is
	// replayed to the upstream intact either way.
	served, streaming := resolveRequest(r, body)
	if served == "" {
		http.Error(w, `{"error":{"message":"missing \"model\""}}`, http.StatusBadRequest)
		return
	}
	cands, ok := p.config().ResolveServed(served)
	if !ok {
		http.Error(w, `{"error":{"message":"unknown model \"`+served+`\""}}`, http.StatusNotFound)
		return
	}

	start := time.Now()
	// Capture the request payload once (available on every exit path). STT uploads
	// are multipart/binary → summarized to a size, not stored raw. Captured BEFORE
	// PDF conversion so the activity row holds the (small) original, not the
	// document text injected below.
	reqBody := p.capturePayload(body, audio && !tts, r.Header.Get("Content-Type"), reqBodyCap)

	// PDF auto-conversion (P13): a text model can't read an attached PDF, so replace
	// any PDF content part in a chat request with its extracted text. Done once here
	// (not per backend in the loop); a no-op when there are no PDFs.
	if p.convertEnabled && r.URL.Path == "/v1/chat/completions" {
		// Resolve the per-model ingestion config (global default ← model's override).
		eff := p.config().ConvertFor(p.convertGlobal, served)
		if nb, n := convertChatPDFs(r.Context(), body, eff); n > 0 {
			body = nb
		}
	}
	// Only impose a deadline when one is configured. A fixed cap here would turn
	// long-but-valid requests (big prompts, image data on a 27B/220k-ctx model)
	// into spurious timeouts — the regression that surfaced as 502s in production.
	ctx := r.Context()
	cancel := func() {}
	if p.requestTimeout > 0 {
		ctx, cancel = context.WithTimeout(r.Context(), p.requestTimeout)
	}
	defer cancel()

	key := callerKey(r)
	groupName, group, recognized := p.config().ResolveGroupRecognized(key)
	if !recognized && !p.config().UnknownKeys.Allowed() {
		// Refusing a stranger is a POLICY, off by default: corrallm has always
		// served any key, and an operator who turns this on is choosing to.
		// 401 rather than 429 — this is not backpressure and no amount of
		// retrying fixes it; the caller needs an operator, and the message
		// says so rather than leaving them to retry into a wall.
		http.Error(w, "unrecognized caller key: this corrallm requires keys to be "+
			"assigned a priority group before it will serve them", http.StatusUnauthorized)
		return
	}
	weight := group.EffectiveWeight()

	// A cancellable context wrapping the request's own, so an operator can abort
	// this request from ANY state. Derived here rather than at admission
	// because a request wedged in a queue has no slot and no backend to be
	// addressed by, and is exactly the one worth being able to kill.
	ctx, cancelReq := context.WithCancelCause(ctx)
	defer cancelReq(nil)

	// Register as live BEFORE admission: a request queued behind a saturated
	// backend, or waiting out a cold load, is precisely the one you want to see
	// on the dashboard — and it never reaches the activity log until it ends.
	retryable := retryableRequest(r)
	live := p.beginInflight(r, served, groupName, key, streaming, retryable, cancelReq)
	defer p.endInflight(live)

	// Walk the served name's candidates in order (a lane's members, or the one
	// pinned model; rr within a cost-equivalent `type`, ordered across types).
	// For each: take a slot or honor the group's saturation stage for that type —
	// spill/fallThrough advances to the next candidate; queue waits; reject is
	// terminal. A candidate that won't become ready also spills.
	// Quality-degrade fall-through (P7): walk best-quality-first, keeping only
	// the tiers this group accepts. A non-degrading group sees only the top tier,
	// so saturation there backs off (per its stage) instead of spilling onto a
	// worse model; a degrading group walks down to its floor.
	// P16 quota-aware selection, applied BEFORE quality gating: drop free-tier
	// backends the ledger knows are exhausted/cooling from the candidate set, so
	// an out-of-budget top-quality remote does not pin the quality ceiling and
	// shut out a lower local floor. (A late filter could not fix this — the
	// quality gate below would already have excluded the floor.) Locals are
	// always Available; if every candidate is filtered out, all are kept so a
	// free-only lane tries for the real 429 over a blind 503.
	// P16 privacy tiering: a request marked sensitive (X-Corrallm-Sensitive) may
	// only reach backends safe for its data — local models (own box) and remotes
	// flagged freeTier.private (contractually don't train). Applied before quality
	// gating for the same reason as the quota filter, and with NO keep-all
	// fallback: a sensitive request must refuse rather than leak, so an empty
	// result is a real "no privacy-safe backend" answer, not a reason to relax.
	// Paused models are out of service by operator order — the most absolute of
	// the filters, so it runs first. A lane falls through to its unpaused
	// members; a lane (or a directly-named model) with nothing left is a 503,
	// not backpressure: a pause is a decision, not congestion, and there is no
	// retry interval that would be honest for one.
	if kept := p.filterByPaused(cands); len(kept) != len(cands) {
		if len(kept) == 0 {
			// cands, NOT kept: the pauses that produced this 503 are the ones
			// on the candidates just filtered OUT, so the resume time has to be
			// read before the list is narrowed.
			p.writePaused(w, r, served, key, cands, start, reqBody, "model is paused")
			return
		}
		cands = kept
	}
	if isSensitive(r) {
		cands = filterBySensitive(cands)
		if len(cands) == 0 {
			http.Error(w, "no privacy-safe backend available for a sensitive request", http.StatusServiceUnavailable)
			p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
				Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
				Error: "no private backend for sensitive request", ReqBody: reqBody})
			return
		}
	}
	cands = p.filterByQuota(cands)
	topQuality := config.MaxQuality(cands)
	ordered := orderCandidates(cands, p.nextRR(served))
	walk := ordered[:0:0]
	for _, idx := range ordered {
		if group.AcceptsQuality(cands[idx].Model.Quality, topQuality) {
			walk = append(walk, idx)
		}
	}
	// preferResident (best-effort-for-what's-loaded): float already-warm backends
	// to the front of the walk, keeping quality order within each partition. The
	// loop below then serves on a resident backend (EnsureReady returns loaded)
	// without cold-loading a bigger tier; only if none is resident does it fall to
	// the normal quality-first cold-load ladder. Lets a latency lane ride whatever
	// chat model is hot instead of re-hogging the box.
	if group.PreferResident {
		walk = partitionResident(walk, cands, p.residentBackends())
	}
	// bestBP is the most OPTIMISTIC backpressure seen while walking candidates —
	// the smallest Retry-After, i.e. the soonest anything in the lane could
	// serve. Keeping the last one instead would report whichever backend
	// happened to be tried last, which is arbitrary: a saturated-but-live model
	// that frees a slot in 2s is a better answer than a cold one 30s from
	// resident. Only if EVERY candidate is permanently unusable do we 503.
	var bestBP *sched.BackpressureError
	// Cumulative across the spill walk, not just the terminal backend.
	//
	// These were per-candidate assignments, which under-reported whenever a
	// request queued on one backend, spilled, and queued again: only the last
	// wait survived. That is wrong in the activity log and actively misleading
	// in the headers below, where a client subtracts them from its own wall
	// clock to recover execution time — under-reporting the overhead inflates
	// the execution time it computes.
	var queuedMS int64 // time blocked in admission control
	var loadMS int64   // time waiting for a backend to be spawned/resident

	for _, idx := range walk {
		cand := cands[idx]
		backend := cand.Model
		name := cand.Name
		stage := group.StageFor(backend.Type)

		admitStart := time.Now()
		// group.Interruptible OR the request's own opt-in. A caller can only
		// widen this, never narrow it — see retryableRequest.
		release, reqCtx, err := p.sched.Admit(ctx, name, backend.Type, backend.Slots(), groupName, weight, group.Interruptible || retryable, stage)
		queuedMS += time.Since(admitStart).Milliseconds() // ~0 unless this stage queued
		if err == nil {
			// A slot was taken — lanes load changed. markInflight publishes, so
			// the two live views (lanes + active requests) refresh together.
			p.markInflight(live, inflightLoading, name)
		}
		if err != nil {
			var bp *sched.BackpressureError
			if errors.As(err, &bp) {
				if bp.Reason == "spill" {
					bestBP = keepSoonest(bestBP, bp)
					continue // advance to the next backend
				}
				// rejected or queue-timeout → terminal backoff.
				promised := writeBackpressure(w, bp)
				p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
					Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
					QueuedMS: queuedMS, LoadMS: loadMS, Error: bp.Reason, ReqBody: reqBody,
					RetryAfterMS: promised})
				return
			}
			p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
				Status: 499, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS, LoadMS: loadMS,
				Error: "client canceled", ReqBody: reqBody}) // queued then client gave up
			return
		}

		// Free the admission slot on EVERY exit path, panic included.
		// httputil.ReverseProxy raises http.ErrAbortHandler when the
		// client disconnects mid-response (a ragtag request hitting its
		// timeout); net/http recovers that panic SILENTLY, which skipped
		// the explicit release() below and leaked the slot — bs.active
		// stuck at capacity, every later request queue-timing-out forever
		// against an idle backend. releaser is sync.Once, so the explicit
		// release(Done{cost}) on the success path still records cost and
		// this deferred call is then a no-op.
		defer release()

		// Proxy under reqCtx so a later preemption (cause ErrPreempted) aborts the
		// upstream stream and frees this slot.
		loadStart := time.Now()
		pr, done, loaded, err := p.mgr.EnsureReady(reqCtx, name, backend, cand.Sticky)
		// WHERE this was served. A model can be placed on more than one box, so
		// the served name no longer says which machine, quantisation or context
		// window handled the request — and a latency figure that could have come
		// from either of two placements is not a measurement of anything.
		placement := pr.Placement()
		loadMS += time.Since(loadStart).Milliseconds() // ~0 when already resident
		if err != nil {
			release()
			// Doesn't fit + can't evict, or won't come up → spill to next backend.
			// A TRANSIENT capacity miss (a resident is inside its protection
			// window and about to become evictable) is backpressure, not a
			// fault: record it so an exhausted walk answers 429 + Retry-After
			// rather than a bare 503. Permanent misses (won't fit even fully
			// evicted) and spawn failures record nothing and stay 503.
			var ce *proc.CapacityError
			if errors.As(err, &ce) && !ce.Permanent {
				bestBP = keepSoonest(bestBP, &sched.BackpressureError{
					Reason:     "capacity",
					RetryAfter: ce.RetryAfter,
				})
			}
			slog.Warn("backend unavailable, spilling", "backend", name, "err", err)
			p.markInflight(live, inflightQueued, "") // back to the walk, no backend held
			continue
		}
		// Drop the residency ref on EVERY exit path, panic included — the same
		// ErrAbortHandler lesson as the admission slot above: a client abort
		// mid-stream panics out of ServeHTTP, net/http recovers it silently, and
		// the inline done() below never runs. A leaked ref makes the model
		// permanently unevictable (observed: refs=1 450s after last use, starving
		// every other model with ErrNoCapacity). done is sync.Once-guarded, so
		// the inline call on the success path stays a cheap no-op here.
		defer done()

		// Restore the buffered body for the proxy, clamping max_tokens to this
		// backend's cap when it declares one (degrade transform, P7).
		outBody := clampMaxTokens(body, backend)
		// Rewrite the body's `model` to the upstream's own id for a remote that
		// declares one (P16): corrallm routed on the served name, but the remote
		// does not know it. A local backend leaves Target.Model empty and the
		// body forwards unchanged.
		upstream := upstreamID(backend, pr)
		if upstream != "" {
			// JSON bodies (chat/embeddings) and multipart bodies (audio) carry the
			// model in different places, and rewriting only the former silently
			// forwarded the served name upstream on every audio request.
			if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
				if nb, nct, ok := rewriteModelMultipart(outBody, ct, upstream); ok {
					outBody = nb
					r.Header.Set("Content-Type", nct)
				}
			} else {
				outBody = rewriteModelField(outBody, upstream)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(outBody))
		r.ContentLength = int64(len(outBody))
		p.markInflight(live, inflightStreaming, name)
		sc := &statusCapture{ResponseWriter: w, code: http.StatusOK, streaming: streaming, live: live}
		// Capture the proxy error so the activity log can say WHY a request failed
		// (P10a) and map it to an honest status: a canceled connection (client or an
		// upstream front-proxy giving up) is 499, not a backend 502; corrallm's own
		// deadline is 504; a genuine backend dial/transport error stays 502.
		rp := newReverseProxy(pr.Target)
		// Fold this response's rate-limit headers into the quota ledger (P16).
		// A no-op for local backends (no such headers); learns a remote's
		// remaining budget for the selector to route on. On a HARD failure from a
		// free-tier remote (401/402/403 — auth or billing, which a retry won't fix)
		// take it out of rotation and abort with errBackendDown so the loop spills
		// to the next candidate rather than returning the error to the caller.
		isFree := backend.FreeTier != nil
		hardFailStatus := 0
		upstreamStart := time.Now()
		rp.ModifyResponse = func(resp *http.Response) error {
			// Timing breakdown, so a caller can recover EXECUTION time from its
			// own wall clock. A benchmark measuring a busy box otherwise reports
			// scheduler queueing and cold loads as if the model were slow — the
			// numbers move with the neighbours, not the model.
			//
			// Set here rather than after the body: ModifyResponse runs before
			// anything reaches the client, which is the last point a streaming
			// response can still take headers. Total execution is therefore not
			// available (the body has not been streamed yet) and is deliberately
			// not offered — the client subtracts these from its own total, which
			// works for streaming and non-streaming alike.
			setTimingHeaders(resp.Header, queuedMS, loadMS, time.Since(upstreamStart).Milliseconds())
			p.quota.ObserveResponse(name, resp.StatusCode, resp.Header)
			if isFree && isHardFail(resp.StatusCode) {
				hardFailStatus = resp.StatusCode
				p.quota.MarkDown(name) // exponential backoff lives in the ledger
				return errBackendDown
			}
			return nil
		}
		var proxyErr error
		rp.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, err error) {
			proxyErr = err
			if errors.Is(err, errBackendDown) {
				return // spill: write nothing, the walk loop retries the next candidate
			}
			code := http.StatusBadGateway
			switch {
			case errors.Is(err, context.Canceled):
				code = 499
			case errors.Is(err, context.DeadlineExceeded):
				code = http.StatusGatewayTimeout
			}
			rw.WriteHeader(code)
		}
		rp.ServeHTTP(sc, r.WithContext(reqCtx))
		done()
		// Hard-fail spill: the response was aborted before anything reached the
		// client (wroteHeader guards that), so free the slot and try the next
		// candidate — a free-only lane with one broken key still serves from
		// another backend instead of surfacing a 402/403.
		if errors.Is(proxyErr, errBackendDown) && !sc.wroteHeader {
			release()
			slog.Warn("free-tier backend hard-failed, spilling", "backend", name, "status", hardFailStatus)
			p.markInflight(live, inflightQueued, "")
			continue
		}
		status := sc.code
		errReason := ""
		if proxyErr != nil {
			errReason = proxyErr.Error()
		}
		if errors.Is(context.Cause(reqCtx), sched.ErrPreempted) {
			status, errReason = 499, "preempted" // slot reclaimed by a higher-priority group mid-request
		}
		// Meter the served request and resolve it to $ via the backend's cost
		// class. Audio routes carry no token usage, so they cost by byte size
		// (P9c/P9b, file-bytes basis): STT bills the uploaded INPUT audio; TTS
		// bills the synthesized OUTPUT audio (its JSON input is tiny). Text routes
		// extract token usage from the response. A cold load triggered by this
		// request also bills its swap energy to it. The cost is reported to the
		// scheduler (limit budgets + cost share currency) at release.
		var u usage
		var finishReason string
		var costUSD float64
		var audioBytes int64
		var cCost, pCost, gCost float64 // per-class $ (cached, processed, generated)
		switch {
		case tts:
			audioBytes = sc.written
			costUSD = p.cost.AudioRequestUSD(backend.Type, int(sc.written))
		case audio:
			audioBytes = int64(len(body))
			costUSD = p.cost.AudioRequestUSD(backend.Type, len(body))
		default:
			u = extractUsage(sc.buf, streaming)
			finishReason = extractFinishReason(sc.buf, streaming)
			proc := u.PromptTokens - u.CachedTokens
			if proc < 0 {
				proc = 0
			}
			cCost, pCost, gCost = p.cost.RequestUSDByClass(backend.Type, u.CachedTokens, proc, u.CompletionTokens)
			// Provider-reported cost wins when present (OpenRouter): treat it as
			// the authoritative total, keeping the table-derived class ratio (or a
			// token share when the type is unpriced).
			if u.Cost > 0 {
				if s := cCost + pCost + gCost; s > 0 {
					k := u.Cost / s
					cCost, pCost, gCost = cCost*k, pCost*k, gCost*k
				} else if tot := float64(u.PromptTokens + u.CompletionTokens); tot > 0 {
					cCost = u.Cost * float64(u.CachedTokens) / tot
					pCost = u.Cost * float64(proc) / tot
					gCost = u.Cost * float64(u.CompletionTokens) / tot
				} else {
					gCost = u.Cost
				}
			}
			costUSD = cCost + pCost + gCost
		}
		var loadCost float64
		if loaded && backend.Swap != nil {
			loadCost = p.cost.SwapUSD(backend.Swap.LoadSeconds, backend.Swap.LoadWatts)
			costUSD += loadCost
		}
		release(sched.Done{CostUSD: costUSD})

		// Prometheus: meter the served request — provider×model with per-class
		// token counts and dollar cost. Text routes split cached/processed/
		// generated; audio carries no token usage (costed by bytes). A cold-load
		// swap energy cost is reported under the "load" class.
		prov := backend.Provider()
		statusStr := strconv.Itoa(status)
		metrics.Request(prov, name, statusStr)
		if tts || audio {
			metrics.Cost(prov, name, "audio", costUSD)
		} else {
			proc := u.PromptTokens - u.CachedTokens
			if proc < 0 {
				proc = 0
			}
			metrics.Tokens(prov, name, "cached", u.CachedTokens)
			metrics.Tokens(prov, name, "processed", proc)
			metrics.Tokens(prov, name, "generated", u.CompletionTokens)
			metrics.Cost(prov, name, "cached", cCost)
			metrics.Cost(prov, name, "processed", pCost)
			metrics.Cost(prov, name, "generated", gCost)
		}
		metrics.Cost(prov, name, "load", loadCost)

		var respBody string
		if tts {
			if p.capturePayloads {
				respBody = fmt.Sprintf("<audio, %d bytes>", sc.written)
			}
		} else {
			respBody = p.capturePayload(sc.buf, false, "", payloadCap)
		}
		var ttfbMS int64
		if !sc.firstWrite.IsZero() {
			ttfbMS = sc.firstWrite.Sub(start).Milliseconds()
		}
		p.logReq(r, store.Activity{
			Served: served, Placement: placement, Key: key, Path: r.URL.Path, Status: status,
			DwellMS: time.Since(start).Milliseconds(), PromptTokens: u.PromptTokens,
			CompletionTokens: u.CompletionTokens, CachedTokens: u.CachedTokens,
			PromptPerSec: u.PromptPerSec, PredictedPerSec: u.PredictedPerSec,
			CostUSD: costUSD, QueuedMS: queuedMS, LoadMS: loadMS,
			AudioBytes: audioBytes, Error: errReason, TTFBMs: ttfbMS,
			ReqBody: reqBody, RespBody: respBody, FinishReason: finishReason,
		})
		return
	}

	// Exhausted the list without serving.
	if bestBP != nil {
		bestBP.Reason = "exhausted"
		promised := writeBackpressure(w, bestBP)
		p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
			Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
			QueuedMS: queuedMS, LoadMS: loadMS, Error: "exhausted", ReqBody: reqBody,
			RetryAfterMS: promised})
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
		Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
		QueuedMS: queuedMS, LoadMS: loadMS, Error: "no backend available", ReqBody: reqBody})
}

// handleRealtime is the live-transcription edge (P9e): a WebSocket session that
// streams audio in and transcripts out for its whole lifetime. It resolves the
// served model from the ?model= query (a continuous stream has no JSON body to
// read), admits one fairshare slot held for the session, ensures the backend is
// ready, then upgrades + byte-proxies until either side closes or the slot is
// preempted. corrallm stays a transparent pipe — VAD/chunking live in the backend
// (e.g. Speaches), device capture in the client.
func (p *Proxy) handleRealtime(w http.ResponseWriter, r *http.Request) {
	served := r.URL.Query().Get("model")
	if served == "" {
		http.Error(w, `{"error":{"message":"missing \"model\" query param"}}`, http.StatusBadRequest)
		return
	}
	cands, ok := p.config().ResolveServed(served)
	if !ok {
		http.Error(w, `{"error":{"message":"unknown model \"`+served+`\""}}`, http.StatusNotFound)
		return
	}

	// /v1/realtime carries two transports. A WebSocket upgrade (GET + Upgrade:
	// websocket) streams audio through corrallm (hijacked, fully metered). A POST
	// is the WebRTC SDP offer — corrallm only brokers signaling: it reverse-proxies
	// the handshake; the media then flows client↔backend directly (P2P), so there
	// are no audio bytes to meter here.
	ws := r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")

	start := time.Now()
	key := callerKey(r)
	// Same operator-order filter as the inference path: skip paused members so
	// the walk never queues for an admission slot on a model that will not
	// serve, and 503 when the lane has nothing left.
	if kept := p.filterByPaused(cands); len(kept) == 0 {
		p.writePaused(w, r, served, key, cands, start, "", `{"error":{"message":"model is paused"}}`)
		return
	} else {
		cands = kept
	}
	groupName, group := p.config().ResolveGroup(key)
	weight := group.EffectiveWeight()
	// A realtime session is long-lived by construction — the request that most
	// deserves to be visible while it runs, and the one an operator is most
	// likely to need to end.
	sessCtx, cancelReq := context.WithCancelCause(r.Context())
	defer cancelReq(nil)
	r = r.WithContext(sessCtx)
	live := p.beginInflight(r, served, groupName, key, true, retryableRequest(r), cancelReq)
	defer p.endInflight(live)
	topQuality := config.MaxQuality(cands)
	ordered := orderCandidates(cands, p.nextRR(served))
	var lastBP *sched.BackpressureError
	// Cumulative across the walk, same as the chat path: a session that queued
	// on one backend, spilled and queued again must report both waits.
	var queuedMS int64
	var loadMS int64

	for _, idx := range ordered {
		cand := cands[idx]
		backend := cand.Model
		if !group.AcceptsQuality(backend.Quality, topQuality) {
			continue
		}
		name := cand.Name
		stage := group.StageFor(backend.Type)

		admitStart := time.Now()
		release, reqCtx, err := p.sched.Admit(r.Context(), name, backend.Type, backend.Slots(), groupName, weight, group.Interruptible, stage)
		queuedMS += time.Since(admitStart).Milliseconds()
		if err != nil {
			var bp *sched.BackpressureError
			if errors.As(err, &bp) {
				if bp.Reason == "spill" {
					lastBP = keepSoonest(lastBP, bp)
					continue
				}
				promised := writeBackpressure(w, bp)
				p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
					Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
					QueuedMS: queuedMS, LoadMS: loadMS, Error: bp.Reason, RetryAfterMS: promised})
				return
			}
			p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
				Status: 499, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS, LoadMS: loadMS, Error: "client canceled"})
			return
		}
		p.markInflight(live, inflightLoading, name)

		loadStart := time.Now()
		pr, done, _, err := p.mgr.EnsureReady(reqCtx, name, backend, cand.Sticky)
		loadMS += time.Since(loadStart).Milliseconds()
		if err != nil {
			release()
			// Transient capacity → backpressure, same rationale as the
			// inference path above (429 + Retry-After, not a bare 503).
			var ce *proc.CapacityError
			if errors.As(err, &ce) && !ce.Permanent {
				lastBP = keepSoonest(lastBP, &sched.BackpressureError{
					Reason:     "capacity",
					RetryAfter: ce.RetryAfter,
				})
			}
			slog.Warn("realtime backend unavailable, spilling", "backend", name, "err", err)
			p.markInflight(live, inflightQueued, "")
			continue
		}
		// Same abort-panic guard as the inference path: never leak the residency
		// ref (done is sync.Once-guarded; inline calls stay no-ops).
		defer done()

		// Rewrite the model to the id the backend knows, exactly as the
		// inference path rewrites the body — realtime carries the model in the
		// QUERY STRING, and that was never rewritten. So corrallm forwarded the
		// served name to a backend that has never heard of it and every
		// extension-provided realtime model answered 404 "has no realtime
		// transcription", while the same model worked over batch STT.
		// Both transports need it: the WebSocket upgrade and the WebRTC SDP
		// POST are both proxied straight off r.URL.
		if upstream := upstreamID(backend, pr); upstream != "" {
			q := r.URL.Query()
			q.Set("model", upstream)
			r.URL.RawQuery = q.Encode()
		}

		if !ws {
			// WebRTC signaling: reverse-proxy the SDP offer→answer. The slot is held
			// only for the handshake (the P2P media session isn't tracked here); no
			// audio traverses corrallm, so AudioBytes stays 0.
			sc := &statusCapture{ResponseWriter: w, live: live}
			newReverseProxy(pr.Target).ServeHTTP(sc, r.WithContext(reqCtx))
			done()
			status := sc.code
			if status == 0 {
				status = http.StatusOK
			}
			release(sched.Done{})
			p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
				Status: status, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS, LoadMS: loadMS})
			return
		}

		// Proxy the session under reqCtx so a preemption (ErrPreempted) tears down
		// the upgraded conn. Meter the audio streamed IN (client→backend bytes).
		p.markInflight(live, inflightStreaming, name)
		inBytes, reapReason, wsErr := p.proxyWebSocket(w, r, pr.Target, reqCtx)
		done()
		status, errReason := 200, ""
		switch {
		case errors.Is(context.Cause(reqCtx), sched.ErrPreempted):
			status, errReason = 499, "preempted"
		case wsErr != nil:
			status, errReason = http.StatusBadGateway, wsErr.Error()
		case reapReason != "":
			status, errReason = http.StatusRequestTimeout, reapReason // idle / max-session reaped
		}
		costUSD := p.cost.AudioRequestUSD(backend.Type, int(inBytes))
		release(sched.Done{CostUSD: costUSD})
		p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
			Status: status, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS, LoadMS: loadMS,
			AudioBytes: inBytes, CostUSD: costUSD, Error: errReason})
		return
	}

	if lastBP != nil {
		lastBP.Reason = "exhausted"
		promised := writeBackpressure(w, lastBP)
		p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
			Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
			QueuedMS: queuedMS, LoadMS: loadMS, Error: "exhausted", RetryAfterMS: promised})
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
		Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
		QueuedMS: queuedMS, LoadMS: loadMS, Error: "no backend available"})
}

// countingWriter tallies bytes written through it (P9e session metering + idle
// detection). The counter is atomic so the reaper goroutine can read it live.
type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// proxyWebSocket completes a WebSocket upgrade against the target and copies bytes
// both ways until either side closes, ctx is canceled (preemption/shutdown), or the
// reaper trips (idle / max-session). Returns the client→backend byte count (audio
// in) and a reap reason ("" on a clean close or remote-driven end).
func (p *Proxy) proxyWebSocket(w http.ResponseWriter, r *http.Request, t *config.ProxyTarget, ctx context.Context) (int64, string, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return 0, "", errors.New("response writer is not a Hijacker")
	}
	backConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", t.URL.Host)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return 0, "", err
	}
	defer func() { _ = backConn.Close() }()

	// Forward the upgrade request to the backend (auth headers injected for remote).
	out := r.Clone(ctx)
	out.URL.Scheme, out.URL.Host, out.Host = t.URL.Scheme, t.URL.Host, t.URL.Host
	for k, v := range t.Headers {
		out.Header.Set(k, v)
	}
	if err := out.Write(backConn); err != nil {
		http.Error(w, "backend write", http.StatusBadGateway)
		return 0, "", err
	}
	backRd := bufio.NewReader(backConn)
	resp, err := http.ReadResponse(backRd, out)
	if err != nil {
		http.Error(w, "backend read", http.StatusBadGateway)
		return 0, "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// Backend declined the upgrade — relay its response verbatim and stop.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()
		return 0, "", fmt.Errorf("backend refused upgrade: %s", resp.Status)
	}

	cliConn, cliRW, err := hj.Hijack()
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = cliConn.Close() }()
	// Complete the handshake to the client (101 + the backend's upgrade headers).
	if _, err := fmt.Fprint(cliConn, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return 0, "", err
	}
	_ = resp.Header.Write(cliConn)
	if _, err := fmt.Fprint(cliConn, "\r\n"); err != nil {
		return 0, "", err
	}

	// Tear down both ends when the slot is preempted or the server shuts down.
	go func() {
		<-ctx.Done()
		_ = backConn.Close()
		_ = cliConn.Close()
	}()

	var inBytes, outBytes int64
	var reapCode int32 // 0 none · 1 idle · 2 max (atomic)
	// Reaper: close a session that goes silent (no bytes either way for
	// realtimeIdle) or runs past realtimeMaxSession, so a stuck client can't hold
	// its slot forever. Byte counts are live (countingWriter), not end-of-copy
	// totals, so the idle check sees real traffic.
	if p.realtimeIdle > 0 || p.realtimeMaxSession > 0 {
		sessionStart := time.Now()
		tick := time.Second
		if p.realtimeIdle > 0 && p.realtimeIdle < 4*time.Second {
			tick = p.realtimeIdle / 4
		}
		if tick < 20*time.Millisecond {
			tick = 20 * time.Millisecond
		}
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			tk := time.NewTicker(tick)
			defer tk.Stop()
			var lastN int64
			lastChange := sessionStart
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case now := <-tk.C:
					if p.realtimeMaxSession > 0 && now.Sub(sessionStart) > p.realtimeMaxSession {
						atomic.StoreInt32(&reapCode, 2)
						_ = backConn.Close()
						_ = cliConn.Close()
						return
					}
					n := atomic.LoadInt64(&inBytes) + atomic.LoadInt64(&outBytes)
					switch {
					case n != lastN:
						lastN, lastChange = n, now
					case p.realtimeIdle > 0 && now.Sub(lastChange) > p.realtimeIdle:
						atomic.StoreInt32(&reapCode, 1)
						_ = backConn.Close()
						_ = cliConn.Close()
						return
					}
				}
			}
		}()
	}

	errc := make(chan error, 2)
	go func() { _, e := io.Copy(countingWriter{backConn, &inBytes}, cliRW); errc <- e }()  // client→backend (audio)
	go func() { _, e := io.Copy(countingWriter{cliConn, &outBytes}, backRd); errc <- e }() // backend→client (transcripts)
	<-errc                                                                                 // first side closed → end the session
	_ = backConn.Close()                                                                   // unblock the other copy
	_ = cliConn.Close()
	<-errc // wait for both copies so the audio-in count is complete before we read it

	reason := ""
	switch atomic.LoadInt32(&reapCode) {
	case 1:
		reason = "idle timeout"
	case 2:
		reason = "max session"
	}
	return atomic.LoadInt64(&inBytes), reason, nil
}

// partitionResident stably splits walk into resident-first order: candidates
// whose model is in the resident set keep their relative (quality) order at the
// front, the rest follow in their original order. len<2 is returned as-is. The
// engine of preferResident.
// filterByQuota (P16) drops candidates the free-tier ledger knows are exhausted
// or cooling from a 429, so a lane routes to a backend WITH budget rather than
// eating the 429. Local backends are always Available (no rate-limit headers),
// so a lane with a local floor never empties. If the filter WOULD empty the walk
// (a free-only lane, all spent), the unfiltered walk is kept — trying an
// exhausted backend for its own honest error beats a blind 503.
// filterByPaused drops candidates an operator has taken out of service.
//
// EnsureReady refuses a paused model on its own, so this is not what enforces a
// pause — it is what keeps the enforcement from costing anything. Without it a
// request for a lane whose top member is paused takes an admission slot on that
// member first, and under a `queue` stage BLOCKS there waiting for a slot on a
// model that will never serve, before finally spilling. Filtering first makes
// the walk skip it outright.
//
// No keep-all fallback (unlike filterByQuota, deliberately): if every member of
// a lane is paused there is nothing honest left to try, and the empty result is
// the correct answer rather than a reason to relax the operator's order.
// pauseResumeIn reports how long until the SOONEST of these candidates' pauses
// lifts on its own, and whether any of them has a scheduled resume at all.
//
// The soonest, matching soonestRetryAfter's rule for the contention path: with
// a lane, service returns the moment ANY member does. A candidate paused
// indefinitely contributes nothing — it has no resume time to offer — but it
// does not veto a sibling that has one, so a lane holding both still answers
// with the sibling's.
//
// ok=false means "no honest interval exists", which is the original state:
// nothing paused, or everything paused indefinitely.
func (p *Proxy) pauseResumeIn(cands []config.Candidate, now time.Time) (time.Duration, bool) {
	var soonest time.Time
	for _, c := range cands {
		pz, ok := p.mgr.PauseOf(c.Name)
		if !ok || pz.ResumeAt.IsZero() {
			continue
		}
		if soonest.IsZero() || pz.ResumeAt.Before(soonest) {
			soonest = pz.ResumeAt
		}
	}
	if soonest.IsZero() {
		return 0, false
	}
	// Floor at a second: a pause in its final milliseconds would otherwise
	// advertise 0 and invite a hot retry loop, which is the behavior this
	// header exists to prevent.
	if d := soonest.Sub(now); d > time.Second {
		return d, true
	}
	return time.Second, true
}

// writePaused answers a request whose every candidate is paused.
//
// 503, not 429: a pause is an operator decision, not the caller's fault, and
// 429 would additionally be read as congestion by anything watching queue
// pressure. Retry-After is valid on a 503 and is the honest way to say "come
// back then" — but ONLY for a timed pause. An indefinite pause keeps the bare
// 503 it always had, because nothing but a human knows when it lifts.
//
// Deliberately NOT routed through writeBackpressure: that path is EWMA
// guesswork about congestion, and a scheduled resume is a fact. It also emits
// the X-RateLimit-* family, which would be a lie here — no rate limit is
// involved.
func (p *Proxy) writePaused(w http.ResponseWriter, r *http.Request, served, key string,
	cands []config.Candidate, start time.Time, reqBody, body string) {
	retryMS := int64(0)
	if d, ok := p.pauseResumeIn(cands, time.Now()); ok {
		secs := int(d.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		retryMS = int64(secs) * 1000
	}
	http.Error(w, body, http.StatusServiceUnavailable)
	p.logReq(r, store.Activity{Served: served, Key: key, Path: r.URL.Path,
		Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
		Error: "model paused", ReqBody: reqBody, RetryAfterMS: retryMS})
}

func (p *Proxy) filterByPaused(cands []config.Candidate) []config.Candidate {
	kept := make([]config.Candidate, 0, len(cands))
	for _, c := range cands {
		if !p.mgr.IsPaused(c.Name) {
			kept = append(kept, c)
		}
	}
	return kept
}

func (p *Proxy) filterByQuota(cands []config.Candidate) []config.Candidate {
	if p.quota == nil {
		return cands
	}
	kept := make([]config.Candidate, 0, len(cands))
	for _, c := range cands {
		if p.quota.Available(c.Name) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return cands
	}
	return kept
}

func partitionResident(walk []int, cands []config.Candidate, resident map[string]bool) []int {
	if len(walk) < 2 {
		return walk
	}
	warm, cold := walk[:0:0], make([]int, 0, len(walk))
	for _, idx := range walk {
		if resident[cands[idx].Name] {
			warm = append(warm, idx)
		} else {
			cold = append(cold, idx)
		}
	}
	return append(warm, cold...)
}

// residentBackends returns the set of model names that are currently warm —
// ready or mid-load. A mid-load model counts so a preferResident group
// coalesces onto an in-flight load rather than kicking off a second cold load
// of a different tier.
func (p *Proxy) residentBackends() map[string]bool {
	out := map[string]bool{}
	for _, m := range p.mgr.Snapshot().Models {
		switch proc.State(m.State) {
		case proc.StateReady, proc.StateLoading:
			out[m.Name] = true
		}
	}
	return out
}

// orderCandidates returns candidate indices in fall-through order: highest
// quality tier first, descending (the degrade ladder, P7). Within a tier, types
// appear in first-appearance order and same-type candidates rotate by rr (round
// robin across cost-equivalent peers). Uniform quality → a single tier →
// list order with type-rr (no regression for lanes that don't use quality).
func orderCandidates(cands []config.Candidate, rr uint64) []int {
	// Keyed by float now that a tier can sit between two others (1.5). Exact
	// equality is the right test here despite these being floats: a tier key
	// comes from the same parsed config value every time, not from arithmetic,
	// so two models written `quality: 1.5` produce bit-identical keys.
	tiers := map[float64][]int{}
	var qualities []float64
	for i, c := range cands {
		if _, seen := tiers[c.Model.Quality]; !seen {
			qualities = append(qualities, c.Model.Quality)
		}
		tiers[c.Model.Quality] = append(tiers[c.Model.Quality], i)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(qualities))) // best quality first
	out := make([]int, 0, len(cands))
	for _, q := range qualities {
		out = append(out, orderByTypeRR(tiers[q], cands, rr)...)
	}
	return out
}

// orderByTypeRR orders a single quality tier's indices: types in first-appearance
// order, same-type candidates rotated by rr.
func orderByTypeRR(idxs []int, cands []config.Candidate, rr uint64) []int {
	var typeOrder []string
	byType := map[string][]int{}
	for _, i := range idxs {
		t := cands[i].Model.Type
		if _, seen := byType[t]; !seen {
			typeOrder = append(typeOrder, t)
		}
		byType[t] = append(byType[t], i)
	}
	out := make([]int, 0, len(idxs))
	for _, tp := range typeOrder {
		s := byType[tp]
		n := len(s)
		start := int(rr % uint64(n))
		for k := 0; k < n; k++ {
			out = append(out, s[(start+k)%n])
		}
	}
	return out
}

// clampMaxTokens enforces a model's MaxTokens cap on the outgoing request body
// (P7): a present max_tokens/max_completion_tokens larger than the cap is reduced
// to it, and if neither is present the cap is set as max_tokens. Returns body
// unchanged when the model declares no cap or the body isn't JSON.
// rewriteModelField replaces the request body's top-level `model` with the
// upstream's own id. corrallm routes on the served name, but a remote provider
// only knows its own model id (Groq "llama-3.3-70b-versatile"), so the name has
// to be swapped on the way out. Same map[string]json.RawMessage approach as
// clampMaxTokens: it preserves every other field verbatim and is a no-op on a
// body that does not parse as JSON or carries no `model` (e.g. multipart audio),
// so a bad body forwards unchanged rather than being dropped.
func rewriteModelField(body []byte, upstream string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["model"]; !ok {
		return body
	}
	nv, err := json.Marshal(upstream)
	if err != nil {
		return body
	}
	m["model"] = nv
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return body
}

// rewriteModelMultipart replaces the `model` form field of a multipart body with
// the upstream's own id, returning the rebuilt body and its new Content-Type
// (the boundary changes). Reports false and leaves the caller's body alone on
// any parse failure — forwarding an unmodified body is recoverable, corrupting
// one is not.
//
// The audio routes need this because rewriteModelField is JSON-only: an
// extension's models are served as oidio-* but oidio knows them as stt,
// stt-diarize, tts and realtime-stt, and without the swap every transcription
// got a 404 for a model the backend had never heard of.
func rewriteModelMultipart(body []byte, contentType, upstream string) ([]byte, string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", false
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, "", false
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	seen := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", false
		}
		var w io.Writer
		if fn := part.FileName(); fn != "" {
			w, err = mw.CreateFormFile(part.FormName(), fn)
		} else {
			w, err = mw.CreateFormField(part.FormName())
		}
		if err != nil {
			return nil, "", false
		}
		if part.FormName() == "model" {
			seen = true
			if _, err := io.WriteString(w, upstream); err != nil {
				return nil, "", false
			}
			// Drain so the reader advances to the next part.
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, "", false
			}
			continue
		}
		if _, err := io.Copy(w, part); err != nil {
			return nil, "", false
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", false
	}
	if !seen {
		return nil, "", false
	}
	return out.Bytes(), mw.FormDataContentType(), true
}

func clampMaxTokens(body []byte, b config.Model) []byte {
	if b.MaxTokens <= 0 {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	changed := false
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := m[field]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) == nil && int(v) > b.MaxTokens {
			m[field] = json.RawMessage(strconv.Itoa(b.MaxTokens))
			changed = true
		}
	}
	if _, ok1 := m["max_tokens"]; !ok1 {
		if _, ok2 := m["max_completion_tokens"]; !ok2 {
			m["max_tokens"] = json.RawMessage(strconv.Itoa(b.MaxTokens))
			changed = true
		}
	}
	if !changed {
		return body
	}
	if out, err := json.Marshal(m); err == nil {
		return out
	}
	return body
}

// nextRR returns the round-robin rotation counter for a served model, advancing
// it once per request so same-type backends share load.
func (p *Proxy) nextRR(served string) uint64 {
	p.rrMu.Lock()
	defer p.rrMu.Unlock()
	v := p.rr[served]
	p.rr[served] = v + 1
	return v
}

// handleUpstream proxies /upstream/<model>/<rest> to the backend, stripping the
// prefix. Untracked: no model resolution from body, no activity log.
func (p *Proxy) handleUpstream(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upstream/")
	served, tail, _ := strings.Cut(rest, "/")
	model, ok := p.config().Models[served]
	if !ok {
		http.Error(w, "unknown model", http.StatusNotFound)
		return
	}
	pr, done, _, err := p.mgr.EnsureReady(r.Context(), served, model, nil)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
	defer done()
	r.URL.Path = "/" + tail
	newReverseProxy(pr.Target).ServeHTTP(w, r)
}

// handleCountTokens proxies POST /v1/messages/count_tokens to a proxied
// provider that answers it — today that is Anthropic, behind the `claude`
// extension.
//
// UNTRACKED, and that is the design. Counting tokens runs no inference, holds no
// GPU, and costs nothing; putting it through handleInference would make it take
// an admission slot and queue behind saturated work. A caller that sizes a
// prompt BEFORE deciding whether to send it would then be blocked by exactly the
// backlog it was trying to measure against. Same reasoning that makes
// handleUpstream untracked.
//
// PROXY-ONLY, on purpose. A local llama.cpp backend has no such route — its
// equivalent is /upstream/<model>/tokenize, which counts a raw string. The
// refusal says so rather than 404ing blankly, because the two are easy to
// confuse and the fix is a different URL, not a different model.
//
// The model is resolved through ResolveServed, so a glob template
// (`claude-haiku-*`) matches the concrete dated id a caller asks for. The body
// is forwarded UNCHANGED unless the target names an explicit upstream id — for
// the Anthropic passthrough `upstream` is deliberately unset so the provider's
// own model matrix validates the id, and rewriting it here would break every
// dated variant.
func (p *Proxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Model) == "" {
		http.Error(w, `body must be JSON with a "model" field`, http.StatusBadRequest)
		return
	}
	cands, ok := p.config().ResolveServed(req.Model)
	if !ok || len(cands) == 0 {
		http.Error(w, "unknown model: "+req.Model, http.StatusNotFound)
		return
	}
	// A lane resolves to several candidates; token counts are a property of one
	// tokenizer, so answering off the lane's first member would attribute a
	// count to a model the caller never named. Require a concrete model.
	if len(cands) > 1 {
		http.Error(w, "count_tokens needs a model, not a lane: "+req.Model+
			" resolves to "+strconv.Itoa(len(cands))+" members with different tokenizers",
			http.StatusBadRequest)
		return
	}
	// Remote() is the predicate, NOT "has a proxy target". Every locally-spawned
	// backend also has one — a loopback address is how corrallm reaches the
	// llama.cpp process it started — so testing for a target merely forwards
	// this to a local server that has no such route, and the caller gets a 502
	// from a dead port instead of an answer. Remote() means no local process AND
	// a non-loopback host: a provider we do not run, which is the only kind that
	// can answer this.
	if !cands[0].Model.Remote() {
		http.Error(w, "model "+req.Model+" is served locally, and a local backend has no "+
			"count_tokens route; use POST /upstream/"+req.Model+"/tokenize to count a raw string",
			http.StatusNotFound)
		return
	}
	target, err := cands[0].Model.ProxyTarget()
	if err != nil || target == nil || target.URL == nil {
		http.Error(w, "model "+req.Model+" has no reachable proxy target", http.StatusNotFound)
		return
	}
	if target.Model != "" {
		body = rewriteModelField(body, target.Model)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	newReverseProxy(target).ServeHTTP(w, r)
}

// handleModels returns an OpenAI-style catalog of served models, enriched with
// corrallm metadata (extra fields — OpenAI clients ignore unknown keys): the
// quality/type/backend shape from config plus live state + context length from
// the residency snapshot. Standard fields (id/object/created/owned_by) keep it
// drop-in compatible.
func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	// First resident backend per BACKING PROCESS → live state + parsed context
	// length. Keyed by ProcKey, not ModelName: oidio's four models are one
	// process, and keying by name reported whichever one spawned it as ready
	// while its siblings read "absent" off the same live process.
	resident := map[string]proc.ResidentModel{}
	for _, m := range p.mgr.Snapshot().Models {
		if _, ok := resident[m.ProcKey]; !ok {
			resident[m.ProcKey] = m
		}
	}

	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// corrallm metadata
		State   string  `json:"state"`             // absent|loading|ready|idle|evicting|proxy|paused
		Quality float64 `json:"quality,omitempty"` // quality tier (lane: top tier); fractional tiers are legal
		Type    string  `json:"type,omitempty"`    // cost class
		Kind    string  `json:"kind"`              // model|lane
		// Remote: served by a host we do not run (no local process, non-loopback
		// target). Kind stays "model" — it IS a model, and clients filter on
		// kind to separate models from lanes — so the distinction rides here.
		Remote        bool     `json:"remote,omitempty"`
		Members       []string `json:"members,omitempty"`        // lane member model names, in fallback order
		Persistent    bool     `json:"persistent,omitempty"`     // pinned + preloaded
		ContextLength int      `json:"context_length,omitempty"` // parsed n_ctx (if resident)
		Slots         int      `json:"slots,omitempty"`          // admission concurrency (maxConcurrent / --parallel)
		// Modalities: accepted input modalities keyed by name (text|image|audio),
		// each with optional metadata (image maxResolution/formats, text maxTokens).
		Modalities map[string]config.ModalitySpec `json:"modalities"`
		Capability string                         `json:"capability"` // chat|embeddings|audio.stt|audio.tts|rerank
	}
	// Data starts non-nil so an empty config serialises as [] rather than null.
	// OpenAI clients iterate the list without a nil check, and a fresh install
	// — the one case where the list IS empty — is exactly when someone is
	// pointing a client at it for the first time.
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}

	// AllModels, not cfg.Models: a discovered model is served, so it must be
	// listed. Omitting it would leave callers unable to find models the gateway
	// will happily route to.
	all := p.config().AllModels()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		mc := all[name]
		e := model{
			ID: name, Object: "model", Created: p.started, OwnedBy: "corrallm",
			State: "absent", Quality: mc.Quality, Type: mc.Type, Kind: "model",
			Persistent: mc.Persistent,
			Slots:      p.mgr.TunedSlots(name, mc.Slots()),
			Modalities: mc.EffectiveModalities(name, p.cost.IsAudioType(mc.Type)),
			Capability: config.ModelCapability(mc),
		}
		// A remote model has no residency to report. It used to inherit the
		// process state, which latched to "ready" on the first request and never
		// left — describing an unreachable upstream as loaded. "proxy" is the
		// honest answer: reachability is per-request, not a residency fact.
		if mc.Remote() {
			e.Remote, e.State = true, "proxy"
		} else if r, ok := resident[mc.ProcKey(name)]; ok {
			e.State = r.State
			e.ContextLength = r.NCtx
		}
		// Paused outranks everything above: "absent" would say a request could
		// load it, and an extension's paused model would otherwise inherit its
		// still-running sibling's "ready". The model stays LISTED — it is still
		// a configured model, and hiding it would make a paused model look
		// deleted — but its state says why it will not serve.
		if p.mgr.IsPaused(name) {
			e.State = "paused"
		}
		out.Data = append(out.Data, e)
	}

	// Lanes list alongside models: requesting a lane name allows fallback across
	// its members, so clients can target policy ("chat") instead of a model.
	laneNames := make([]string, 0, len(p.config().Lanes))
	for name := range p.config().Lanes {
		laneNames = append(laneNames, name)
	}
	sort.Strings(laneNames)
	for _, name := range laneNames {
		cands, _ := p.config().ResolveServed(name)
		members := make([]string, 0, len(cands))
		// Lane modalities/capability follow the PRIMARY member: a lane advertises
		// what its first-choice model accepts (a fallback may support less).
		var modalities map[string]config.ModalitySpec
		capability, state := "chat", "absent"
		for i, c := range cands {
			members = append(members, c.Name)
			if i == 0 {
				capability = config.ModelCapability(c.Model)
				modalities = c.Model.EffectiveModalities(c.Name, p.cost.IsAudioType(c.Model.Type))
			}
			// A remote member contributes "proxy", not residency; a real resident
			// member's state outranks it, since that one is measurably up.
			if c.Model.Remote() {
				if state == "absent" {
					state = "proxy"
				}
				continue
			}
			if r, ok := resident[c.Model.ProcKey(c.Name)]; ok && (state == "absent" || state == "proxy") {
				state = r.State
			}
		}
		laneSlots := 0
		if len(cands) > 0 {
			laneSlots = p.mgr.TunedSlots(cands[0].Name, cands[0].Model.Slots()) // primary member's (tuned) capacity
		}
		out.Data = append(out.Data, model{
			ID: name, Object: "model", Created: p.started, OwnedBy: "corrallm",
			State: state, Quality: config.MaxQuality(cands), Kind: "lane",
			Members: members, Slots: laneSlots, Modalities: modalities, Capability: capability,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleCapabilities returns the public self-describing manifest: the OpenAI
// surface corrallm fronts, the served models grouped by capability, the fairshare
// lanes (policy only — never the keys), and a runnable example per endpoint with
// real model names substituted. Synthesized from config; safe to expose.
func (p *Proxy) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	scheme, ws := "http", "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme, ws = "https", "wss"
	}
	base := scheme + "://" + r.Host
	wsBase := ws + "://" + r.Host

	// Group served names by capability — lanes first (clients should prefer the
	// policy name; fallback is the point), then pinned models.
	byCap := map[string][]string{}
	cfgLanes := make([]string, 0, len(p.config().Lanes))
	for name := range p.config().Lanes {
		cfgLanes = append(cfgLanes, name)
	}
	sort.Strings(cfgLanes)
	for _, name := range cfgLanes {
		if cands, ok := p.config().ResolveServed(name); ok {
			c := config.ModelCapability(cands[0].Model)
			byCap[c] = append(byCap[c], name)
		}
	}
	names := make([]string, 0, len(p.config().Models))
	for name := range p.config().Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := config.ModelCapability(p.config().Models[name])
		byCap[c] = append(byCap[c], name)
	}
	pick := func(caps ...string) string {
		for _, c := range caps {
			if len(byCap[c]) > 0 {
				return byCap[c][0]
			}
		}
		return "<your-model>"
	}
	// Batch STT (/v1/audio/transcriptions) and realtime STT (/v1/realtime ws) are
	// distinct capabilities — audio.stt vs audio.realtime — so each endpoint lists
	// only the models that serve it. No "modes" field; the cost type decides.
	sttBatch := byCap["audio.stt"]
	sttRealtime := byCap["audio.realtime"]
	first := func(xs []string, fallback string) string {
		if len(xs) > 0 {
			return xs[0]
		}
		return fallback
	}

	jsonExample := func(path string, body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		return map[string]any{
			"curl": fmt.Sprintf("curl -sS %s%s -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' -d '%s'", base, path, b),
			"body": body,
		}
	}

	chatM, embM, ttsM := pick("chat"), pick("embeddings"), pick("audio.tts")
	sttM := first(sttBatch, pick("audio.stt"))        // batch example (transcriptions)
	rtM := first(sttRealtime, pick("audio.realtime")) // realtime example (ws)
	type endpoint struct {
		Path        string         `json:"path"`
		Method      string         `json:"method"`
		Capability  string         `json:"capability"`
		Description string         `json:"description"`
		Models      []string       `json:"models,omitempty"`
		Streaming   bool           `json:"streaming,omitempty"`
		Example     map[string]any `json:"example,omitempty"`
	}
	endpoints := []endpoint{
		{"/v1/chat/completions", "POST", "chat", "OpenAI chat completions; set \"stream\":true for SSE token streaming.", byCap["chat"], true,
			jsonExample("/v1/chat/completions", map[string]any{"model": chatM, "messages": []map[string]string{{"role": "user", "content": "Hello"}}})},
		{"/v1/completions", "POST", "chat", "Legacy text completions.", byCap["chat"], true,
			jsonExample("/v1/completions", map[string]any{"model": chatM, "prompt": "Hello"})},
		{"/v1/embeddings", "POST", "embeddings", "Text embeddings.", byCap["embeddings"], false,
			jsonExample("/v1/embeddings", map[string]any{"model": embM, "input": "Hello world"})},
		{"/v1/rerank", "POST", "rerank", "Rerank documents against a query.", byCap["rerank"], false,
			jsonExample("/v1/rerank", map[string]any{"model": pick("rerank", "chat"), "query": "what is corrallm", "documents": []string{"a proxy", "a database"}})},
		{"/v1/audio/transcriptions", "POST", "audio.stt", "Speech-to-text (Whisper-compatible). multipart/form-data upload; supports response_format and stream. Some models also return speaker-diarized output.", sttBatch, true,
			map[string]any{
				"curl": fmt.Sprintf("curl -sS %s/v1/audio/transcriptions -H 'Authorization: Bearer <key>' -F model=%s -F file=@examples/audio/speech.wav", base, sttM),
				// A real, shipped sample: the manifest previously pointed at a
				// `speech.wav` that existed nowhere, so the example documenting
				// OpenAI compatibility could not actually be run.
				"sample":      "examples/audio/speech.wav (16-bit PCM WAV, mono 24kHz) — generated by this stack's own TTS and transcribes back to its source sentence",
				"note":        "multipart/form-data: model + file fields",
				"diarization": "A diarizing model additionally returns `segments:[{speaker,start,end,text}]` and `num_speakers` alongside the OpenAI `text`. Plain clients ignore the extra fields and read `.text`.",
			}},
		{"/v1/audio/translations", "POST", "audio.stt", "Speech-to-English translation; same shape as transcriptions.", sttBatch, false,
			map[string]any{"curl": fmt.Sprintf("curl -sS %s/v1/audio/translations -H 'Authorization: Bearer <key>' -F model=%s -F file=@examples/audio/speech.wav", base, sttM)}},
		{"/v1/audio/speech", "POST", "audio.tts", "Text-to-speech; returns binary audio (audio/mpeg by default).", byCap["audio.tts"], true,
			map[string]any{"curl": fmt.Sprintf("curl -sS %s/v1/audio/speech -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' -d '{\"model\":\"%s\",\"input\":\"Hello from corrallm\",\"voice\":\"af_heart\"}' --output speech.mp3", base, ttsM),
				"body": map[string]any{"model": ttsM, "input": "Hello from corrallm", "voice": "af_heart", "response_format": "mp3"}}},
		{"/v1/realtime", "GET", "audio.stt", "Live transcription over WebSocket (OpenAI Realtime transcription schema). Holds one fairshare slot for the session.", sttRealtime, true,
			map[string]any{
				"ws_url":   fmt.Sprintf("%s/v1/realtime?model=%s&intent=transcription", wsBase, rtM),
				"protocol": "OpenAI Realtime transcription schema. Send PCM16 mono @ 24kHz, base64-encoded inside JSON frames.",
				"flow": []string{
					"connect with header `Authorization: Bearer <key>` → receive {\"type\":\"session.created\"}",
					"send {\"type\":\"session.update\",\"session\":{\"input_audio_transcription\":{\"model\":\"" + rtM + "\"},\"turn_detection\":{\"type\":\"server_vad\"}}}",
					"stream repeatedly: {\"type\":\"input_audio_buffer.append\",\"audio\":\"<base64 pcm16@24k>\"}",
					"receive {\"type\":\"conversation.item.input_audio_transcription.completed\",\"transcript\":\"...\"}",
				},
			}},
		{"/v1/models", "GET", "meta", "OpenAI model catalog enriched with corrallm metadata (state, modality, types, context length).", nil, false,
			map[string]any{"curl": fmt.Sprintf("curl -sS %s/v1/models", base)}},
		{"/v1/capabilities", "GET", "meta", "This manifest.", nil, false,
			map[string]any{"curl": fmt.Sprintf("curl -sS %s/v1/capabilities", base)}},
		{"/v1/reservations", "POST", "meta", "Reserve slots on a model for your lane so interactive work has headroom against saturating batch. Short-lived (max 5m) and must be renewed by re-POSTing (heartbeat); auto-expires. DELETE ?model= to release; GET to list.", nil, false,
			map[string]any{
				"curl": fmt.Sprintf("curl -sS %s/v1/reservations -H 'Authorization: Bearer <key>' -H 'Content-Type: application/json' -d '{\"model\":\"%s\",\"slots\":1,\"ttl\":\"5m\"}'", base, chatM),
				"note": "Your key selects the lane the slots are held for. Re-POST every few minutes to keep the lease; stop to let batch reclaim.",
			}},
	}

	lanes := make([]map[string]any, 0, len(p.config().PriorityGroups))
	laneNames := make([]string, 0, len(p.config().PriorityGroups))
	for n := range p.config().PriorityGroups {
		laneNames = append(laneNames, n)
	}
	sort.Strings(laneNames)
	for _, n := range laneNames {
		g := p.config().PriorityGroups[n]
		cur := g.ShareCurrency
		if cur == "" {
			cur = "requests"
		}
		lanes = append(lanes, map[string]any{
			"name": n, "weight": g.EffectiveWeight(), "shareCurrency": cur, "interruptible": g.Interruptible,
		})
	}

	out := map[string]any{
		"service":           "corrallm",
		"description":       "OpenAI-compatible LLM reverse proxy + fairshare scheduler. Point any OpenAI client at this base URL.",
		"base_url":          base,
		"openai_compatible": true,
		"auth": map[string]any{
			"description": "Send your API key as `Authorization: Bearer <key>` (or `X-Corrallm-Key: <key>`). The key selects your fairshare lane; unkeyed callers use the default lane.",
			"headers":     []string{"Authorization: Bearer <key>", "X-Corrallm-Key: <key>"},
		},
		"endpoints":            endpoints,
		"models_by_capability": byCap,
		"lanes":                lanes,
		"backpressure": map[string]any{
			"description": "Under contention you get HTTP 429 with Retry-After + X-RateLimit-Capacity/-InFlight/-Waiting headers (and a JSON hint). Honor Retry-After and back off.",
			"headers":     []string{"Retry-After", "X-RateLimit-Capacity", "X-RateLimit-InFlight", "X-RateLimit-Waiting"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep <key>, &, → readable in the examples
	_ = enc.Encode(out)
}

// log persists an activity record (stamping its timestamp) and pushes a lean copy
// over the events broker. The pushed copy omits the captured payloads — those stay
// in the DB and are fetched on demand for the detail modal (P10b/c), not streamed
// to every SSE subscriber.
func (p *Proxy) log(a store.Activity) {
	a.TS = time.Now().UnixMilli()
	if err := p.store.InsertActivity(a); err != nil {
		slog.Warn("activity log", "err", err)
	}
	ev := a
	ev.ReqBody, ev.RespBody = "", ""
	p.publish(events.Event{Type: "activity", Data: ev})
}

// logReq stamps request-derived fields (the caller's source IP) onto the
// activity row before logging it. Every activity record originates from a
// request, so this is the single seam that resolves the client IP — callers
// pass their *http.Request rather than repeating clientIP(r) at each site.
func (p *Proxy) logReq(r *http.Request, a store.Activity) {
	a.SourceIP = clientIP(r)
	p.log(a)
}

// publish emits an event if a broker is attached (no-op otherwise).
func (p *Proxy) publish(e events.Event) { p.events.Publish(e) }

// callerKey extracts the caller identity used for group resolution: an explicit
// X-Corrallm-Key, else the bearer token from Authorization, else "" (→ default
// group). The token is the OpenAI API-key slot clients already send.
func callerKey(r *http.Request) string {
	if k := r.Header.Get("X-Corrallm-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); a != "" {
		if tok, ok := strings.CutPrefix(a, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
	}
	return ""
}

// clientIP returns the caller's IP for the activity log. chi's middleware.RealIP
// has already rewritten r.RemoteAddr from X-Forwarded-For / X-Real-IP (haproxy
// sets these via `option forwardfor`), so RemoteAddr is the real origin, not the
// proxy. The host:port is split to keep just the IP; a bare value (no port) is
// returned as-is. Trust note: RealIP trusts the forwarded header, which is fine
// because corrallm is only reachable via the trusted front proxy on the LAN.
func clientIP(r *http.Request) string {
	ra := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ra); err == nil {
		return host
	}
	return ra
}

// keepSoonest returns whichever backpressure promises relief first, so a walk
// over several candidates reports the earliest moment ANY of them could serve
// rather than whichever happened to be tried last. A zero RetryAfter means
// "unknown" (e.g. every blocker is held by an in-flight request with no
// predictable end), so it loses to any concrete estimate; two unknowns keep the
// incumbent.
func keepSoonest(cur, next *sched.BackpressureError) *sched.BackpressureError {
	if cur == nil {
		return next
	}
	if next == nil || next.RetryAfter == 0 {
		return cur
	}
	if cur.RetryAfter == 0 || next.RetryAfter < cur.RetryAfter {
		return next
	}
	return cur
}

// writeBackpressure renders a BackpressureError as 429 + informative headers and
// a JSON hint — always actionable (Retry-After + capacity/inflight/waiting).
//
// Returns the promise it actually made, in milliseconds, so the activity row
// records the number the caller received rather than a second rounding of
// bp.RetryAfter. The two would drift: this floors sub-second estimates to 1s.
func writeBackpressure(w http.ResponseWriter, bp *sched.BackpressureError) int64 {
	secs := int(bp.RetryAfter.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", strconv.Itoa(secs))
	h.Set("X-RateLimit-Capacity", strconv.Itoa(bp.Capacity))
	h.Set("X-RateLimit-InFlight", strconv.Itoa(bp.InFlight))
	h.Set("X-RateLimit-Waiting", strconv.Itoa(bp.Waiting))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message":     "backend at capacity; retry after backoff",
			"type":        "backpressure",
			"reason":      bp.Reason,
			"retry_after": secs,
			"capacity":    bp.Capacity,
			"in_flight":   bp.InFlight,
			"waiting":     bp.Waiting,
		},
	})
	return int64(secs) * 1000
}

// newReverseProxy builds a single-target reverse proxy that injects the
// target's auth headers (for remote/paid endpoints) and preserves streaming.
// joinPath concatenates a base-path prefix and the request path with exactly one
// slash between them — the same single-joining-slash rule net/http/httputil uses,
// so "/openai" + "/v1/chat/completions" = "/openai/v1/chat/completions" and a
// stray trailing/leading slash never doubles.
func joinPath(base, reqPath string) string {
	aslash := strings.HasSuffix(base, "/")
	bslash := strings.HasPrefix(reqPath, "/")
	switch {
	case aslash && bslash:
		return base + reqPath[1:]
	case !aslash && !bslash:
		return base + "/" + reqPath
	}
	return base + reqPath
}

// errBackendDown aborts a proxied response and spills to the next candidate when
// a free-tier remote hard-fails: 401/402/403 mean auth or billing, which a retry
// cannot fix, so the error must not reach the caller while another backend can
// serve. Returned from ModifyResponse before any body is written.
var errBackendDown = errors.New("free-tier backend hard failure")

// isHardFail is true for statuses that mean a backend structurally cannot serve
// this caller (unauthorized / payment-required / forbidden), as opposed to a
// transient 429 (rate limit) or 5xx (retryable).
func isHardFail(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusPaymentRequired ||
		status == http.StatusForbidden
}

// isSensitive reports whether a request opted into privacy-safe routing via the
// X-Corrallm-Sensitive header (true / 1 / yes). Such a request is confined to
// backends that will not expose its prompt to training (see filterBySensitive).
func isSensitive(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Corrallm-Sensitive"))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// upstreamID returns the id the BACKEND knows a model by, or "" when it uses
// the served name unchanged.
//
// The REQUESTED model's upstream id, not the process's. An extension's models
// share one Process, so pr.Target.Model is whichever of them happened to spawn
// it — routing every later request to that one's upstream. A diarize request
// answered as `tts` is how this first showed up.
func upstreamID(backend config.Model, pr *proc.Process) string {
	if backend.Upstream != "" {
		return backend.Upstream
	}
	if pr != nil && pr.Target != nil {
		return pr.Target.Model
	}
	return ""
}

// filterBySensitive keeps only backends safe for sensitive data: local models
// (own box — prompts never leave) and remotes flagged freeTier.private
// (contractually don't train on inputs). A non-private remote that may train is
// dropped. Unlike filterByQuota there is NO keep-all fallback — a sensitive
// request must refuse rather than leak, so an empty result is the correct
// "no privacy-safe backend" answer.
func filterBySensitive(cands []config.Candidate) []config.Candidate {
	kept := make([]config.Candidate, 0, len(cands))
	for _, c := range cands {
		if ft := c.Model.FreeTier; ft == nil || ft.Private {
			kept = append(kept, c)
		}
	}
	return kept
}

func newReverseProxy(t *config.ProxyTarget) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = t.URL.Scheme
			req.URL.Host = t.URL.Host
			req.Host = t.URL.Host
			// Prepend the target's base path when the upstream mounts its
			// OpenAI surface below root (Groq /openai, OpenRouter /api). Empty
			// for local backends, so /v1/... forwards unchanged.
			if t.BasePath != "" {
				req.URL.Path = joinPath(t.BasePath, req.URL.Path)
			}
			// Drop the client's Accept-Encoding so the transport negotiates
			// (and transparently decodes) compression itself — the body we
			// capture for usage metering is then identity, not gzip. Without
			// this a compressing upstream (common for paid endpoints) yields
			// unparseable bytes and meters as $0.
			req.Header.Del("Accept-Encoding")
			for k, v := range t.Headers {
				req.Header.Set(k, v)
			}
			// Dynamic bearer for short-lived rotating credentials (e.g. reusing
			// Claude Code's OAuth subscription token). A static Authorization
			// header set above wins; otherwise inject the resolved token.
			if t.AuthTokenCommand != "" && req.Header.Get("Authorization") == "" {
				if tok := resolveAuthToken(t.AuthTokenCommand); tok != "" {
					req.Header.Set("Authorization", "Bearer "+tok)
				}
			}
		},
		FlushInterval: 100 * time.Millisecond, // stream SSE chunks promptly
	}
	return rp
}

// authTokenCache memoizes AuthTokenCommand output so a per-request bearer isn't
// re-shelled on every call. Rotating tokens (Claude Code OAuth is good for ~4h
// and its own store keeps it refreshed) tolerate a short cache.
var (
	authTokMu    sync.Mutex
	authTokCache = map[string]authTokEntry{}
)

type authTokEntry struct {
	val string
	exp time.Time
}

const authTokenTTL = 60 * time.Second

// resolveAuthToken runs cmd via the shell, caches its trimmed stdout for
// authTokenTTL, and returns "" on failure — the request then goes out without a
// bearer, surfacing as a clear upstream 401 rather than a silent hang.
func resolveAuthToken(cmd string) string {
	authTokMu.Lock()
	if e, ok := authTokCache[cmd]; ok && time.Now().Before(e.exp) {
		authTokMu.Unlock()
		return e.val
	}
	authTokMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).Output()
	if err != nil {
		slog.Warn("authTokenCommand failed", "err", err)
		return ""
	}
	tok := strings.TrimSpace(string(out))

	authTokMu.Lock()
	authTokCache[cmd] = authTokEntry{val: tok, exp: time.Now().Add(authTokenTTL)}
	authTokMu.Unlock()
	return tok
}

// resolveRequest reads the served model and stream flag from a request. A JSON
// body (chat/completions/embeddings/…) carries both as top-level fields; an audio
// multipart/form-data body (P9a) carries them as form fields. It only inspects the
// buffered bytes — the same buffer is replayed to the upstream unchanged.
func resolveRequest(r *http.Request, body []byte) (model string, streaming bool) {
	if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil &&
		strings.HasPrefix(mt, "multipart/") {
		return modelFromMultipart(body, params["boundary"])
	}
	return modelFromBody(body), streamFromBody(body)
}

// modelFromMultipart extracts the "model" and "stream" form fields from a buffered
// multipart/form-data body without reading the (large) audio file part: NextPart
// streams past any part we don't consume. Field values are bounded — a multipart
// form field is small. Returns "" model when the boundary or field is absent.
func modelFromMultipart(body []byte, boundary string) (model string, streaming bool) {
	if boundary == "" {
		return "", false
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		switch part.FormName() {
		case "model":
			v, _ := io.ReadAll(io.LimitReader(part, 1<<10))
			model = strings.TrimSpace(string(v))
		case "stream":
			v, _ := io.ReadAll(io.LimitReader(part, 16))
			streaming = strings.TrimSpace(string(v)) == "true"
		}
		_ = part.Close()
	}
	return model, streaming
}

// modelFromBody extracts the "model" field from an OpenAI request body without
// fully unmarshalling it.
func modelFromBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// streamFromBody reports whether the request asked for an SSE stream, which
// decides how usage is recovered from the response.
func streamFromBody(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

// usage is the OpenAI token accounting carried in a response, plus the
// backend-measured (llama.cpp) telemetry extractUsage derives alongside it:
// cached prompt tokens and prompt/generation throughput. The throughput and
// CachedTokens values are backend-reported — corrallm does not compute them.
type usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Cost             float64 `json:"cost"` // provider-reported $ (OpenRouter); 0 if absent
	// PromptTokensDetails is the OpenAI-shape cached-token report nested under
	// "usage". extractUsage collapses it into CachedTokens.
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Derived by extractUsage from usage + the sibling "timings" object; not
	// members of the OpenAI "usage" JSON, hence json:"-".
	CachedTokens    int     `json:"-"` // cached prompt tokens (usage.prompt_tokens_details.cached_tokens, else timings.cache_n)
	PromptPerSec    float64 `json:"-"` // tp/s — prompt processing (timings.prompt_per_second)
	PredictedPerSec float64 `json:"-"` // tg/s — generation (timings.predicted_per_second)
}

// timings is llama.cpp's non-standard top-level throughput report, a sibling of
// "usage" in both non-streaming replies and the final streaming event.
type timings struct {
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	CacheN             int     `json:"cache_n"`
}

// mergeTimings folds a captured timings object into a usage value: fills the
// throughput speeds and uses cache_n as the cached-token fallback when the
// OpenAI-shape prompt_tokens_details.cached_tokens is absent/zero.
func mergeTimings(u usage, t timings) usage {
	u.CachedTokens = u.PromptTokensDetails.CachedTokens
	if u.CachedTokens == 0 {
		u.CachedTokens = t.CacheN
	}
	u.PromptPerSec = t.PromptPerSecond
	u.PredictedPerSec = t.PredictedPerSecond
	return u
}

// extractUsage recovers token usage from a captured response. A non-streaming
// body carries a single top-level "usage" object (and a sibling "timings"); a
// streaming (SSE) body carries them in a trailing data: event, present only when
// the client set stream_options.include_usage. Missing usage (no include_usage,
// or a body past the capture cap) yields zero — the request simply meters as $0.
func extractUsage(buf []byte, streaming bool) usage {
	if len(buf) == 0 {
		return usage{}
	}
	if !streaming {
		var r struct {
			Usage   usage   `json:"usage"`
			Timings timings `json:"timings"`
		}
		_ = json.Unmarshal(buf, &r)
		return mergeTimings(r.Usage, r.Timings)
	}
	var last usage
	for _, line := range bytes.Split(buf, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var r struct {
			Usage   *usage  `json:"usage"`
			Timings timings `json:"timings"`
		}
		if json.Unmarshal(data, &r) == nil && r.Usage != nil {
			last = mergeTimings(*r.Usage, r.Timings)
		}
	}
	return last
}

// extractFinishReason recovers why the model stopped, from the same captured
// body usage comes from.
//
// "stop" means the model chose to end. "length" means it ran into a cap and did
// NOT finish — the reply is truncated mid-thought, and a run of them is the
// signature of a caller sending no max_tokens against a model that will happily
// generate until the context wall.
//
// Streaming carries it on the LAST event whose choice has one (the final delta
// before [DONE]), which is why the streaming capture keeps a tail. Non-streaming
// carries it in the single JSON object; a reply past the capture cap yields
// empty, the same limitation usage already has, because the body must be parsed
// whole and a 1 MiB cap cannot hold an unbounded generation.
func extractFinishReason(buf []byte, streaming bool) string {
	if len(buf) == 0 {
		return ""
	}
	type choice struct {
		FinishReason string `json:"finish_reason"`
	}
	if !streaming {
		var r struct {
			Choices []choice `json:"choices"`
		}
		_ = json.Unmarshal(buf, &r)
		if len(r.Choices) > 0 {
			return r.Choices[0].FinishReason
		}
		return ""
	}
	last := ""
	for _, line := range bytes.Split(buf, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var r struct {
			Choices []choice `json:"choices"`
		}
		if json.Unmarshal(data, &r) == nil && len(r.Choices) > 0 && r.Choices[0].FinishReason != "" {
			last = r.Choices[0].FinishReason
		}
	}
	return last
}

// usageCaptureLimit bounds the response bytes statusCapture retains for usage
// extraction: the whole body for a (small) non-streaming reply, or the tail for
// a stream (usage rides in the final event).
const usageCaptureLimit = 1 << 20 // 1 MiB

// statusCapture records the response status and a bounded slice of the body for
// activity logging + usage metering, while preserving streaming.
type statusCapture struct {
	http.ResponseWriter
	// live, when set, receives per-write progress so the dashboard can tell a
	// long answer from a looping one while it is still running.
	live        *inflightEntry
	code        int
	wroteHeader bool
	streaming   bool
	buf         []byte    // bounded captured body for usage extraction
	written     int64     // total response bytes — TTS (P9b) is metered by output size
	firstWrite  time.Time // time of the first body write — for TTFB (P10b)
}

func (s *statusCapture) WriteHeader(code int) {
	// 1xx are interim (e.g. a backend's 100-continue on a large upload, forwarded
	// by ReverseProxy before the final status). Forward them but don't latch — else
	// the activity row records "100" instead of the real 200/4xx/5xx (P10b metering).
	if code >= 100 && code < 200 {
		s.ResponseWriter.WriteHeader(code)
		return
	}
	if !s.wroteHeader {
		s.code, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	if s.firstWrite.IsZero() && len(b) > 0 {
		s.firstWrite = time.Now()
	}
	s.written += int64(len(b))
	s.live.recordProgress(len(b))
	if s.streaming {
		// Keep a rolling tail — the final SSE event holds usage. Trim lazily at
		// 2× the cap so a long stream stays amortized O(n), not O(n²): the tail
		// we retain still covers the final event (well under one cap's worth).
		s.buf = append(s.buf, b...)
		if len(s.buf) > 2*usageCaptureLimit {
			s.buf = append([]byte(nil), s.buf[len(s.buf)-usageCaptureLimit:]...)
		}
	} else if len(s.buf) < usageCaptureLimit {
		// Non-streaming: capture from the front up to the cap (usage is in the
		// single JSON object; a reply larger than the cap meters as $0).
		if n := usageCaptureLimit - len(s.buf); n < len(b) {
			s.buf = append(s.buf, b[:n]...)
		} else {
			s.buf = append(s.buf, b...)
		}
	}
	return s.ResponseWriter.Write(b)
}

// Flush exposes the underlying flusher so SSE streaming works through the capture.
func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// config returns the proxy's current config. Always use this rather than a bare
// field read: the pointer is swapped on reload.
func (p *Proxy) config() *config.Config {
	return p.cfg.Load()
}

// SetConfig swaps in a reloaded config and rebuilds the derived cost model,
// which caches per-type coefficients read at construction.
func (p *Proxy) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	p.cfg.Store(cfg)
	p.cost = cost.NewModel(cfg)
}

// RetryableHeader lets a CALLER volunteer its request for preemption.
const RetryableHeader = "X-Corrallm-Retryable"

// retryableRequest reports whether the caller marked this request as safe to
// kick.
//
// Deliberately one-directional: a request may only make itself MORE
// interruptible, never less. Interruptibility is the operator's policy, set per
// priority group; letting a caller opt OUT would hand every client a way to
// pin a slot against that policy, and the loudest caller would win. Opting IN
// is safe because the cost lands on the volunteer.
//
// The honest use is a client that can cheaply redo its work — an indexing pass,
// a batch summarization — telling the scheduler "if something more important
// arrives, take my slot; I will come back".
func retryableRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(RetryableHeader))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
