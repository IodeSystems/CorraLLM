package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	// defaultPDFMaxChars caps the text extracted from one PDF that gets injected
	// into the prompt. ~400k chars ≈ ~100k tokens — generous on a 220k-ctx model
	// while guarding against a pathological document blowing the context.
	defaultPDFMaxChars = 400_000
	// defaultOCRMaxPages bounds OCR latency on a long scanned document.
	defaultOCRMaxPages = 20
	// minTextChars: below this, pdftotext effectively found nothing → treat the PDF
	// as scanned/image-only and try OCR.
	minTextChars = 16
)

// pdfOpts carries the per-request PDF conversion config (from Proxy flags).
type pdfOpts struct {
	maxChars    int
	ocr         bool
	ocrMaxPages int
}

// convertChatPDFs rewrites a /v1/chat/completions body, replacing every PDF
// content part with a text part holding the PDF's extracted text. It returns the
// new body and how many PDFs were converted; on 0 it returns the original bytes
// untouched (byte-exact passthrough). A PDF that yields no text (even via OCR) is
// left as-is rather than dropped.
func convertChatPDFs(ctx context.Context, body []byte, o pdfOpts) ([]byte, int) {
	if o.maxChars <= 0 {
		o.maxChars = defaultPDFMaxChars
	}
	if !bytes.Contains(body, []byte("application/pdf")) {
		return body, 0 // fast path: no PDF data URL anywhere
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, 0
	}
	msgs, ok := req["messages"].([]any)
	if !ok {
		return body, 0
	}

	converted := 0
	for _, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue // string content (or none) → no attachments
		}
		for i, pi := range parts {
			part, ok := pi.(map[string]any)
			if !ok {
				continue
			}
			data, name, ok := pdfFromPart(part)
			if !ok {
				continue
			}
			text, err := extractPDFText(ctx, data, o)
			if err != nil || strings.TrimSpace(text) == "" {
				continue // unreadable even with OCR → leave the part untouched
			}
			if len(text) > o.maxChars {
				text = text[:o.maxChars] + "\n[…truncated]"
			}
			label := name
			if label == "" {
				label = "document.pdf"
			}
			parts[i] = map[string]any{"type": "text", "text": "[attached file: " + label + "]\n" + text}
			converted++
		}
	}
	if converted == 0 {
		return body, 0
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return out, converted
}

// extractPDFText pulls text from a PDF: pdftotext first (fast, exact for text
// PDFs); if that finds ~nothing (a scanned/image PDF) and OCR is enabled +
// available, falls back to rasterize + tesseract.
func extractPDFText(ctx context.Context, pdf []byte, o pdfOpts) (string, error) {
	text, err := pdftotext(ctx, pdf)
	if err == nil && len(strings.TrimSpace(text)) >= minTextChars {
		return text, nil
	}
	if o.ocr && haveTesseract() {
		if ocrText, oerr := ocrPDF(ctx, pdf, o.ocrMaxPages); oerr == nil && strings.TrimSpace(ocrText) != "" {
			return ocrText, nil
		}
	}
	return text, err
}

// pdfFromPart extracts PDF bytes + a filename from a content part, across the
// shapes clients use: an OpenAI `file` part, an `input_file` part, or an
// `image_url`/`file_data` whose data URL is application/pdf. ok=false if not a PDF.
func pdfFromPart(part map[string]any) (data []byte, filename string, ok bool) {
	typ, _ := part["type"].(string)

	// {"type":"file","file":{"filename":..,"file_data":"data:application/pdf;base64,.."}}
	if f, isMap := part["file"].(map[string]any); isMap {
		filename, _ = f["filename"].(string)
		if d, fok := pdfFromDataURL(str(f["file_data"])); fok {
			return d, filename, true
		}
	}
	// {"type":"input_file","filename":..,"file_data":".."} (data URL at the top level)
	if typ == "input_file" || part["file_data"] != nil {
		filename = firstNonEmpty(str(part["filename"]), filename)
		if d, fok := pdfFromDataURL(str(part["file_data"])); fok {
			return d, filename, true
		}
	}
	// {"type":"image_url","image_url":{"url":"data:application/pdf;base64,.."}} (misuse)
	if iu, isMap := part["image_url"].(map[string]any); isMap {
		if d, fok := pdfFromDataURL(str(iu["url"])); fok {
			return d, filename, true
		}
	}
	return nil, "", false
}

// pdfFromDataURL decodes a "data:application/pdf;base64,<b64>" URL to bytes.
func pdfFromDataURL(s string) ([]byte, bool) {
	if !strings.HasPrefix(s, "data:") || !strings.Contains(s, "application/pdf") {
		return nil, false
	}
	_, b64, found := strings.Cut(s, ",")
	if !found || !strings.Contains(s[:len(s)-len(b64)], "base64") {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(b) < 5 || string(b[:5]) != "%PDF-" {
		return nil, false
	}
	return b, true
}

// pdftotext pipes a PDF through poppler's pdftotext (layout-preserving), reading
// stdin and writing stdout.
func pdftotext(ctx context.Context, pdf []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-q", "-", "-")
	cmd.Stdin = bytes.NewReader(pdf)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// ocrPDF rasterizes the first ocrMaxPages of a scanned PDF (pdftoppm @200dpi) and
// OCRs each page with tesseract, concatenating the text. Page count is capped to
// bound latency; the request context cancels the subprocesses.
func ocrPDF(ctx context.Context, pdf []byte, maxPages int) (string, error) {
	if maxPages <= 0 {
		maxPages = defaultOCRMaxPages
	}
	dir, err := os.MkdirTemp("", "corrallm-ocr-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		return "", err
	}
	rast := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "200", "-l", strconv.Itoa(maxPages), in, filepath.Join(dir, "page"))
	if out, err := rast.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm: %v: %s", err, strings.TrimSpace(string(out)))
	}
	pages, _ := filepath.Glob(filepath.Join(dir, "page*.png"))
	sort.Strings(pages)

	var b strings.Builder
	for _, pg := range pages {
		out, err := exec.CommandContext(ctx, "tesseract", pg, "stdout", "-l", "eng").Output()
		if err != nil {
			continue // skip a page tesseract chokes on rather than failing the whole doc
		}
		b.Write(bytes.TrimSpace(out))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

var (
	tesseractOnce sync.Once
	tesseractOK   bool
)

// haveTesseract reports whether the tesseract OCR binary is on PATH (checked once).
func haveTesseract() bool {
	tesseractOnce.Do(func() {
		_, err := exec.LookPath("tesseract")
		tesseractOK = err == nil
	})
	return tesseractOK
}

func str(v any) string { s, _ := v.(string); return s }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
