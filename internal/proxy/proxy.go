// Package proxy is corrallm's OpenAI-compatible passthrough. It resolves the
// served model from a request, ensures a backend is ready (via proc.Manager),
// and reverse-proxies to it — logging each request to the activity store.
//
// P1 routes a served model to its FIRST backend only (single local backend).
// The ordered-list fall-through, fairshare admission, and scheduling land in
// P2/P3 — this package is the request edge they will wrap.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/store"
)

// Proxy is the inference edge handler.
type Proxy struct {
	cfg   *config.Config
	mgr   *proc.Manager
	store *store.Store
}

// New constructs a Proxy.
func New(cfg *config.Config, mgr *proc.Manager, st *store.Store) *Proxy {
	return &Proxy{cfg: cfg, mgr: mgr, store: st}
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

	// P1: first backend only.
	backend := model.Backends[0]
	name := served + "#0"

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	pr, err := p.mgr.EnsureReady(ctx, name, backend)
	if err != nil {
		slog.Error("backend not ready", "model", served, "err", err)
		http.Error(w, `{"error":{"message":"backend unavailable"}}`, http.StatusServiceUnavailable)
		p.log(served, name, r.URL.Path, http.StatusServiceUnavailable, time.Since(start))
		return
	}

	// Restore the buffered body for the proxy.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	sc := &statusCapture{ResponseWriter: w, code: http.StatusOK}
	newReverseProxy(pr.Target).ServeHTTP(sc, r.WithContext(ctx))
	p.log(served, name, r.URL.Path, sc.code, time.Since(start))
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
	pr, err := p.mgr.EnsureReady(r.Context(), name, model.Backends[0])
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		return
	}
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

func (p *Proxy) log(served, backend, path string, status int, dwell time.Duration) {
	if err := p.store.InsertActivity(store.Activity{
		TS:      time.Now().UnixMilli(),
		Served:  served,
		Backend: backend,
		Path:    path,
		Status:  status,
		DwellMS: dwell.Milliseconds(),
	}); err != nil {
		slog.Warn("activity log", "err", err)
	}
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
