package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// imageUpstream is a stand-in for sd-server: it already speaks OpenAI's
// /v1/images/generations, which is why corrallm forwards rather than adapts.
// reply is written verbatim; hits counts non-health requests so a test can
// assert a refused request never reached (and never cold-loaded) a backend.
func imageUpstream(t *testing.T, status int, reply string, hits *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt64(hits, 1)
		if r.URL.Path != "/v1/images/generations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
}

// imageConfig is a one-model config whose cost type declares a text-to-image
// generator, so config.Capability gives it image.generate.
func imageConfig(t *testing.T, served, upstream, typ string) *config.Config {
	t.Helper()
	cfg := mkConfig(t, served, upstream)
	m := cfg.Models[served]
	m.Type = typ
	cfg.Models[served] = m
	cfg.CostPerKwh = 0.14
	cfg.CommandCosts = map[string]map[string]any{typ: {"imageWhPerImage": 3}}
	return cfg
}

// TestImageGenerationsForwards is the P9h happy path: the route is mounted,
// goes through handleInference (so it takes a slot and logs activity), and the
// OpenAI response comes back byte-for-byte. The body is a base64 PNG, which is
// the whole reason this surface meters and captures differently from chat.
func TestImageGenerationsForwards(t *testing.T) {
	// ~48 KiB of base64 — small next to a real 1024² PNG, still far past the
	// 4 KiB payloadCap a naive capture would truncate it at.
	png := strings.Repeat("\x89PNG\r\n\x1a\n", 4096)
	b64 := base64.StdEncoding.EncodeToString([]byte(png))
	reply := `{"created":1,"data":[{"b64_json":"` + b64 + `"}]}`

	var hits int64
	upstream := imageUpstream(t, http.StatusOK, reply, &hits)
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := imageConfig(t, "sdxl-local", upstream.URL, "diffusion")
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"sdxl-local","prompt":"a red fox in snow","n":2,"size":"1024x1024"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != reply {
		t.Errorf("response not forwarded verbatim: got %d bytes, want %d", rec.Body.Len(), len(reply))
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}

	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	a := acts[0]
	if a.Path != "/v1/images/generations" || a.Served != "sdxl-local" || a.Status != http.StatusOK {
		t.Errorf("activity = %+v", a)
	}
	// No usage block exists on an image reply — a token count here would be
	// invented, and AudioBytes belongs to a different surface.
	if a.PromptTokens != 0 || a.CompletionTokens != 0 || a.AudioBytes != 0 {
		t.Errorf("image request metered tokens/audio: %d/%d/%d",
			a.PromptTokens, a.CompletionTokens, a.AudioBytes)
	}
	// Per IMAGE (n=2), not per byte: 2 × 3 Wh → kWh × $0.14.
	want := 2 * 3.0 / 1000 * 0.14
	if d := a.CostUSD - want; d < -1e-12 || d > 1e-12 {
		t.Errorf("cost = %v, want %v", a.CostUSD, want)
	}
	// Payloads live on the detail row, not the list row.
	full, err := st.ActivityByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The captured response is a SUMMARY. Storing 4 KiB of a truncated base64
	// payload would be neither a picture nor valid JSON.
	if full.RespBody != "<2 image(s), "+itoa(len(reply))+" bytes>" {
		t.Errorf("resp body = %q, want an image summary", full.RespBody)
	}
	if strings.Contains(full.RespBody, b64[:64]) {
		t.Error("base64 image data leaked into the activity log")
	}
	// The request body is small and IS worth keeping: it holds the prompt.
	if !strings.Contains(full.ReqBody, "a red fox in snow") {
		t.Errorf("req body = %q, want the prompt captured", full.ReqBody)
	}
}

// TestImageGenerationsDefaultN covers the `n`-absent case: OpenAI defaults it to
// 1, and metering it as 0 would make "omit n" the free way to generate.
func TestImageGenerationsDefaultN(t *testing.T) {
	var hits int64
	upstream := imageUpstream(t, http.StatusOK, `{"created":1,"data":[{"b64_json":"AAAA"}]}`, &hits)
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := imageConfig(t, "sd", upstream.URL, "stable-diffusion")
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"sd","prompt":"a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	want := 1 * 3.0 / 1000 * 0.14
	if d := acts[0].CostUSD - want; d < -1e-12 || d > 1e-12 {
		t.Errorf("cost = %v, want %v (n defaults to 1)", acts[0].CostUSD, want)
	}
}

