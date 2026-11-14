package proxy

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// capsAndModels mounts a config where an extension declares routes and serves
// a segmenter, plus a TTS and an STT model with no declared modalities.
func capsAndModels(t *testing.T) (caps struct {
	Endpoints []struct {
		Path                 string    `json:"path"`
		Method               string    `json:"method"`
		RequestContentTypes  *[]string `json:"request_content_types"`
		ResponseContentTypes *[]string `json:"response_content_types"`
	} `json:"endpoints"`
}, models map[string]struct {
	Modalities    map[string]config.ModalitySpec `json:"modalities"`
	Outputs       []string                       `json:"outputs"`
	OutputFormats map[string][]string            `json:"output_formats"`
}) {
	t.Helper()
	ext := newFakeColmap(t)
	port := ext.URL[strings.LastIndex(ext.URL, ":")+1:]
	body := `
priorityGroups: { default: { weight: 1 } }
models:
  voice: { proxy: ` + port + `, type: tts }
  ears:  { proxy: ` + port + `, type: stt }
extensions:
  colmap:
    proxy: ` + port + `
    routes: [ /v1/files, /v1/uploads, /v1/reconstructions ]
    provides:
      seg: { type: segment, upstream: seg, modalities: { image: { formats: [jpeg, png, webp], maxCount: 16 }, text: {} } }
`
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := extAPIMount(t, cfg)
	rec := extReq(r, http.MethodGet, "/v1/capabilities", "", nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []struct {
			ID            string                         `json:"id"`
			Modalities    map[string]config.ModalitySpec `json:"modalities"`
			Outputs       []string                       `json:"outputs"`
			OutputFormats map[string][]string            `json:"output_formats"`
		} `json:"data"`
	}
	rec = extReq(r, http.MethodGet, "/v1/models", "", nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	models = map[string]struct {
		Modalities    map[string]config.ModalitySpec `json:"modalities"`
		Outputs       []string                       `json:"outputs"`
		OutputFormats map[string][]string            `json:"output_formats"`
	}{}
	for _, m := range list.Data {
		models[m.ID] = struct {
			Modalities    map[string]config.ModalitySpec `json:"modalities"`
			Outputs       []string                       `json:"outputs"`
			OutputFormats map[string][]string            `json:"output_formats"`
		}{m.Modalities, m.Outputs, m.OutputFormats}
	}
	return
}

// Every row names its wire formats; a route that reads a body names one.
func TestCapabilitiesRowsCarryContentTypes(t *testing.T) {
	caps, _ := capsAndModels(t)
	byPath := map[string][]string{}
	for _, e := range caps.Endpoints {
		if e.RequestContentTypes == nil || e.ResponseContentTypes == nil {
			t.Errorf("%s %s: content-type fields missing", e.Method, e.Path)
			continue
		}
		if len(*e.ResponseContentTypes) == 0 {
			t.Errorf("%s: no response_content_types", e.Path)
		}
		if e.Method == "POST" && len(*e.RequestContentTypes) == 0 {
			t.Errorf("%s: POST row with no request_content_types", e.Path)
		}
		byPath[e.Path] = append(*e.RequestContentTypes, "->")
		byPath[e.Path] = append(byPath[e.Path], *e.ResponseContentTypes...)
	}
	has := func(path string, want ...string) {
		got := strings.Join(byPath[path], " ")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %s", path, got, w)
			}
		}
	}
	has("/v1/audio/transcriptions", "multipart/form-data", "text/event-stream")
	has("/v1/audio/speech", "application/json", "audio/*")
	has("/v1/images/segmentations", "application/json", "multipart/form-data")
	has("/v1/chat/completions", "text/event-stream")
	// Extension-declared routes are emitted, with their formats.
	has("/v1/files", "multipart/form-data")
	has("/v1/uploads", "application/json")
	has("/v1/reconstructions", "application/zip")
}

// A TTS model accepts text; an STT model accepts audio; declared sam3 image
// formats and count pass through, and the mask encodings are published.
func TestModelsPublishAccurateIO(t *testing.T) {
	_, models := capsAndModels(t)
	if _, ok := models["voice"].Modalities["text"]; !ok || len(models["voice"].Modalities) != 1 {
		t.Errorf("tts modalities = %v, want text only", models["voice"].Modalities)
	}
	if models["voice"].Outputs[0] != "audio" {
		t.Errorf("tts outputs = %v", models["voice"].Outputs)
	}
	if _, ok := models["ears"].Modalities["audio"]; !ok || len(models["ears"].Modalities) != 1 {
		t.Errorf("stt modalities = %v, want audio only", models["ears"].Modalities)
	}
	img := models["colmap-seg"].Modalities["image"]
	if strings.Join(img.Formats, ",") != "jpeg,png,webp" || img.MaxCount != 16 {
		t.Errorf("segment image spec = %+v", img)
	}
	if got := models["colmap-seg"].OutputFormats["mask"]; strings.Join(got, ",") != "rle,png" {
		t.Errorf("segment output_formats = %v", models["colmap-seg"].OutputFormats)
	}
	if models["ears"].OutputFormats != nil {
		t.Errorf("stt output_formats = %v", models["ears"].OutputFormats)
	}
}
