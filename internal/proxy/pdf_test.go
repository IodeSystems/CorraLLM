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
	out, n := convertChatPDFs(context.Background(), body, pdfOpts{})
	if n != 0 || string(out) != string(body) {
		t.Errorf("plain chat should be untouched: n=%d", n)
	}
	// non-chat-shaped json mentioning pdf → still safe no-op
	if _, n := convertChatPDFs(context.Background(), []byte(`{"x":"application/pdf"}`), pdfOpts{}); n != 0 {
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
	out, n := convertChatPDFs(context.Background(), body, pdfOpts{})
	if n != 1 {
		t.Fatalf("expected 1 PDF converted, got %d", n)
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	// all-text content is flattened to a single string (a multimodal model refuses
	// an all-text content array).
	content := req["messages"].([]any)[0].(map[string]any)["content"]
	text, ok := content.(string)
	if !ok {
		t.Fatalf("converted all-text content should be a string, got %T", content)
	}
	if !strings.Contains(text, "marker-12345") {
		t.Errorf("extracted text missing marker: %q", text)
	}
	if !strings.Contains(text, "sample.pdf") {
		t.Errorf("should name the file: %q", text)
	}
	if !strings.Contains(text, "summarize this") {
		t.Errorf("original text should be preserved: %q", text)
	}
}

func TestConvertChatPDFsKeepsImageAsArray(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	pdf, _ := os.ReadFile("testdata/sample.pdf")
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	// a message with BOTH a PDF and an image: the PDF → text, but content must stay
	// an array because the image needs the multimodal path.
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "file", "file": map[string]any{"file_data": url}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}}},
	})
	out, n := convertChatPDFs(context.Background(), body, pdfOpts{})
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	if _, isArray := req["messages"].([]any)[0].(map[string]any)["content"].([]any); !isArray {
		t.Error("content with an image must stay an array, not flatten")
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
	out, n := convertChatPDFs(context.Background(), body, pdfOpts{maxChars: 5}) // tiny cap
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	text := req["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(text, "truncated") {
		t.Errorf("tiny cap should truncate: %q", text)
	}
}

func TestConvertChatPDFsOCR(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not installed — OCR fallback path can't run")
	}
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed")
	}
	// scanned.pdf is an image-only PDF (no text layer): pdftotext finds nothing, so
	// extraction must fall through to OCR.
	pdf, err := os.ReadFile("testdata/scanned.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if txt, _ := pdftotext(context.Background(), pdf); strings.TrimSpace(txt) != "" {
		t.Skipf("fixture is not text-less (pdftotext got %q) — OCR wouldn't trigger", txt)
	}
	url := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	body, _ := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "file", "file": map[string]any{"file_data": url}}}}},
	})
	out, n := convertChatPDFs(context.Background(), body, pdfOpts{ocr: true})
	if n != 1 {
		t.Fatalf("OCR should have converted the scanned PDF, n=%d", n)
	}
	var req map[string]any
	_ = json.Unmarshal(out, &req)
	text := req["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(text, "marker-12345") {
		t.Errorf("OCR text missing marker: %q", text)
	}
}

func TestExtractNoOCRWhenDisabled(t *testing.T) {
	pdf, _ := os.ReadFile("testdata/scanned.pdf")
	// ocr:false → no OCR even if tesseract exists; scanned PDF yields ~nothing.
	txt, _ := extractPDFText(context.Background(), pdf, pdfOpts{ocr: false})
	if strings.TrimSpace(txt) != "" {
		t.Errorf("with OCR disabled, scanned PDF should yield no text, got %q", txt)
	}
}