// TestImageGenerationsRefusesNonGenerator is the door this surface needed that
// the audio routes never did. A chat model posted here would otherwise pay an
// admission slot, an eviction and a multi-GB cold load before llama-server
// answered 404 on a path it has never served.
func TestImageGenerationsRefusesNonGenerator(t *testing.T) {
	var hits int64
	upstream := imageUpstream(t, http.StatusOK, `{"data":[]}`, &hits)
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	// A chat model that ACCEPTS images (vision) is still not a generator: image
	// is in its In, and the route asks about Out.
	cfg := mkConfig(t, "vision-chat", upstream.URL)
	m := cfg.Models["vision-chat"]
	m.Type = "chat"
	m.Modalities = map[string]config.ModalitySpec{"text": {}, "image": {}}
	cfg.Models["vision-chat"] = m

	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"vision-chat","prompt":"a red fox"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// 400: the model exists and is spelled right, nothing is congested, and no
	// amount of retrying makes a chat model paint.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// The refusal is stated in IO terms, and names BOTH halves — that the model
	// accepts images is the very thing that makes the refusal look wrong until
	// you are told it returns text.
	if !strings.Contains(rec.Body.String(), `returns [text]`) ||
		!strings.Contains(rec.Body.String(), `accepts [image text]`) {
		t.Errorf("refusal does not state what the model takes and returns: %s", rec.Body.String())
	}
	// Declaring the `image` INPUT modality must not put image in Out.
	io := config.ModelIO("vision-chat", m, false)
	if !io.Accepts(config.ModalityImage) {
		t.Error("vision model does not accept images")
	}
	if io.Emits(config.ModalityImage) {
		t.Errorf("accepting images bought the model an image output: %+v", io)
	}
	// Nothing was loaded and nothing was forwarded — the point of the gate.
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("upstream hits = %d, want 0 (refused at the door)", n)
	}

	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 || acts[0].Status != http.StatusBadRequest ||
		acts[0].Path != "/v1/images/generations" {
		t.Fatalf("activity = %+v", acts)
	}
	if !strings.Contains(acts[0].Error, "does not return image") {
		t.Errorf("activity error = %q, want the output mismatch", acts[0].Error)
	}
}

// TestImageGenerationsCapturesErrors is the deliberate difference from TTS,
// which summarizes its body unconditionally and so loses its errors. A failed
// generation's body is small JSON and is the only thing in the row that says
// what went wrong.
func TestImageGenerationsCapturesErrors(t *testing.T) {
	var hits int64
	upstream := imageUpstream(t, http.StatusBadRequest,
		`{"error":{"message":"unsupported size 4096x4096"}}`, &hits)
	defer upstream.Close()

	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	cfg := imageConfig(t, "sd", upstream.URL, "diffusion")
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"sd","prompt":"a cat","size":"4096x4096"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}

	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	full, err := st.ActivityByID(acts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.RespBody, "unsupported size") {
		t.Errorf("failed generation's error body was not captured: %q", full.RespBody)
	}
	// Nothing was generated, so nothing is billed.
	if acts[0].CostUSD != 0 {
		t.Errorf("a failed generation billed %v", acts[0].CostUSD)
	}
}

