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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// Proxy is the inference edge handler.
type Proxy struct {
	cfg   *config.Config
	mgr   *proc.Manager
	sched *sched.Scheduler
	store *store.Store

	rrMu sync.Mutex
	rr   map[string]uint64 // per-served-model round-robin counter
}

// New constructs a Proxy.
func New(cfg *config.Config, mgr *proc.Manager, sc *sched.Scheduler, st *store.Store) *Proxy {
	return &Proxy{cfg: cfg, mgr: mgr, sched: sc, store: st, rr: map[string]uint64{}}
}

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
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	key := callerKey(r)
	groupName, group := p.cfg.ResolveGroup(key)
	weight := group.EffectiveWeight()

	// Walk the ordered backend list (rr within a cost-equivalent `type`, ordered
	// across types). For each: take a slot or honor the group's saturation stage
	// for that type — spill/fallThrough advances to the next backend; queue waits;
	// reject is terminal. A backend that won't become ready also spills.
	walk := orderBackends(model.Backends, p.nextRR(served))
	var lastBP *sched.BackpressureError

	for _, idx := range walk {
		backend := model.Backends[idx]
		name := fmt.Sprintf("%s#%d", served, idx)
		stage := group.StageFor(backend.Type)

		release, err := p.sched.Admit(ctx, name, backend.Slots(), groupName, weight, stage)
		if err != nil {
			var bp *sched.BackpressureError
			if errors.As(err, &bp) {
				if bp.Reason == "spill" {
					lastBP = bp
					continue // advance to the next backend
				}
				// rejected or queue-timeout → terminal backoff.
				writeBackpressure(w, bp)
				p.log(served, name, key, r.URL.Path, http.StatusTooManyRequests, time.Since(start))
				return
			}
			p.log(served, name, key, r.URL.Path, 499, time.Since(start)) // client canceled
			return
		}

		pr, done, err := p.mgr.EnsureReady(ctx, name, served, backend)
		if err != nil {
			release()
			// Doesn't fit + can't evict, or won't come up → spill to next backend.
			slog.Warn("backend unavailable, spilling", "backend", name, "err", err)
			continue
		}

		// Restore the buffered body for the proxy.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		sc := &statusCapture{ResponseWriter: w, code: http.StatusOK}
		newReverseProxy(pr.Target).ServeHTTP(sc, r.WithContext(ctx))
		done()
		release()
		p.log(served, name, key, r.URL.Path, sc.code, time.Since(start))
		return
	}

	// Exhausted the list without serving.
	if lastBP != nil {
		lastBP.Reason = "exhausted"
		writeBackpressure(w, lastBP)
		p.log(served, "-", key, r.URL.Path, http.StatusTooManyRequests, time.Since(start))
		return
	}
	http.Error(w, `{"error":{"message":"no backend available"}}`, http.StatusServiceUnavailable)
	p.log(served, "-", key, r.URL.Path, http.StatusServiceUnavailable, time.Since(start))
}

// orderBackends returns config indices in fall-through order: types in
// first-appearance order, and within a type the backends rotated by rr (round
// robin across cost-equivalent backends). Quality is carried metadata, not a
// sort key here — the operator's list order across types is authoritative; per-
// quality variant routing is P7.
func orderBackends(backends []config.Backend, rr uint64) []int {
	var typeOrder []string
	byType := map[string][]int{}
	for i, b := range backends {
		if _, seen := byType[b.Type]; !seen {
			typeOrder = append(typeOrder, b.Type)
		}
		byType[b.Type] = append(byType[b.Type], i)
	}
	out := make([]int, 0, len(backends))
	for _, tp := range typeOrder {
		idxs := byType[tp]
		n := len(idxs)
		start := int(rr % uint64(n))
		for k := 0; k < n; k++ {
			out = append(out, idxs[(start+k)%n])
		}
	}
	return out
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
	pr, done, err := p.mgr.EnsureReady(r.Context(), name, served, model.Backends[0])
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

func (p *Proxy) log(served, backend, key, path string, status int, dwell time.Duration) {
	if err := p.store.InsertActivity(store.Activity{
		TS:      time.Now().UnixMilli(),
		Served:  served,
		Backend: backend,
		Key:     key,
		Path:    path,
		Status:  status,
		DwellMS: dwell.Milliseconds(),
	}); err != nil {
		slog.Warn("activity log", "err", err)
	}
}

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

// statusCapture records the response status for the activity log.
type statusCapture struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
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
	return s.ResponseWriter.Write(b)
}

// Flush exposes the underlying flusher so SSE streaming works through the capture.
func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
