package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"gopkg.in/yaml.v3"
)

// The PDF rule per backend (owner, 2026-11-06):
//  1. a backend that reads PDFs natively (api: anthropic) gets the part as is,
//     even when a strategy is set;
//  2. otherwise the configured strategy applies, and "off" passes it through;
//  3. otherwise the built-in default, text.

// pdfRig serves "claude" (api anthropic, with an explicit pdf: text that rule 1
// overrides), "local" (no convert block), "localoff" (pdf: off), and a lane
// "mix" = [claude, local]. It returns the body the OpenAI-shaped upstream got.
func pdfRig(t *testing.T, f *fakeAnthropic) (*chi.Mux, *[]byte) {
	t.Helper()
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(up.Close)

	u, _ := url.Parse(f.srv.URL)
	var doc yaml.Node
	src := fmt.Sprintf("host: %s\nport: %s\napi: anthropic\nmodel: claude-up\nheaders:\n  x-api-key: test-key\n", u.Hostname(), u.Port())
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	claude := config.Model{Proxy: *doc.Content[0], Convert: &config.ConvertConfig{PDF: "text"}}
	off := modelTo(t, up.URL, "local")
	off.Convert = &config.ConvertConfig{PDF: "off"}
	cfg := &config.Config{
		Models: map[string]config.Model{"claude": claude, "local": modelTo(t, up.URL, "local"), "localoff": off},
		Lanes: map[string]config.Lane{
			"mix": {Members: []config.LaneMember{{Model: "claude"}, {Model: "local"}}},
		},
		PriorityGroups: map[string]config.PriorityGroup{"default": {Weight: 1}},
		Keys:           map[string]config.KeyPolicy{"k": {Group: "default"}},
	}
	r := chi.NewRouter()
	New(cfg, mgr, sched.New(), st).Mount(r)
	return r, &got
}

func postPDF(t *testing.T, r *chi.Mux, model string) *httptest.ResponseRecorder {
	t.Helper()
	pdf, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "summarize"},
			map[string]any{"type": "file", "file": map[string]any{"filename": "sample.pdf",
				"file_data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)}},
		}},
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func needPdftotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
}

// Rule 1: Claude gets a native document block although its convert says text.
func TestPDFNativeBackendGetsDocumentEvenWithStrategy(t *testing.T) {
	f := newFakeAnthropic(t)
	r, _ := pdfRig(t, f)
	if rec := postPDF(t, r, "claude"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	b, _ := json.Marshal(f.body)
	if !strings.Contains(string(b), `"type":"document"`) || !strings.Contains(string(b), `"media_type":"application/pdf"`) {
		t.Errorf("Claude should get a native PDF document block: %s", b)
	}
}

// Rule 3: no strategy anywhere → the built-in text default converts.
func TestPDFDefaultStrategyIsText(t *testing.T) {
	needPdftotext(t)
	f := newFakeAnthropic(t)
	r, got := pdfRig(t, f)
	if rec := postPDF(t, r, "local"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(string(*got), "application/pdf") || !strings.Contains(string(*got), "sample.pdf") {
		t.Errorf("local should get extracted text, not the PDF: %.300s", *got)
	}
}

// Rule 2: an explicit off passes the part through.
func TestPDFStrategyOffPassesThrough(t *testing.T) {
	f := newFakeAnthropic(t)
	r, got := pdfRig(t, f)
	if rec := postPDF(t, r, "localoff"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(string(*got), "data:application/pdf;base64,") {
		t.Errorf("pdf: off should forward the PDF part unchanged: %.300s", *got)
	}
}

// Per candidate: in a lane of [claude, local], Claude gets the PDF and, when
// it fails, the local fallback gets the converted text.
func TestPDFLaneChoosesPerCandidate(t *testing.T) {
	needPdftotext(t)
	f := newFakeAnthropic(t)
	r, got := pdfRig(t, f)
	if rec := postPDF(t, r, "mix"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if b, _ := json.Marshal(f.body); !strings.Contains(string(b), `"type":"document"`) {
		t.Errorf("lane's Claude member should get the native PDF: %s", b)
	}
	if len(*got) != 0 {
		t.Errorf("local should not have been called when Claude answered")
	}

	// The fakes are on loopback; a remote's 503 is what spills the walk.
	treatAsRemote(t)
	f.status, f.errBody = http.StatusServiceUnavailable, `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`
	if rec := postPDF(t, r, "mix"); rec.Code != http.StatusOK || len(*got) == 0 {
		t.Fatalf("lane should fall through to local: status %d, local called %v: %s", rec.Code, len(*got) > 0, rec.Body)
	}
	if strings.Contains(string(*got), "application/pdf") || !strings.Contains(string(*got), "sample.pdf") {
		t.Errorf("local fallback should get extracted text: %.300s", *got)
	}
}