// TestCapabilitiesListsImageGeneration holds the self-describing manifest to
// its promise: a client pointed at /v1/capabilities can find the endpoint, the
// models that serve it, and a curl that runs.
func TestCapabilitiesListsImageGeneration(t *testing.T) {
	cfg := &config.Config{
		CommandCosts: map[string]map[string]any{"diffusion": {"imageWhPerImage": 3}},
		Models: map[string]config.Model{
			"qwen": {Type: "chat"},
			// Vision: accepts images, generates none. Must NOT appear under
			// image.generate.
			"qwen-vl":    {Type: "chat", Modalities: map[string]config.ModalitySpec{"text": {}, "image": {}}},
			"qwen-image": {Type: "diffusion"},
		},
	}
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var m struct {
		ModelsByCapability map[string][]string `json:"models_by_capability"`
		Modalities         struct {
			Vocabulary []string `json:"vocabulary"`
		} `json:"modalities"`
		Endpoints []struct {
			Path       string         `json:"path"`
			Capability string         `json:"capability"`
			Accepts    []string       `json:"accepts"`
			Emits      []string       `json:"emits"`
			Models     []string       `json:"models"`
			Example    map[string]any `json:"example"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	got := m.ModelsByCapability["image.generate"]
	if len(got) != 1 || got[0] != "qwen-image" {
		t.Errorf("image.generate models = %v, want [qwen-image]", got)
	}
	if len(m.ModelsByCapability["chat"]) != 2 {
		t.Errorf("chat models = %v, want both qwen and qwen-vl", m.ModelsByCapability["chat"])
	}
	find := func(path string) int {
		for i := range m.Endpoints {
			if m.Endpoints[i].Path == path {
				return i
			}
		}
		return -1
	}
	i := find("/v1/images/generations")
	if i < 0 {
		t.Fatal("/v1/images/generations missing from the manifest")
	}
	ep := &m.Endpoints[i]
	if ep.Capability != "image.generate" {
		t.Errorf("endpoint capability = %q", ep.Capability)
	}
	// The route declares its own IO, which is how a client matches models to it
	// without decoding a capability name.
	if len(ep.Emits) != 1 || ep.Emits[0] != "image" {
		t.Errorf("endpoint emits = %v, want [image]", ep.Emits)
	}
	if len(ep.Accepts) != 1 || ep.Accepts[0] != "text" {
		t.Errorf("endpoint accepts = %v, want [text]", ep.Accepts)
	}
	// Chat ACCEPTS image (vision) and emits text. The two routes touching the
	// same modality on opposite sides is the whole distinction.
	if c := find("/v1/chat/completions"); c < 0 {
		t.Error("chat endpoint missing")
	} else {
		chat := m.Endpoints[c]
		if !containsStr(chat.Accepts, "image") {
			t.Errorf("chat accepts = %v, want image among them", chat.Accepts)
		}
		if containsStr(chat.Emits, "image") {
			t.Errorf("chat claims to emit an image: %v", chat.Emits)
		}
	}
	if len(m.Modalities.Vocabulary) == 0 {
		t.Error("the manifest does not publish the modality vocabulary")
	}
	if len(ep.Models) != 1 || ep.Models[0] != "qwen-image" {
		t.Errorf("endpoint models = %v", ep.Models)
	}
	curl, _ := ep.Example["curl"].(string)
	// A runnable example: the real model name, not a placeholder.
	if !strings.Contains(curl, "qwen-image") || !strings.Contains(curl, "/v1/images/generations") ||
		!strings.Contains(curl, "prompt") {
		t.Errorf("example curl is not runnable: %q", curl)
	}
	if _, ok := ep.Example["metering"]; !ok {
		t.Error("the manifest does not say how an image request is billed")
	}
}

func containsStr(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// TestModelsOutputsSplit holds /v1/models to the same split the manifest
// promises: `modalities` is what a model ACCEPTS, `outputs` is what it RETURNS,
// and reading the first as the whole story is what makes a vision model look
// like a generator.
func TestModelsOutputsSplit(t *testing.T) {
	cfg := &config.Config{
		CommandCosts: map[string]map[string]any{"diffusion": {"imageWhPerImage": 3}},
		Models: map[string]config.Model{
			"qwen-vl":    {Type: "chat", Modalities: map[string]config.ModalitySpec{"text": {}, "image": {}}},
			"qwen-image": {Type: "diffusion"},
		},
	}
	st, _ := store.Open(context.Background(), ":memory:")
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID         string            `json:"id"`
			Modalities map[string]any    `json:"modalities"`
			Outputs    []string          `json:"outputs"`
			Capability string            `json:"capability"`
			Extra      map[string]string `json:"-"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range out.Data {
		switch e.ID {
		case "qwen-vl":
			seen++
			if _, ok := e.Modalities["image"]; !ok {
				t.Error("vision model does not accept image")
			}
			if containsStr(e.Outputs, "image") {
				t.Errorf("vision model outputs = %v, want text only", e.Outputs)
			}
		case "qwen-image":
			seen++
			if _, ok := e.Modalities["image"]; ok {
				t.Errorf("a text-to-image model was given an image INPUT modality: %v", e.Modalities)
			}
			if !containsStr(e.Outputs, "image") {
				t.Errorf("generator outputs = %v, want image", e.Outputs)
			}
			if e.Capability != "image.generate" {
				t.Errorf("capability = %q", e.Capability)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 models", seen)
	}
}
