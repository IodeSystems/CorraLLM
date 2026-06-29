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

	"github.com/iodesystems/corrallm/internal/config"
)

// minTextChars: below this, pdftotext effectively found nothing → treat the PDF as
// scanned/image-only and try OCR (text strategy).
const minTextChars = 16

// convertChatPDFs rewrites a /v1/chat/completions body, replacing each PDF content
// part with its ingested form per cfg: extracted text (text strategy) or rasterized
// page images (vision strategy). Returns the new body and how many PDFs were
// converted; on 0 it returns the original bytes untouched. A PDF that can't be read
// is left as-is rather than dropped.
func convertChatPDFs(ctx context.Context, body []byte, cfg config.ConvertConfig) ([]byte, int) {
	if cfg.PDF == "off" || !bytes.Contains(body, []byte("application/pdf")) {
		return body, 0
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
		out := make([]any, 0, len(parts))
		msgConverted := false
		for _, pi := range parts {
			part, isMap := pi.(map[string]any)
			if !isMap {
				out = append(out, pi)
				continue
			}
			data, name, isPDF := pdfFromPart(part)
			if !isPDF {
				out = append(out, pi)
				continue
			}
			repl, ok := convertOnePDF(ctx, data, name, cfg)
			if !ok {
				out = append(out, pi) // unreadable (even via OCR) → leave untouched
				continue
			}
			out = append(out, repl...)
			converted++
			msgConverted = true
		}
		if msgConverted {
			// The text strategy yields only text parts; a multimodal model refuses an
			// all-text content ARRAY, so flatten it to a string. The vision strategy
			// adds image parts → stays an array (the multimodal path needs it).
			if s, allText := flattenAllText(out); allText {
				msg["content"] = s
			} else {
				msg["content"] = out
			}
		}
	}
	if converted == 0 {
		return body, 0
	}
	nb, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return nb, converted
}

// convertOnePDF produces the replacement content part(s) for one PDF: a single
// text part (text strategy) or a caption + N image_url parts (vision strategy).
func convertOnePDF(ctx context.Context, pdf []byte, name string, cfg config.ConvertConfig) ([]any, bool) {
	label := name
	if label == "" {
		label = "document.pdf"
	}
	if cfg.PDF == "vision" {
		urls, err := rasterizePDF(ctx, pdf, cfg)
		if err != nil || len(urls) == 0 {
			return nil, false
		}
		out := make([]any, 0, len(urls)+1)
		out = append(out, map[string]any{"type": "text",
			"text": "Attached file \"" + label + "\" rendered to " + strconv.Itoa(len(urls)) + " page image(s):"})
		for _, u := range urls {
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		return out, true
	}
	// text strategy (default)
	text, err := extractPDFText(ctx, pdf, cfg)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil, false
	}
	if cfg.MaxChars > 0 && len(text) > cfg.MaxChars {
		text = text[:cfg.MaxChars] + "\n[…truncated]"
	}
	return []any{map[string]any{"type": "text", "text": pdfTextBlock(label, text)}}, true
}

// pdfTextBlock frames extracted text as the file's content — NOT an "[attached
// file]" the model claims it "can't access" (which makes instruction-tuned models
// refuse).
func pdfTextBlock(name, text string) string {
	return "The attached file \"" + name + "\" was extracted to text; its contents follow:\n---\n" +
		strings.TrimSpace(text) + "\n---"
}

// flattenAllText joins a content-parts array into one string iff every part is
// text. ok=false (leave as an array) if any part is non-text (e.g. an image).
func flattenAllText(parts []any) (string, bool) {
	var sb strings.Builder
	for _, pi := range parts {
		pm, ok := pi.(map[string]any)
		if !ok || pm["type"] != "text" {
			return "", false
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(str(pm["text"]))
	}
	return sb.String(), true
}

// extractPDFText pulls text from a PDF: pdftotext first (fast, exact for text
// PDFs); if that finds ~nothing (scanned) and OCR is enabled + available, falls
// back to rasterize + tesseract.
func extractPDFText(ctx context.Context, pdf []byte, cfg config.ConvertConfig) (string, error) {
	text, err := pdftotext(ctx, pdf)
	if err == nil && len(strings.TrimSpace(text)) >= minTextChars {
		return text, nil
	}
	if cfg.OCREnabled() && haveTesseract() {
		if ocrText, oerr := ocrPDF(ctx, pdf, cfg); oerr == nil && strings.TrimSpace(ocrText) != "" {
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
	if f, isMap := part["file"].(map[string]any); isMap {
		filename, _ = f["filename"].(string)
		if d, fok := pdfFromDataURL(str(f["file_data"])); fok {
			return d, filename, true
		}
	}
	if typ == "input_file" || part["file_data"] != nil {
		filename = firstNonEmpty(str(part["filename"]), filename)
		if d, fok := pdfFromDataURL(str(part["file_data"])); fok {
			return d, filename, true
		}
	}
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

// pdftotext pipes a PDF through poppler's pdftotext (layout-preserving).
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

// rasterizePages renders the first cfg.MaxPages pages of a PDF to image files in a
// temp dir and returns their paths (+ a cleanup func). Shared by the vision
// strategy and the OCR fallback. jpeg unless cfg.Format is png.
func rasterizePages(ctx context.Context, pdf []byte, dpi, quality int, format string, maxPages int) (paths []string, cleanup func(), err error) {
	if dpi <= 0 {
		dpi = 200
	}
	if maxPages <= 0 {
		maxPages = 20
	}
	png := format == "png"
	dir, err := os.MkdirTemp("", "corrallm-pdf-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	args := []string{"-r", strconv.Itoa(dpi), "-l", strconv.Itoa(maxPages)}
	ext := "jpg"
	if png {
		args = append(args, "-png")
		ext = "png"
	} else {
		args = append(args, "-jpeg")
		if quality > 0 {
			args = append(args, "-jpegopt", "quality="+strconv.Itoa(quality))
		}
	}
	args = append(args, in, filepath.Join(dir, "page"))
	if out, err := exec.CommandContext(ctx, "pdftoppm", args...).CombinedOutput(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("pdftoppm: %v: %s", err, strings.TrimSpace(string(out)))
	}
	pages, _ := filepath.Glob(filepath.Join(dir, "page*."+ext))
	sort.Strings(pages)
	return pages, cleanup, nil
}

// rasterizePDF renders a PDF's pages to base64 image data URLs (vision strategy).
func rasterizePDF(ctx context.Context, pdf []byte, cfg config.ConvertConfig) ([]string, error) {
	paths, cleanup, err := rasterizePages(ctx, pdf, cfg.DPI, cfg.Quality, cfg.Format, cfg.MaxPages)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	mime := "image/jpeg"
	if cfg.Format == "png" {
		mime = "image/png"
	}
	urls := make([]string, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		urls = append(urls, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(b))
	}
	return urls, nil
}

// ocrPDF rasterizes a scanned PDF to PNG pages and OCRs each with tesseract.
func ocrPDF(ctx context.Context, pdf []byte, cfg config.ConvertConfig) (string, error) {
	paths, cleanup, err := rasterizePages(ctx, pdf, cfg.DPI, 0, "png", cfg.MaxPages) // PNG for OCR fidelity
	if err != nil {
		return "", err
	}
	defer cleanup()
	var b strings.Builder
	for _, p := range paths {
		out, err := exec.CommandContext(ctx, "tesseract", p, "stdout", "-l", "eng").Output()
		if err != nil {
			continue // skip a page tesseract chokes on rather than failing the doc
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
