package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	fields := map[string][]string{}
	if stream {
		fields["stream"] = []string{"true"}
	}
	return sttRigForm(t, path, fields, h)
}

// sttRigForm is the same rig with the caller's form fields spelled out, for
// the remap tests: response_format and include[] are what a caller sets and
// what corrallm has to answer for.
func sttRigForm(t *testing.T, path string, fields map[string][]string, h http.HandlerFunc) *flushRecorder {
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
	for k, vals := range fields {
		for _, v := range vals {
			_ = mw.WriteField(k, v)
		}
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
		// The space after `data:` is optional in SSE, and a passed-through
		// stream is the backend's bytes — this helper reads both forms.
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
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
	// The speaker label is CANONICAL, not the backend's. §12.4 is ratified:
	// the event carries "A", and oidio's per-request UUID — which is not an
	// identity and changes every call — stays out of a field a client would
	// key a database on. The streaming and non-streaming forms agree because
	// the remap runs inside the synthesiser (P35 §12).
	seg := events[1]
	if seg["speaker"] != "A" {
		t.Errorf("speaker = %v, want the canonical label A", seg["speaker"])
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
// integer, oidio with a UUID. Both are legal IN — that is §9.0b's tolerance —
// and both come out as the one canonical label, by order of first appearance.
// An integer 2 is the first speaker seen here, so it is "A" and not "2":
// forwarding a backend's numbering would be letting that backend's accident
// out through a portable API.
func TestSTTStreamIntegerSpeaker(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, jsonBody(
		`{"text":"hi ho","segments":[{"id":0,"start":0,"end":1,"text":"hi","speaker":2},`+
			`{"id":1,"start":1,"end":2,"text":"ho","speaker":0}]}`))
	events, _ := sseEvents(t, rec.body.String())
	var labels []string
	for _, e := range events {
		if e["type"] == "transcript.text.segment" {
			labels = append(labels, e["speaker"].(string))
		}
	}
	if strings.Join(labels, ",") != "A,B" {
		t.Fatalf("speakers = %v, want A,B by order of first appearance", labels)
	}
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
	// It is still NOT verbatim, and that changed with §12: the default
	// response_format is `json`, whose canonical body is `{"text":…}` and
	// nothing else. The voiceprints are not lost — they are behind
	// include[]=backend, where 5.2 KB per speaker is a caller's choice rather
	// than every caller's cost.
	if got := rec.body.String(); got != `{"text":"The quick brown fox. Jumps over the lazy dog."}` {
		t.Errorf("body = %s", got)
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

func TestIsEventStreamContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/event-stream":                true,
		"text/event-stream; charset=utf-8": true,
		"TEXT/EVENT-STREAM":                true,
		"application/json":                 false,
		"audio/mpeg":                       false,
		"":                                 false,
	} {
		if got := isEventStreamContentType(ct); got != want {
			t.Errorf("isEventStreamContentType(%q) = %v", ct, got)
		}
	}
}

// sseStream serves a real event stream, one flush per frame, as oidio-stt does.
func sseStream(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
}

// oidioRealStream is what oidio-stt actually put on the wire for
// examples/audio/speech.wav on 2026-09-23: ten transcript.text.delta events, a
// transcript.text.done with NO usage block, and NO terminator. Reproduced here
// field for field, key order included — this is the shape §12.10.2 is about.
var oidioRealStream = []string{
	`data: {"delta":" The","type":"transcript.text.delta"}`,
	`data: {"delta":" quick","type":"transcript.text.delta"}`,
	`data: {"delta":" brown","type":"transcript.text.delta"}`,
	`data: {"delta":" fox","type":"transcript.text.delta"}`,
	`data: {"delta":" jumps","type":"transcript.text.delta"}`,
	`data: {"delta":" over","type":"transcript.text.delta"}`,
	`data: {"delta":" the","type":"transcript.text.delta"}`,
	`data: {"delta":" lazy","type":"transcript.text.delta"}`,
	`data: {"delta":" dog","type":"transcript.text.delta"}`,
	`data: {"delta":".","type":"transcript.text.delta"}`,
	`data: {"text":"The quick brown fox jumps over the lazy dog.","type":"transcript.text.done"}`,
}

// TestSTTStreamPassthroughGetsTerminator is P35 §12.10.2 under §9.0b: one route,
// one output contract. oidio's real stream ends without [DONE]; corrallm's
// synthesised stream ends with it. A client must be able to use [DONE] as its
// end-of-stream test whichever half answered, so a real stream is COMPLETED.
func TestSTTStreamPassthroughGetsTerminator(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, sseStream(oidioRealStream...))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.code, rec.body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.body.String()
	evs, done := sseEvents(t, body)
	if !done {
		t.Fatalf("no [DONE] appended to a real stream: %s", body)
	}
	if n := strings.Count(body, "[DONE]"); n != 1 {
		t.Errorf("terminator count = %d", n)
	}
	// Every backend event survives, in order, unaltered. The completion ADDS;
	// it does not rewrite.
	if len(evs) != len(oidioRealStream) {
		t.Fatalf("events = %d, want the backend's %d: %s", len(evs), len(oidioRealStream), body)
	}
	if n := strings.Count(body, `"transcript.text.done"`); n != 1 {
		t.Errorf("done events = %d: %s", n, body)
	}
	if evs[0]["delta"] != " The" || evs[9]["delta"] != "." {
		t.Errorf("deltas altered: %+v", evs)
	}
	if evs[10]["text"] != "The quick brown fox jumps over the lazy dog." {
		t.Errorf("done event altered: %+v", evs[10])
	}
	// Still a stream, not a buffer: the backend's flushes reached the client.
	if rec.flushes == 0 {
		t.Error("real stream was buffered")
	}
}

// TestSTTStreamPassthroughKeepsOwnTerminator is accept-generously applied to
// what the backend already did: a stream that terminates correctly must not end
// up with two terminators, or two done events.
func TestSTTStreamPassthroughKeepsOwnTerminator(t *testing.T) {
	frames := append(append([]string{}, oidioRealStream...), `data: [DONE]`)
	rec := sttRig(t, "/v1/audio/transcriptions", true, sseStream(frames...))
	body := rec.body.String()
	if n := strings.Count(body, "[DONE]"); n != 1 {
		t.Fatalf("terminator count = %d: %s", n, body)
	}
	if n := strings.Count(body, `"transcript.text.done"`); n != 1 {
		t.Errorf("done events = %d", n)
	}
}

// TestSTTStreamPassthroughGetsDone: a backend that streams deltas and stops
// still owes the route a done event. It is built from the deltas the backend
// itself sent — concatenation of returned data, not a guess — and carries no
// usage block, because a real stream reports no duration and computing one
// would emit a number nobody measured (§9.0b, the same ruling as words[].end).
//
// NOT EXERCISED BY ANY LIVE BACKEND: oidio-stt sends its own done. Unit
// coverage only.
func TestSTTStreamPassthroughGetsDone(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, sseStream(
		`data: {"type":"transcript.text.delta","delta":"Hello"}`,
		`data: {"type":"transcript.text.delta","delta":" world"}`,
	))
	evs, done := sseEvents(t, rec.body.String())
	if !done {
		t.Fatalf("no terminator: %s", rec.body.String())
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 2 deltas + a done: %s", len(evs), rec.body.String())
	}
	last := evs[2]
	if last["type"] != "transcript.text.done" || last["text"] != "Hello world" {
		t.Fatalf("done event = %+v", last)
	}
	if _, ok := last["usage"]; ok {
		t.Errorf("usage invented for a stream that measured none: %+v", last)
	}
}

