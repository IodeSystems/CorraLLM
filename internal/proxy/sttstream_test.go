package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// sttRig stands up a proxy in front of one mock STT backend and posts a
// multipart transcription. stream carries the form flag the backend ignores,
// exactly as oidio ignores it.
func sttRig(t *testing.T, path string, stream bool, h http.HandlerFunc) *flushRecorder {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}))
	defer upstream.Close()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := proc.NewManager(&config.Config{})
	defer mgr.Shutdown()

	r := chi.NewRouter()
	New(mkConfig(t, "stt-mock", upstream.URL), mgr, sched.New(), st).Mount(r)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "stt-mock")
	if stream {
		_ = mw.WriteField("stream", "true")
	}
	fw, _ := mw.CreateFormFile("file", "speech.wav")
	_, _ = fw.Write([]byte("fake-wav"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := &flushRecorder{}
	r.ServeHTTP(rec, req)
	return rec
}

// sseEvents splits a synthesised stream into its decoded data: payloads,
// dropping the [DONE] sentinel but reporting whether it was last.
func sseEvents(t *testing.T, body string) ([]map[string]any, bool) {
	t.Helper()
	var out []map[string]any
	done := false
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		data := strings.TrimPrefix(strings.TrimSpace(frame), "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		if done {
			t.Errorf("event after [DONE]: %s", data)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event is not JSON: %q (%v)", data, err)
		}
		out = append(out, m)
	}
	return out, done
}

// The live oidio-stt-diarize shape, verified on the wire 2026-09-23: a
// per-request speaker UUID on each segment, voiceprints on a top-level
// speakers[], and a plain `text` so an OpenAI client already works.
const oidioDiarizeBody = `{
  "duration": 2.56,
  "language": "en",
  "task": "transcribe",
  "text": "The quick brown fox. Jumps over the lazy dog.",
  "segments": [
    {"id":0,"start":0,"end":1.2,"text":"The quick brown fox.","speaker":"3f1c2a10-0000-4000-8000-000000000001"},
    {"id":1,"start":1.2,"end":2.56,"text":"Jumps over the lazy dog.","speaker":"3f1c2a10-0000-4000-8000-000000000002"}
  ],
  "speakers": [
    {"uuid":"3f1c2a10-0000-4000-8000-000000000001","embedding":[0.1,0.2],"clean_seconds":1.2,"total_seconds":1.2}
  ]
}`

func jsonBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// TestSTTStreamSynthesisDiarized is the decided behaviour of P35 §9.1: the
// backend answers a stream=true request with a finished JSON body, and corrallm
// emits the OpenAI transcription event sequence instead of forwarding JSON.
func TestSTTStreamSynthesisDiarized(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, jsonBody(oidioDiarizeBody))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.code, rec.body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	// A Content-Length describing the JSON would truncate the stream.
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length survived: %q", cl)
	}
	events, done := sseEvents(t, rec.body.String())
	if !done {
		t.Fatalf("stream not terminated by [DONE]: %s", rec.body.String())
	}
	var types []string
	for _, e := range events {
		types = append(types, e["type"].(string))
	}
	want := []string{
		"transcript.text.delta", "transcript.text.segment",
		"transcript.text.delta", "transcript.text.segment",
		"transcript.text.done",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	// Deltas carry the segment they belong to, so a client can correlate.
	if got := events[0]["segment_id"]; got != "0" {
		t.Errorf("delta segment_id = %v, want \"0\"", got)
	}
	// The speaker label is the backend's, rendered as a string, not renumbered:
	// stabilising or relabelling it is a §12 decision, not this transform's.
	seg := events[1]
	if seg["speaker"] != "3f1c2a10-0000-4000-8000-000000000001" {
		t.Errorf("speaker = %v", seg["speaker"])
	}
	if seg["id"] != "0" || seg["start"] != 0.0 || seg["end"] != 1.2 {
		t.Errorf("segment id/start/end = %v/%v/%v", seg["id"], seg["start"], seg["end"])
	}
	// done carries the whole transcript plus the one usage an STT reply has.
	fin := events[len(events)-1]
	if fin["text"] != "The quick brown fox. Jumps over the lazy dog." {
		t.Errorf("done text = %v", fin["text"])
	}
	u, ok := fin["usage"].(map[string]any)
	if !ok || u["type"] != "duration" || u["seconds"] != 2.56 {
		t.Errorf("done usage = %v", fin["usage"])
	}
}

// TestSTTStreamSynthesisPlain: no segments, so one delta and a done. The
// chunking is the data's — nothing splits a finished string into fake words.
func TestSTTStreamSynthesisPlain(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true,
		jsonBody(`{"text":"The quick brown fox jumps over the lazy dog."}`))
	events, done := sseEvents(t, rec.body.String())
	if !done || len(events) != 2 {
		t.Fatalf("events = %v (done=%v), want one delta + one done", events, done)
	}
	if events[0]["type"] != "transcript.text.delta" ||
		events[0]["delta"] != "The quick brown fox jumps over the lazy dog." {
		t.Errorf("delta = %v", events[0])
	}
	if _, has := events[0]["segment_id"]; has {
		t.Errorf("segment_id on a response with no segments: %v", events[0])
	}
	if events[1]["type"] != "transcript.text.done" {
		t.Errorf("last event = %v", events[1])
	}
	if _, has := events[1]["usage"]; has {
		t.Errorf("usage invented from a body with no duration: %v", events[1])
	}
}

