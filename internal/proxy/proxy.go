// Package proxy is corrallm's OpenAI-compatible passthrough. It resolves the
// served model and caller group from a request, acquires a fairshare slot
// (sched), ensures the backend is ready (proc), reverse-proxies to it, and logs
// the request. Saturation yields 429 + informative backoff.
//
// It routes a served model to its FIRST backend only; ordered-list fall-through
// across types is P3 — this package is the request edge those phases wrap.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/cost"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/proc"
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

	rrMu sync.Mutex
	rr   map[string]uint64 // per-served-model round-robin counter
}

// New constructs a Proxy.
func New(cfg *config.Config, mgr *proc.Manager, sc *sched.Scheduler, st *store.Store) *Proxy {
	return &Proxy{cfg: cfg, mgr: mgr, sched: sc, store: st, cost: cost.NewModel(cfg), rr: map[string]uint64{}}
}

// SetBroker attaches an events broker so the request path can push live updates
// (a new activity record, a "state changed" ping). Optional; nil disables it.
func (p *Proxy) SetBroker(b *events.Broker) { p.events = b }

// Mount registers the OpenAI-compatible inference routes plus the untracked
// non-inference passthrough on r. The route set mirrors the OpenAI surface
// corrallm fronts (chat/completions, completions, embeddings, models).
func (p *Proxy) Mount(mux interface {
	Handle(pattern string, h http.Handler)
}) {
	for _, path := range []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/rerank",
	} {
		mux.Handle(path, http.HandlerFunc(p.handleInference))
	}
	// /v1/models is a catalog response synthesized from config, not proxied.
	mux.Handle("/v1/models", http.HandlerFunc(p.handleModels))
	// Non-inference UI/passthrough: /upstream/<model>/… serves UNTRACKED once
	// the backend is up — it must not consume admission/concurrency (the
	// gatedPaths lesson, structural here). No activity log, no scheduling.
	// Wildcard so chi matches the whole subtree.
	mux.Handle("/upstream/*", http.HandlerFunc(p.handleUpstream))
}

// handleInference resolves the served model from the JSON body's "model" field,
// ensures its first backend is ready, and reverse-proxies the (buffered) body.
func (p *Proxy) handleInference(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	served := modelFromBody(body)
	if served == "" {
		http.Error(w, `{"error":{"message":"missing \"model\""}}`, http.StatusBadRequest)
		return
	}
	model, ok := p.cfg.Models[served]
	if !ok || len(model.Backends) == 0 {
		http.Error(w, `{"error":{"message":"unknown model \"`+served+`\""}}`, http.StatusNotFound)
		return
	}

	start := time.Now()
	streaming := streamFromBody(body)
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	key := callerKey(r)
	groupName, group := p.cfg.ResolveGroup(key)
	weight := group.EffectiveWeight()

	// Walk the ordered backend list (rr within a cost-equivalent `type`, ordered
	// across types). For each: take a slot or honor the group's saturation stage
	// for that type — spill/fallThrough advances to the next backend; queue waits;
	// reject is terminal. A backend that won't become ready also spills.
	// Quality-degrade fall-through (P7): walk the backend list best-quality-first,
	// keeping only the tiers this group accepts. A non-degrading group sees only
	// the top tier, so saturation there backs off (per its stage) instead of
	// spilling onto a worse model; a degrading group walks down to its floor.
	topQuality := config.MaxQuality(model.Backends)
	ordered := orderBackends(model.Backends, p.nextRR(served))
	walk := ordered[:0:0]
	for _, idx := range ordered {
		if group.AcceptsQuality(model.Backends[idx].Quality, topQuality) {
			walk = append(walk, idx)
		}
	}
	var lastBP *sched.BackpressureError
	var queuedMS int64 // queue wait on the terminal backend (admit or reject)

	for _, idx := range walk {
		backend := model.Backends[idx]
		name := fmt.Sprintf("%s#%d", served, idx)
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
					lastBP = bp
					continue // advance to the next backend
				}
				// rejected or queue-timeout → terminal backoff.
				writeBackpressure(w, bp)
				p.log(served, name, key, r.URL.Path, http.StatusTooManyRequests, time.Since(start), 0, 0, 0, queuedMS)
				return
			}
			p.log(served, name, key, r.URL.Path, 499, time.Since(start), 0, 0, 0, queuedMS) // client canceled
			return
		}

		// Proxy under reqCtx so a later preemption (cause ErrPreempted) aborts the
		// upstream stream and frees this slot.
		pr, done, loaded, err := p.mgr.EnsureReady(reqCtx, name, served, backend)
		if err != nil {
			release()
			// Doesn't fit + can't evict, or won't come up → spill to next backend.
			slog.Warn("backend unavailable, spilling", "backend", name, "err", err)
			continue
		}

		// Restore the buffered body for the proxy, clamping max_tokens to this
		// backend's cap when it declares one (degrade transform, P7).
		outBody := clampMaxTokens(body, backend)
		r.Body = io.NopCloser(bytes.NewReader(outBody))
		r.ContentLength = int64(len(outBody))
		sc := &statusCapture{ResponseWriter: w, code: http.StatusOK, streaming: streaming}
		newReverseProxy(pr.Target).ServeHTTP(sc, r.WithContext(reqCtx))
		done()
		status := sc.code
		if errors.Is(context.Cause(reqCtx), sched.ErrPreempted) {
			status = 499 // slot reclaimed by a higher-priority group mid-request
		}
		// Meter the served request: extract token usage from the response and
		// resolve it to $ via the backend's cost class. A cold load triggered by
		// this request also bills its swap energy to it. The cost is reported to
		// the scheduler (limit budgets + cost share currency) at release.
		u := extractUsage(sc.buf, streaming)
		costUSD := p.cost.RequestUSD(backend.Type, u.PromptTokens, u.CompletionTokens)
		if loaded && backend.Swap != nil {
			costUSD += p.cost.SwapUSD(backend.Swap.LoadSeconds, backend.Swap.LoadWatts)
		}
		release(sched.Done{CostUSD: costUSD})
		p.log(served, name, key, r.URL.Path, status, time.Since(start), u.PromptTokens, u.CompletionTokens, costUSD, queuedMS)
		return
	}

	// Exhausted the list without serving.
	if lastBP != nil {
		lastBP.Reason = "exhausted"
		writeBackpressure(w, lastBP)
		p.log(served, "-", key, r.URL.Path, http.StatusTooManyRequests, time.Since(start), 0, 0, 0, queuedMS)
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.log(served, "-", key, r.URL.Path, http.StatusServiceUnavailable, time.Since(start), 0, 0, 0, queuedMS)
}

