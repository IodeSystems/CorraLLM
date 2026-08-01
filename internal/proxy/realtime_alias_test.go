package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// TestRealtimeForwardsUpstreamAlias: /v1/realtime must forward the id the
// BACKEND knows a model by, not the served name.
//
// The model rides in the query string on this path rather than the body, and it
// was never rewritten — so corrallm forwarded "oidio-realtime-stt" to a backend
// whose model is "realtime-stt", and every extension-provided realtime model
// answered 404 "has no realtime transcription" while the same backend served
// batch STT fine.
//
// Exercised over the WebRTC/SDP transport (a plain POST) rather than the
// WebSocket upgrade: both branches proxy straight off r.URL, and this one is
// assertable without standing up a websocket server.
func TestRealtimeForwardsUpstreamAlias(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		gotModel = r.URL.Query().Get("model")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	m := modelTo(t, up.URL, "realtime")
	// What an extension's provided model looks like: served as "oidio-realtime-stt",
	// known to the backend as "realtime-stt".
	m.Upstream = "realtime-stt"
	cfg := &config.Config{
		Models:         map[string]config.Model{"oidio-realtime-stt": m},
		PriorityGroups: map[string]config.PriorityGroup{"g": {Weight: 1}},
		Keys:           map[string]string{"k": "g"},
	}
	mgr := proc.NewManager(cfg)
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/realtime?model=oidio-realtime-stt", nil)
	req.Header.Set("X-Corrallm-Key", "k")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if gotModel != "realtime-stt" {
		t.Errorf("backend saw model %q, want realtime-stt (the served name must not leak upstream)", gotModel)
	}
}

// TestRealtimeKeepsServedNameWithoutAlias: a model the backend knows by its own
// served name is forwarded unchanged — the rewrite must not invent an alias.
func TestRealtimeKeepsServedNameWithoutAlias(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		gotModel = r.URL.Query().Get("model")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	cfg := &config.Config{
		Models:         map[string]config.Model{"plain-realtime": modelTo(t, up.URL, "realtime")},
		PriorityGroups: map[string]config.PriorityGroup{"g": {Weight: 1}},
		Keys:           map[string]string{"k": "g"},
	}
	mgr := proc.NewManager(cfg)
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/realtime?model=plain-realtime", nil)
	req.Header.Set("X-Corrallm-Key", "k")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if gotModel != "plain-realtime" {
		t.Errorf("backend saw model %q, want plain-realtime unchanged", gotModel)
	}
}

// TestUpstreamIDPrefersRequestedModel: the alias comes from the REQUESTED model,
// never from the process's target.
//
// An extension's models share one Process, so Target.Model is whichever sibling
// happened to spawn it. Reading the alias from there routed every later request
// to that sibling's upstream — a diarize request answered as `tts`.
func TestUpstreamIDPrefersRequestedModel(t *testing.T) {
	var pn yaml.Node
	if err := pn.Encode(1234); err != nil {
		t.Fatal(err)
	}
	requested := config.Model{Upstream: "stt-diarize", Proxy: pn}
	spawned := &proc.Process{Target: &config.ProxyTarget{Model: "tts"}}

	if got := upstreamID(requested, spawned); got != "stt-diarize" {
		t.Errorf("upstreamID = %q, want stt-diarize (the requested model's alias)", got)
	}
	// With no alias of its own it falls back to the process's target.
	if got := upstreamID(config.Model{Proxy: pn}, spawned); got != "tts" {
		t.Errorf("fallback = %q, want tts", got)
	}
	// And nothing to rewrite when neither declares one.
	if got := upstreamID(config.Model{Proxy: pn}, &proc.Process{}); got != "" {
		t.Errorf("upstreamID = %q, want empty", got)
	}
}