// TestSTTStreamSegmentsWithoutSpeaker: verbose_json from a NON-diarizing model
// has segments but no speaker. The segment event exists to say who spoke, so
// there is nothing for it to carry — deltas only, no empty speaker label.
func TestSTTStreamSegmentsWithoutSpeaker(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, jsonBody(
		`{"text":"one two","duration":2,"segments":[{"id":0,"start":0,"end":1,"text":"one "},{"id":1,"start":1,"end":2,"text":"two"}]}`))
	events, _ := sseEvents(t, rec.body.String())
	for _, e := range events {
		if e["type"] == "transcript.text.segment" {
			t.Fatalf("segment event with no speaker: %v", e)
		}
	}
	if len(events) != 3 {
		t.Fatalf("events = %v, want two deltas + done", events)
	}
}

// TestSTTStreamIntegerSpeaker: NeMo-Speech.cpp labels a speaker with an
// integer, oidio with a UUID. Both are legal in; the event schema's `speaker`
// is a label, so both come out as a string.
func TestSTTStreamIntegerSpeaker(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, jsonBody(
		`{"text":"hi","segments":[{"id":0,"start":0,"end":1,"text":"hi","speaker":2}]}`))
	events, _ := sseEvents(t, rec.body.String())
	for _, e := range events {
		if e["type"] != "transcript.text.segment" {
			continue
		}
		if e["speaker"] != "2" {
			t.Fatalf("speaker = %#v, want the string \"2\"", e["speaker"])
		}
		return
	}
	t.Fatalf("no segment event: %v", events)
}

// TestSTTStreamNotRequested: without the flag the body is forwarded verbatim.
// The synthesis is a response to what the CALLER asked for, never a rewrite of
// every STT reply.
func TestSTTStreamNotRequested(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", false, jsonBody(oidioDiarizeBody))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if strings.Contains(rec.body.String(), "data: ") {
		t.Fatalf("unrequested stream: %s", rec.body.String())
	}
	// The voiceprints a stream=true request loses are still here.
	if !strings.Contains(rec.body.String(), "embedding") {
		t.Errorf("body was rewritten: %s", rec.body.String())
	}
}

// TestSTTStreamErrorStaysJSON: an error must reach the caller as a JSON error
// with its own status, not as an SSE frame. An OpenAI client expects that
// before a stream opens.
func TestSTTStreamErrorStaysJSON(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad audio"}}`))
	})
	if rec.code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.body.String(), "bad audio") {
		t.Errorf("error body lost: %s", rec.body.String())
	}
}

// TestSTTStreamUnreadableBodyForwarded: a 200 JSON body that is not a
// transcription is forwarded exactly as it arrived. The failure mode of the
// transform is "what corrallm did before it existed".
func TestSTTStreamUnreadableBodyForwarded(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, jsonBody(`{"unexpected":"shape"}`))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d", rec.code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want the original", ct)
	}
	if got := strings.TrimSpace(rec.body.String()); got != `{"unexpected":"shape"}` {
		t.Errorf("body = %q", got)
	}
}

// TestSTTStreamTranslationsToo: §9.1 answered both upload routes together.
func TestSTTStreamTranslationsToo(t *testing.T) {
	rec := sttRig(t, "/v1/audio/translations", true, jsonBody(`{"text":"hello"}`))
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.body.String(), "[DONE]") {
		t.Errorf("no terminator: %s", rec.body.String())
	}
}

// TestSTTStreamSpeechUntouched: /v1/audio/speech is deliberately still open
// (§9.1) — TTS streams audio chunks, not SSE events. The transcription answer
// must not travel there by momentum, so the gate excludes it by construction.
func TestSTTStreamSpeechUntouched(t *testing.T) {
	if audioUploadRoute("/v1/audio/speech") {
		t.Fatal("/v1/audio/speech is inside the SSE-synthesis gate")
	}
}

// TestSynthesizeTranscriptSSERejects covers the bodies that must be forwarded
// rather than transformed, without standing up a proxy for each.
func TestSynthesizeTranscriptSSERejects(t *testing.T) {
	for _, raw := range []string{
		``, `not json`, `[1,2,3]`, `"a string"`, `{}`, `{"error":{"message":"x"}}`,
	} {
		if _, ok := synthesizeTranscriptSSE([]byte(raw)); ok {
			t.Errorf("synthesised from %q", raw)
		}
	}
	// An empty transcript IS a transcription — a silent recording — and gets a
	// well-formed, empty stream rather than a passthrough.
	if _, ok := synthesizeTranscriptSSE([]byte(`{"text":""}`)); !ok {
		t.Error("empty transcript rejected")
	}
}

func TestIsJSONContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"APPLICATION/JSON":                true,
		"text/event-stream":               false,
		"text/plain":                      false,
		"":                                false,
	} {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("isJSONContentType(%q) = %v", ct, got)
		}
	}
}
