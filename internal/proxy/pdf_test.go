package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPdfFromDataURL(t *testing.T) {
	pdf := []byte("%PDF-1.4 hello")
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	if d, ok := pdfFromDataURL(url); !ok || string(d) != string(pdf) {
		t.Errorf("valid pdf data URL: ok=%v data=%q", ok, d)
	}
	if _, ok := pdfFromDataURL("data:image/png;base64,AAAA"); ok {
		t.Error("non-pdf mime should not match")
	}
	if _, ok := pdfFromDataURL("data:application/pdf;base64,not!base64!"); ok {
		t.Error("bad base64 should not match")
	}
	// right mime but the bytes aren't actually a PDF (no %PDF- magic)
	if _, ok := pdfFromDataURL("data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("nope"))); ok {
		t.Error("missing %PDF- magic should not match")
	}
	if _, ok := pdfFromDataURL("https://example.com/x.pdf"); ok {
		t.Error("non-data URL should not match")
	}
}

func TestPdfFromPartShapes(t *testing.T) {
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 x"))
	shapes := []map[string]any{
		{"type": "file", "file": map[string]any{"filename": "a.pdf", "file_data": url}},
		{"type": "input_file", "filename": "b.pdf", "file_data": url},
		{"type": "image_url", "image_url": map[string]any{"url": url}},
	}
	for i, part := range shapes {
		if _, _, ok := pdfFromPart(part); !ok {
			t.Errorf("shape %d should be detected as a PDF", i)
		}
	}
	// a plain text part is not a PDF
	if _, _, ok := pdfFromPart(map[string]any{"type": "text", "text": "hi"}); ok {
		t.Error("text part should not be a PDF")
	}
}

func TestConvertChatPDFsNoOp(t *testing.T) {
	// no "application/pdf" anywhere → byte-exact passthrough
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	out, n := convertChatPDFs(context.Background(), body, 0)
	if n != 0 || string(out) != string(body) {
		t.Errorf("plain chat should be untouched: n=%d", n)
	}
	// non-chat-shaped json mentioning pdf → still safe no-op
	if _, n := convertChatPDFs(context.Background(), []byte(`{"x":"application/pdf"}`), 0); n != 0 {
		t.Errorf("non-messages body should convert nothing: n=%d", n)
	}
}

func TestConvertChatPDFsExtracts(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	pdf, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "summarize this"},
				map[string]any{"type": "file", "file": map[string]any{"filename": "sample.pdf", "file_data": url}},
			}},
		},
	})
	out, n := convertChatPDFs(context.Background(), body, 0)
	if n != 1 {
		t.Fatalf("expected 1 PDF converted, got %d", n)
	}
	// the file part must be gone, replaced by text holding the extracted marker
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	parts := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	last := parts[1].(map[string]any)
	if last["type"] != "text" {
		t.Fatalf("PDF part should become text, got %v", last["type"])
	}
	text := last["text"].(string)
	if !strings.Contains(text, "marker-12345") {
		t.Errorf("extracted text missing marker: %q", text)
	}
	if !strings.Contains(text, "sample.pdf") {
		t.Errorf("injected text should name the file: %q", text)
	}
	// the original text part is preserved
	if parts[0].(map[string]any)["text"] != "summarize this" {
		t.Error("original text part should be preserved")
	}
}

func TestConvertChatPDFsTruncates(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	pdf, _ := os.ReadFile("testdata/sample.pdf")
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	body, _ := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "file", "file": map[string]any{"file_data": url}}}}},
	})
	out, n := convertChatPDFs(context.Background(), body, 5) // tiny cap
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	text := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "truncated") {
		t.Errorf("tiny cap should truncate: %q", text)
	}
}
