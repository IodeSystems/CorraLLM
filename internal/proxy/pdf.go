package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// defaultPDFMaxChars caps the text extracted from one PDF that gets injected into
// the prompt. ~400k chars ≈ ~100k tokens — generous on a 220k-ctx model while
// guarding against a pathological document blowing the context.
const defaultPDFMaxChars = 400_000

// convertChatPDFs rewrites a /v1/chat/completions body, replacing every PDF
// content part with a text part holding the PDF's extracted text (via pdftotext).
// It returns the new body and how many PDFs were converted; on 0 it returns the
// original bytes untouched (byte-exact passthrough — no JSON churn). A PDF that
// fails to decode/extract is left as-is rather than dropped.
func convertChatPDFs(ctx context.Context, body []byte, maxChars int) ([]byte, int) {
	if maxChars <= 0 {
		maxChars = defaultPDFMaxChars
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
			text, err := pdftotext(ctx, data)
			if err != nil || strings.TrimSpace(text) == "" {
				continue // unreadable (e.g. scanned/no text) → leave the part untouched
			}
			if len(text) > maxChars {
				text = text[:maxChars] + "\n[…truncated]"
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
	comma := strings.IndexByte(s, ',')
	if comma < 0 || !strings.Contains(s[:comma], "base64") {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s[comma+1:]))
	if err != nil || len(b) < 5 || string(b[:5]) != "%PDF-" {
		return nil, false
	}
	return b, true
}

// pdftotext pipes a PDF through poppler's pdftotext (layout-preserving), reading
// stdin and writing stdout. No OCR — scanned PDFs yield little/no text.
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

func str(v any) string { s, _ := v.(string); return s }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