// orderBackends returns config indices in fall-through order: highest quality
// tier first, descending (the degrade ladder, P7). Within a tier, types appear
// in first-appearance order and same-type backends rotate by rr (round robin
// across cost-equivalent peers). Uniform quality → a single tier → identical to
// the pre-P7 type-rr ordering (no regression for configs that don't use quality).
func orderBackends(backends []config.Backend, rr uint64) []int {
	tiers := map[int][]int{}
	var qualities []int
	for i, b := range backends {
		if _, seen := tiers[b.Quality]; !seen {
			qualities = append(qualities, b.Quality)
		}
		tiers[b.Quality] = append(tiers[b.Quality], i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(qualities))) // best quality first
	out := make([]int, 0, len(backends))
	for _, q := range qualities {
		out = append(out, orderByTypeRR(tiers[q], backends, rr)...)
	}
	return out
}

// orderByTypeRR orders a single quality tier's indices: types in first-appearance
// order, same-type backends rotated by rr.
func orderByTypeRR(idxs []int, backends []config.Backend, rr uint64) []int {
	var typeOrder []string
	byType := map[string][]int{}
	for _, i := range idxs {
		t := backends[i].Type
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

// clampMaxTokens enforces a backend's MaxTokens cap on the outgoing request body
// (P7): a present max_tokens/max_completion_tokens larger than the cap is reduced
// to it, and if neither is present the cap is set as max_tokens. Returns body
// unchanged when the backend declares no cap or the body isn't JSON.
func clampMaxTokens(body []byte, b config.Backend) []byte {
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
	if !ok || len(model.Backends) == 0 {
		http.Error(w, "unknown model", http.StatusNotFound)
		return
	}
	name := served + "#0"
	pr, done, _, err := p.mgr.EnsureReady(r.Context(), name, served, model.Backends[0])
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
	defer done()
	r.URL.Path = "/" + tail
	newReverseProxy(pr.Target).ServeHTTP(w, r)
}

// handleModels returns an OpenAI-style catalog of served models from config.
func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}
	for name := range p.cfg.Models {
		out.Data = append(out.Data, model{ID: name, Object: "model", OwnedBy: "corrallm"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (p *Proxy) log(served, backend, key, path string, status int, dwell time.Duration, prompt, completion int, costUSD float64, queuedMS int64) {
	a := store.Activity{
		TS:               time.Now().UnixMilli(),
		Served:           served,
		Backend:          backend,
		Key:              key,
		Path:             path,
		Status:           status,
		DwellMS:          dwell.Milliseconds(),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostUSD:          costUSD,
		QueuedMS:         queuedMS,
	}
	if err := p.store.InsertActivity(a); err != nil {
		slog.Warn("activity log", "err", err)
	}
	// Push the new record (the request also just released a slot → lanes/usage
	// changed). Best-effort; the UI keeps a slow fallback poll.
	p.publish(events.Event{Type: "activity", Data: a})
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
func newReverseProxy(t *config.ProxyTarget) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = t.URL.Scheme
			req.URL.Host = t.URL.Host
			req.Host = t.URL.Host
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

// usage is the OpenAI token accounting carried in a response.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// extractUsage recovers token usage from a captured response. A non-streaming
// body carries a single top-level "usage" object; a streaming (SSE) body carries
// it in a trailing data: event, present only when the client set
// stream_options.include_usage. Missing usage (no include_usage, or a body past
// the capture cap) yields zero — the request simply meters as $0.
func extractUsage(buf []byte, streaming bool) usage {
	if len(buf) == 0 {
		return usage{}
	}
	if !streaming {
		var r struct {
			Usage usage `json:"usage"`
		}
		_ = json.Unmarshal(buf, &r)
		return r.Usage
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
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(data, &r) == nil && r.Usage != nil {
			last = *r.Usage
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
	buf         []byte // bounded captured body for usage extraction
}

func (s *statusCapture) WriteHeader(code int) {
	if !s.wroteHeader {
		s.code, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
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
