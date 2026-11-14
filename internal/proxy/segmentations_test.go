package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// fakeSam is a stand-in for the sam3 shim. It validates nothing: coordinate
// checks belong to the shim (P78 §1a), and corrallm must forward the body and
// the shim's verdict untouched. A request whose first point has x < 0 gets the
// 400 the real shim would give, with a `param` path.
type fakeSam struct {
	*httptest.Server
	mu   sync.Mutex
	hits int
	last []byte
}

func newFakeSam(t *testing.T, okReply string) *fakeSam {
	t.Helper()
	f := &fakeSam{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits++
		f.last = b
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/images/segmentations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(string(b), `"x":-1`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"x out of range","type":"invalid_request_error","param":"prompts[1].points[0].x"}}`))
			return
		}
		_, _ = w.Write([]byte(okReply))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSam) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits }

const samReply = `{"id":"seg_1","object":"image.segmentation","model":"sam3-segment","image":{"width":64,"height":48},` +
	`"results":[{"prompt_index":0,"type":"concept","predictions":[{"score":0.9,"box":[1,2,30,40],"area":99,"mask":{"size":[48,64],"counts":"abc"}}]}],` +
	`"usage":{"images":1,"prompts":2,"masks":1}}`

const samReq = `{"model":"sam3-segment","image":"data:image/png;base64,AAAA","prompts":[` +
	`{"type":"concept","text":"wheel"},` +
	`{"type":"visual","points":[{"x":%s,"y":230,"label":1}]}]}`

func segMount(t *testing.T, cfg *config.Config) (*chi.Mux, *store.Store) {
	t.Helper()
	st, _ := store.Open(context.Background(), ":memory:")
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, st
}

func segConfig(t *testing.T, upstream string) *config.Config {
	t.Helper()
	cfg := imageConfig(t, "sam3-segment", upstream, "segment")
	return cfg
}

func postSeg(r http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/segmentations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// Forwarded verbatim in both directions; costed as ONE image although the
// request carries two prompts; the capture is a summary, not the RLE body.
func TestSegmentationsForwardAndMeter(t *testing.T) {
	sam := newFakeSam(t, samReply)
	r, st := segMount(t, segConfig(t, sam.URL))

	body := strings.Replace(samReq, "%s", "412.5", 1)
	rec := postSeg(r, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != samReply {
		t.Errorf("reply not forwarded verbatim: %s", rec.Body.String())
	}
	if string(sam.last) != body {
		t.Errorf("shim saw a rewritten body: %s", sam.last)
	}
	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 activity row, got %d", len(acts))
	}
	a := acts[0]
	if a.Path != "/v1/images/segmentations" || a.Served != "sam3-segment" || a.Status != 200 {
		t.Errorf("activity = %+v", a)
	}
	want := 1 * 3.0 / 1000 * 0.14 // one image, not two prompts
	if d := a.CostUSD - want; d < -1e-12 || d > 1e-12 {
		t.Errorf("cost = %v, want %v", a.CostUSD, want)
	}
	if a.PromptTokens != 0 || a.CompletionTokens != 0 || a.AudioBytes != 0 {
		t.Errorf("segmentation metered tokens/audio: %+v", a)
	}
	full, err := st.ActivityByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.RespBody != "<1 image(s) segmented, "+itoa(len(samReply))+" bytes>" {
		t.Errorf("resp body = %q, want a summary", full.RespBody)
	}
}

// Coordinate validation is the shim's: corrallm passes the 400 and its `param`
// through unchanged, keeps the error body in the row, and bills nothing.
func TestSegmentationsPassesShimValidationThrough(t *testing.T) {
	sam := newFakeSam(t, samReply)
	r, st := segMount(t, segConfig(t, sam.URL))

	rec := postSeg(r, strings.Replace(samReq, "%s", "-1", 1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Param, Type string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Param != "prompts[1].points[0].x" {
		t.Errorf("param lost in transit: %s (%v)", rec.Body.String(), err)
	}
	acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
	if len(acts) != 1 {
		t.Fatalf("want 1 row, got %d", len(acts))
	}
	if acts[0].CostUSD != 0 {
		t.Errorf("a refused request billed %v", acts[0].CostUSD)
	}
	full, _ := st.ActivityByID(acts[0].ID)
	if !strings.Contains(full.RespBody, "x out of range") {
		t.Errorf("error body not captured: %q", full.RespBody)
	}
}

// 32 MiB is the cap: exactly at it passes, one byte over is 413 with the
// contract's type, and the shim never sees it.
func TestSegmentationsBodyCap(t *testing.T) {
	sam := newFakeSam(t, samReply)
	r, _ := segMount(t, segConfig(t, sam.URL))

	pad := func(n int) string {
		head := `{"model":"sam3-segment","image":"`
		tail := `"}`
		return head + strings.Repeat("A", n-len(head)-len(tail)) + tail
	}
	if rec := postSeg(r, pad(32<<20)); rec.Code != http.StatusOK {
		t.Fatalf("body at the cap: status %d", rec.Code)
	}
	before := sam.count()
	rec := postSeg(r, pad(32<<20+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: status %d, want 413", rec.Code)
	}
	var env struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "request_too_large" || !strings.Contains(env.Error.Message, "32 MiB") {
		t.Errorf("413 envelope = %s", rec.Body.String())
	}
	if sam.count() != before {
		t.Error("an oversize body reached the shim")
	}
}

// A chat model and a diffusion model are both refused 400 at the door, before
// any cold load or forward.
func TestSegmentationsRefusesNonSegmenter(t *testing.T) {
	for _, typ := range []string{"chat", "diffusion"} {
		sam := newFakeSam(t, samReply)
		cfg := imageConfig(t, "other", sam.URL, typ)
		r, st := segMount(t, cfg)

		rec := postSeg(r, `{"model":"other","image":"AAAA","prompts":[{"type":"concept","text":"x"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d: %s", typ, rec.Code, rec.Body.String())
		}
		if sam.count() != 0 {
			t.Errorf("%s: refused request reached the backend", typ)
		}
		acts, _ := st.RecentActivity(store.Window{}, 10, "", "", "")
		if len(acts) != 1 || !strings.Contains(acts[0].Error, "does not return mask") {
			t.Errorf("%s: activity = %+v", typ, acts)
		}
	}
	// The mirror: a segmenter is refused on image generation.
	sam := newFakeSam(t, samReply)
	r, _ := segMount(t, segConfig(t, sam.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"sam3-segment","prompt":"a fox"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || sam.count() != 0 {
		t.Errorf("segmenter on /images/generations: status %d hits %d", rec.Code, sam.count())
	}
}

// The manifest documents the route, the model that serves it, the coordinate
// rules, and the 413 in the error contract.
func TestCapabilitiesListsSegmentations(t *testing.T) {
	cfg := &config.Config{
		CommandCosts: map[string]map[string]any{"segment": {"imageWhPerImage": 3}},
		Models: map[string]config.Model{
			"qwen":         {Type: "chat"},
			"sam3-segment": {Type: "segment"},
		},
	}
	r, _ := segMount(t, cfg)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
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
		Errors struct {
			Statuses []struct {
				Status int      `json:"status"`
				Types  []string `json:"types"`
			} `json:"statuses"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if got := m.ModelsByCapability["image.segment"]; len(got) != 1 || got[0] != "sam3-segment" {
		t.Errorf("image.segment models = %v", got)
	}
	if !containsStr(m.Modalities.Vocabulary, "mask") {
		t.Errorf("vocabulary = %v, want mask", m.Modalities.Vocabulary)
	}
	found := false
	for _, ep := range m.Endpoints {
		if ep.Path != "/v1/images/segmentations" {
			continue
		}
		found = true
		if ep.Capability != "image.segment" || !containsStr(ep.Emits, "mask") || !containsStr(ep.Accepts, "image") {
			t.Errorf("row = %+v", ep)
		}
		if len(ep.Models) != 1 || ep.Models[0] != "sam3-segment" {
			t.Errorf("row models = %v", ep.Models)
		}
		curl, _ := ep.Example["curl"].(string)
		if !strings.Contains(curl, "sam3-segment") || !strings.Contains(curl, "prompts") {
			t.Errorf("curl not runnable: %q", curl)
		}
		for _, k := range []string{"coordinates", "limits", "metering"} {
			if s, _ := ep.Example[k].(string); s == "" {
				t.Errorf("example lacks %q", k)
			}
		}
	}
	if !found {
		t.Fatal("/v1/images/segmentations missing from the manifest")
	}
	has413 := false
	for _, s := range m.Errors.Statuses {
		if s.Status == 413 && containsStr(s.Types, "request_too_large") {
			has413 = true
		}
	}
	if !has413 {
		t.Error("error contract lacks 413 request_too_large")
	}
}
