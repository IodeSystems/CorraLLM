package proxy

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func buildMultipart(t *testing.T, model string, file []byte) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if model != "" {
		if err := w.WriteField("model", model); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteField("response_format", "verbose_json"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("file", "hearing.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), w.FormDataContentType()
}

func parts(t *testing.T, body []byte, ct string) (map[string]string, []byte, string) {
	t.Helper()
	_, ps, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), ps["boundary"])
	fields := map[string]string{}
	var fileBytes []byte
	var fileName string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			fileBytes, fileName = b, p.FileName()
			continue
		}
		fields[p.FormName()] = string(b)
	}
	return fields, fileBytes, fileName
}

// The audio path had no model rewrite at all, so every transcription forwarded
// the SERVED name and oidio 404'd a model it had never heard of.
func TestRewriteModelMultipartSwapsModelAndKeepsTheFile(t *testing.T) {
	audio := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 4096)
	body, ct := buildMultipart(t, "oidio-stt-diarize", audio)

	out, newCT, ok := rewriteModelMultipart(body, ct, "stt-diarize")
	if !ok {
		t.Fatal("rewrite reported failure")
	}
	fields, gotFile, gotName := parts(t, out, newCT)

	if fields["model"] != "stt-diarize" {
		t.Errorf("model = %q, want %q", fields["model"], "stt-diarize")
	}
	if fields["response_format"] != "verbose_json" {
		t.Errorf("other fields must survive: response_format = %q", fields["response_format"])
	}
	if !bytes.Equal(gotFile, audio) {
		t.Errorf("file corrupted: %d bytes out, %d in", len(gotFile), len(audio))
	}
	if gotName != "hearing.mp4" {
		t.Errorf("filename = %q, want hearing.mp4", gotName)
	}
}

// Forwarding an unmodified body is recoverable; corrupting one is not. Every
// failure path must leave the caller's body untouched.
func TestRewriteModelMultipartFailsClosed(t *testing.T) {
	audio := []byte("audio")
	withModel, ct := buildMultipart(t, "oidio-stt", audio)
	noModel, ctNo := buildMultipart(t, "", audio)

	cases := []struct {
		name, ct string
		body     []byte
	}{
		{"no model field", ctNo, noModel},
		{"not multipart", "application/json", []byte(`{"model":"x"}`)},
		{"missing boundary", "multipart/form-data", withModel},
		{"truncated body", ct, withModel[:len(withModel)/3]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := rewriteModelMultipart(tc.body, tc.ct, "stt"); ok {
				t.Fatal("expected failure, got ok=true")
			}
		})
	}
}
