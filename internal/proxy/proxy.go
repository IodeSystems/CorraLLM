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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// Proxy is the inference edge handler.
type Proxy struct {
	cfg    *config.Config
	mgr    *proc.Manager
	sched  *sched.Scheduler
	store  *store.Store
	cost   *cost.Model
	events *events.Broker // optional: live UI events (P8-beyond)

	started int64 // unix seconds at construction — the catalog's "created"

	// calib holds the exclusive calibration lease, if any. Nil is valid and
	// means calibration mode is unavailable — every reader tolerates it.
	calib *CalibrationState

	requestTimeout     time.Duration        // 0 = no corrallm-imposed deadline (defer to client + backend)
	capturePayloads    bool                 // capture req/resp payloads onto the activity row (P10b)
	realtimeIdle       time.Duration        // 0 = no idle reap of a realtime ws session (P9e)
	realtimeMaxSession time.Duration        // 0 = no max-duration reap of a realtime ws session (P9e)
	convertEnabled     bool                 // master switch for chat attachment ingestion (P13)
	convertGlobal      config.ConvertConfig // global default; per-model `convert:` overrides it

	rrMu sync.Mutex
	rr   map[string]uint64 // per-served-model round-robin counter

	// quota is the P16 free-tier budget ledger: it learns each remote backend's
	// remaining rate-limit budget from the X-Ratelimit-* headers on its responses.
	quota *quota.Ledger
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
	p := &Proxy{cfg: cfg, mgr: mgr, sched: sc, store: st, cost: cost.NewModel(cfg),
		started: time.Now().Unix(), rr: map[string]uint64{}, capturePayloads: true,
		convertEnabled: true, convertGlobal: config.DefaultConvert(),
		calib: NewCalibrationState(), quota: quota.New()}
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
	cands, ok := p.cfg.ResolveServed(served)
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
		eff := p.cfg.ConvertFor(p.convertGlobal, served)
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
	// A calibration run owns the box: its measurements are meaningless under
	// contention, and its evictions would fight live traffic. 429 (not 503) so
	// clients PAUSE rather than fail — see calibration.go.
	if blocked, remaining := p.calib.Blocks(key); blocked {
		active, reason, _ := p.calib.Status()
		_ = active
		writeCalibrationBackpressure(w, remaining, reason)
		return
	}
	groupName, group := p.cfg.ResolveGroup(key)
	weight := group.EffectiveWeight()

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
	var queuedMS int64 // queue wait on the terminal backend (admit or reject)

	for _, idx := range walk {
		cand := cands[idx]
		backend := cand.Model
		name := cand.Name
		stage := group.StageFor(backend.Type)

		admitStart := time.Now()
		release, reqCtx, err := p.sched.Admit(ctx, name, backend.Type, backend.Slots(), groupName, weight, group.Interruptible, stage)
		queuedMS = time.Since(admitStart).Milliseconds() // ~0 unless this stage queued
		if err == nil {
			p.publish(events.Event{Type: "changed"}) // a slot was taken — lanes load changed
		}
		if err != nil {
			var bp *sched.BackpressureError
			if errors.As(err, &bp) {
				if bp.Reason == "spill" {
					bestBP = keepSoonest(bestBP, bp)
					continue // advance to the next backend
				}
				// rejected or queue-timeout → terminal backoff.
				writeBackpressure(w, bp)
				p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
					Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
					QueuedMS: queuedMS, Error: bp.Reason, ReqBody: reqBody})
				return
			}
			p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
				Status: 499, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS,
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
		pr, done, loaded, err := p.mgr.EnsureReady(reqCtx, name, backend, cand.Sticky)
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
		if pr.Target != nil && pr.Target.Model != "" {
			outBody = rewriteModelField(outBody, pr.Target.Model)
		}
		r.Body = io.NopCloser(bytes.NewReader(outBody))
		r.ContentLength = int64(len(outBody))
		sc := &statusCapture{ResponseWriter: w, code: http.StatusOK, streaming: streaming}
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
		rp.ModifyResponse = func(resp *http.Response) error {
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
		var costUSD float64
		var audioBytes int64
		switch {
		case tts:
			audioBytes = sc.written
			costUSD = p.cost.AudioRequestUSD(backend.Type, int(sc.written))
		case audio:
			audioBytes = int64(len(body))
			costUSD = p.cost.AudioRequestUSD(backend.Type, len(body))
		default:
			u = extractUsage(sc.buf, streaming)
			costUSD = p.cost.RequestUSD(backend.Type, u.PromptTokens, u.CompletionTokens)
		}
		if loaded && backend.Swap != nil {
			costUSD += p.cost.SwapUSD(backend.Swap.LoadSeconds, backend.Swap.LoadWatts)
		}
		release(sched.Done{CostUSD: costUSD})

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
			Served: served, Backend: name, Key: key, Path: r.URL.Path, Status: status,
			DwellMS: time.Since(start).Milliseconds(), PromptTokens: u.PromptTokens,
			CompletionTokens: u.CompletionTokens, CachedTokens: u.CachedTokens,
			PromptPerSec: u.PromptPerSec, PredictedPerSec: u.PredictedPerSec,
			CostUSD: costUSD, QueuedMS: queuedMS,
			AudioBytes: audioBytes, Error: errReason, TTFBMs: ttfbMS,
			ReqBody: reqBody, RespBody: respBody,
		})
		return
	}

	// Exhausted the list without serving.
	if bestBP != nil {
		bestBP.Reason = "exhausted"
		writeBackpressure(w, bestBP)
		p.logReq(r, store.Activity{Served: served, Backend: "-", Key: key, Path: r.URL.Path,
			Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
			QueuedMS: queuedMS, Error: "exhausted", ReqBody: reqBody})
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.logReq(r, store.Activity{Served: served, Backend: "-", Key: key, Path: r.URL.Path,
		Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
		QueuedMS: queuedMS, Error: "no backend available", ReqBody: reqBody})
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
	cands, ok := p.cfg.ResolveServed(served)
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
	if blocked, remaining := p.calib.Blocks(key); blocked {
		_, reason, _ := p.calib.Status()
		writeCalibrationBackpressure(w, remaining, reason)
		return
	}
	groupName, group := p.cfg.ResolveGroup(key)
	weight := group.EffectiveWeight()
	topQuality := config.MaxQuality(cands)
	ordered := orderCandidates(cands, p.nextRR(served))
	var lastBP *sched.BackpressureError
	var queuedMS int64

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
		queuedMS = time.Since(admitStart).Milliseconds()
		if err != nil {
			var bp *sched.BackpressureError
			if errors.As(err, &bp) {
				if bp.Reason == "spill" {
					lastBP = keepSoonest(lastBP, bp)
					continue
				}
				writeBackpressure(w, bp)
				p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
					Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
					QueuedMS: queuedMS, Error: bp.Reason})
				return
			}
			p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
				Status: 499, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS, Error: "client canceled"})
			return
		}
		p.publish(events.Event{Type: "changed"})

		pr, done, _, err := p.mgr.EnsureReady(reqCtx, name, backend, cand.Sticky)
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
			continue
		}
		// Same abort-panic guard as the inference path: never leak the residency
		// ref (done is sync.Once-guarded; inline calls stay no-ops).
		defer done()

		if !ws {
			// WebRTC signaling: reverse-proxy the SDP offer→answer. The slot is held
			// only for the handshake (the P2P media session isn't tracked here); no
			// audio traverses corrallm, so AudioBytes stays 0.
			sc := &statusCapture{ResponseWriter: w}
			newReverseProxy(pr.Target).ServeHTTP(sc, r.WithContext(reqCtx))
			done()
			status := sc.code
			if status == 0 {
				status = http.StatusOK
			}
			release(sched.Done{})
			p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
				Status: status, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS})
			return
		}

		// Proxy the session under reqCtx so a preemption (ErrPreempted) tears down
		// the upgraded conn. Meter the audio streamed IN (client→backend bytes).
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
		p.logReq(r, store.Activity{Served: served, Backend: name, Key: key, Path: r.URL.Path,
			Status: status, DwellMS: time.Since(start).Milliseconds(), QueuedMS: queuedMS,
			AudioBytes: inBytes, CostUSD: costUSD, Error: errReason})
		return
	}

	if lastBP != nil {
		lastBP.Reason = "exhausted"
		writeBackpressure(w, lastBP)
		p.logReq(r, store.Activity{Served: served, Backend: "-", Key: key, Path: r.URL.Path,
			Status: http.StatusTooManyRequests, DwellMS: time.Since(start).Milliseconds(),
			QueuedMS: queuedMS, Error: "exhausted"})
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.logReq(r, store.Activity{Served: served, Backend: "-", Key: key, Path: r.URL.Path,
		Status: http.StatusServiceUnavailable, DwellMS: time.Since(start).Milliseconds(),
		QueuedMS: queuedMS, Error: "no backend available"})
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
	tiers := map[int][]int{}
	var qualities []int
	for i, c := range cands {
		if _, seen := tiers[c.Model.Quality]; !seen {
			qualities = append(qualities, c.Model.Quality)
		}
		tiers[c.Model.Quality] = append(tiers[c.Model.Quality], i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(qualities))) // best quality first
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
	model, ok := p.cfg.Models[served]
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

// handleModels returns an OpenAI-style catalog of served models, enriched with
// corrallm metadata (extra fields — OpenAI clients ignore unknown keys): the
// quality/type/backend shape from config plus live state + context length from
// the residency snapshot. Standard fields (id/object/created/owned_by) keep it
// drop-in compatible.
func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	// First resident backend per served model → live state + parsed context length.
	resident := map[string]proc.ResidentModel{}
	for _, m := range p.mgr.Snapshot().Models {
		if _, ok := resident[m.ModelName]; !ok {
			resident[m.ModelName] = m
		}
	}

	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// corrallm metadata
		State         string   `json:"state"`                    // absent|loading|ready|idle|evicting
		Quality       int      `json:"quality,omitempty"`        // quality tier (lane: top tier)
		Type          string   `json:"type,omitempty"`           // cost class
		Kind          string   `json:"kind"`                     // model|lane
		Members       []string `json:"members,omitempty"`        // lane member model names, in fallback order
		Persistent    bool     `json:"persistent,omitempty"`     // pinned + preloaded
		ContextLength int      `json:"context_length,omitempty"` // parsed n_ctx (if resident)
		Slots         int      `json:"slots,omitempty"`          // admission concurrency (maxConcurrent / --parallel)
		// Modalities: accepted input modalities keyed by name (text|image|audio),
		// each with optional metadata (image maxResolution/formats, text maxTokens).
		Modalities map[string]config.ModalitySpec `json:"modalities"`
		Capability string                         `json:"capability"` // chat|embeddings|audio.stt|audio.tts|rerank
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}

	names := make([]string, 0, len(p.cfg.Models))
	for name := range p.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		mc := p.cfg.Models[name]
		e := model{
			ID: name, Object: "model", Created: p.started, OwnedBy: "corrallm",
			State: "absent", Quality: mc.Quality, Type: mc.Type, Kind: "model",
			Persistent: mc.Persistent,
			Slots:      p.mgr.TunedSlots(name, mc.Slots()),
			Modalities: mc.EffectiveModalities(p.cost.IsAudioType(mc.Type)),
			Capability: config.ModelCapability(mc),
		}
		if r, ok := resident[name]; ok {
			e.State = r.State
			e.ContextLength = r.NCtx
		}
		out.Data = append(out.Data, e)
	}

	// Lanes list alongside models: requesting a lane name allows fallback across
	// its members, so clients can target policy ("chat") instead of a model.
	laneNames := make([]string, 0, len(p.cfg.Lanes))
	for name := range p.cfg.Lanes {
		laneNames = append(laneNames, name)
	}
	sort.Strings(laneNames)
	for _, name := range laneNames {
		cands, _ := p.cfg.ResolveServed(name)
		members := make([]string, 0, len(cands))
		// Lane modalities/capability follow the PRIMARY member: a lane advertises
		// what its first-choice model accepts (a fallback may support less).
		var modalities map[string]config.ModalitySpec
		capability, state := "chat", "absent"
		for i, c := range cands {
			members = append(members, c.Name)
			if i == 0 {
				capability = config.ModelCapability(c.Model)
				modalities = c.Model.EffectiveModalities(p.cost.IsAudioType(c.Model.Type))
			}
			if r, ok := resident[c.Name]; ok && state == "absent" {
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
	cfgLanes := make([]string, 0, len(p.cfg.Lanes))
	for name := range p.cfg.Lanes {
		cfgLanes = append(cfgLanes, name)
	}
	sort.Strings(cfgLanes)
	for _, name := range cfgLanes {
		if cands, ok := p.cfg.ResolveServed(name); ok {
			c := config.ModelCapability(cands[0].Model)
			byCap[c] = append(byCap[c], name)
		}
	}
	names := make([]string, 0, len(p.cfg.Models))
	for name := range p.cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := config.ModelCapability(p.cfg.Models[name])
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

	lanes := make([]map[string]any, 0, len(p.cfg.PriorityGroups))
	laneNames := make([]string, 0, len(p.cfg.PriorityGroups))
	for n := range p.cfg.PriorityGroups {
		laneNames = append(laneNames, n)
	}
	sort.Strings(laneNames)
	for _, n := range laneNames {
		g := p.cfg.PriorityGroups[n]
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
func writeBackpressure(w http.ResponseWriter, bp *sched.BackpressureError) {
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
		},
		FlushInterval: 100 * time.Millisecond, // stream SSE chunks promptly
	}
	return rp
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
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
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

// usageCaptureLimit bounds the response bytes statusCapture retains for usage
// extraction: the whole body for a (small) non-streaming reply, or the tail for
// a stream (usage rides in the final event).
const usageCaptureLimit = 1 << 20 // 1 MiB

// statusCapture records the response status and a bounded slice of the body for
// activity logging + usage metering, while preserving streaming.
type statusCapture struct {
	http.ResponseWriter
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

// Calibration exposes the exclusive-calibration lease so the admin API can
// begin/end it. Never nil for a Proxy built by New.
func (p *Proxy) Calibration() *CalibrationState { return p.calib }