// TestSTTStreamPassthroughSplitFrames: an event cut in half by a write boundary
// is still one event. The observation holds the partial line rather than
// reading it as two, and the bytes reach the client either way.
func TestSTTStreamPassthroughSplitFrames(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"type":"transcript.text.de`,
			`lta","delta":"Hel`,
			"lo\"}\n\ndata:{\"type\":\"transcript.text.delta\",\"delta\":\" world\"}\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if fl != nil {
				fl.Flush()
			}
		}
	})
	evs, done := sseEvents(t, rec.body.String())
	if !done {
		t.Fatalf("no terminator: %s", rec.body.String())
	}
	if len(evs) != 3 || evs[2]["text"] != "Hello world" {
		t.Fatalf("split frame misread: %+v (%s)", evs, rec.body.String())
	}
}

// TestSTTStreamPassthroughNoDeltas: a stream carrying nothing this can read
// gets the terminator and nothing else. corrallm does not assert a transcript
// was produced when no backend said one was.
func TestSTTStreamPassthroughNoDeltas(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", true, sseStream(
		`data: {"type":"transcript.text.something_else"}`,
	))
	body := rec.body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no terminator: %s", body)
	}
	if strings.Contains(body, "transcript.text.done") {
		t.Errorf("done event invented: %s", body)
	}
}

// TestSTTStreamPassthroughNotRequested: the completion belongs to the contract
// the caller opted into with stream=true. A caller that did not ask gets the
// backend's bytes exactly as before.
func TestSTTStreamPassthroughNotRequested(t *testing.T) {
	rec := sttRig(t, "/v1/audio/transcriptions", false, sseStream(oidioRealStream...))
	if strings.Contains(rec.body.String(), "[DONE]") {
		t.Errorf("unrequested stream was rewritten: %s", rec.body.String())
	}
}

// TestStreamFlagAcceptedGenerously: the flag arrives as form TEXT, not as a
// JSON boolean, and an exact match on "true" served a non-streaming reply to a
// client that wrote it any other way (P35 §9.0b, accept generously).
func TestStreamFlagAcceptedGenerously(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"true", true}, {"True", true}, {"TRUE", true}, {"1", true}, {"t", true},
		{" true ", true},
		{"false", false}, {"False", false}, {"0", false}, {"", false},
		{"yes", false}, // not a bool: unreadable stays false, as it did
	} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", "m")
		_ = mw.WriteField("stream", tc.val)
		_ = mw.Close()
		model, streaming := modelFromMultipart(buf.Bytes(), mw.Boundary())
		if model != "m" {
			t.Fatalf("%q: model = %q", tc.val, model)
		}
		if streaming != tc.want {
			t.Errorf("stream=%q: got %v, want %v", tc.val, streaming, tc.want)
		}
	}
}
