package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Remap-out, contract 2 (P35 §9.1): stream=true on an STT path whose backend
// cannot stream.
//
// THE CALLER SET A FLAG THE SPEC DEFINES AND THE BACKEND THREW IT AWAY. oidio's
// transcriptions.go:45 dispatches to handleDiarize before the stream check and
// handleDiarize never reads the flag; verified on the wire 2026-09-23, a
// stream=true transcription answers 200 application/json with a complete body.
// A corral layer that forwards that is the silently-wrong option — the client
// gets JSON where it is parsing SSE — and a 400 would punish a caller for
// defensive correctness. So corrallm emits the event sequence itself.
//
// THE LATENCY BENEFIT IS A FICTION, AND IT IS THE ONE THING THIS MUST NOT
// PRETEND. The backend's body is COMPLETE before the first event leaves. There
// is no time-to-first-word here and no progressive delivery; a caller waits
// exactly as long as it would have for the JSON and then receives the whole
// transcript as events, back to back. What this buys is SHAPE: one client code
// path for every STT backend, streaming or not. Nothing else. The capabilities
// manifest says the same sentence to anyone who asks the API, and P35 §12 says
// it to anyone who reads the design.
//
// A REAL STREAM IS FORWARDED EVENT FOR EVENT, AND TERMINATED THE SAME WAY. The
// decision is made on the backend's own Content-Type: text/event-stream is
// passed through unaltered and unbuffered, and synthesis is the fallback for a
// backend that answered with a finished body. Anything that is not a 200 JSON
// object forwards too — an error must stay a JSON error with its own status,
// which is what an OpenAI client expects before a stream opens.
//
// ONE ROUTE, ONE OUTPUT CONTRACT (P35 §9.0b, §12.10.2). Until this, the two
// halves of this route ended differently: the synthesised stream terminated
// with `data: [DONE]`, and oidio's real stream — measured on the wire, ten
// `transcript.text.delta` events and a `transcript.text.done` — sent no
// terminator at all, so a client could not use [DONE] as its end-of-stream
// test. Produce-strictly makes that a broken contract rather than a quirk, so a
// passed-through stream is now COMPLETED, not rewritten: corrallm appends only
// what the backend left out.
//
// Accept generously is the other half, and it is what stops the completion
// doubling anything. A backend that already sent [DONE] gets no second one; a
// backend that already sent `transcript.text.done` gets no second one either.
// Nothing the backend emitted is dropped, reordered, re-serialised or counted —
// the observation reads the bytes on their way past and appends at the end.
//
// NOT /v1/audio/speech. TTS streams AUDIO chunks, not SSE events: chunking a
// finished mp3 is a different mechanism with a different honesty problem, and
// it is deliberately still an open question (P35 §9.1). audioUploadRoute is the
// gate, and it excludes speech by construction.

// sttStreamWriter converts a complete JSON transcription into the OpenAI
// transcription event stream, or gets out of the way.
//
// It HOLDS THE RESPONSE HEADERS until the body is complete. That is the point
// of the design rather than an accident: the choice to synthesise is taken from
// the status and Content-Type, but whether the body is really a transcription
// is only knowable once it has been parsed. Holding the headers means a body
// that turns out not to parse is still forwarded verbatim, with its original
// headers and status — the failure mode is "exactly what corrallm did before",
// not a half-written stream. It costs nothing a caller can measure: the body
// being buffered is one that arrived complete anyway.
type sttStreamWriter struct {
	http.ResponseWriter
	decided   bool // the status has been seen and the mode chosen
	buffering bool // chosen: hold the body and try to synthesise at finish()
	code      int
	body      bytes.Buffer
	// synthesized records what finish() actually did, for the log line.
	synthesized bool

	// passing is the OTHER mode: the backend answered with a real event stream,
	// which is forwarded as it arrives and only COMPLETED at finish(). The
	// fields below are observation of bytes already written downstream — they
	// never gate, delay or alter a single one.
	passing    bool
	tail       []byte          // an SSE line split across two Writes
	sawTerm    bool            // the backend sent `data: [DONE]` itself
	sawDone    bool            // the backend sent its own transcript.text.done
	deltas     strings.Builder // delta text, for a done event no backend sent
	normalized bool            // finish() appended something, for the log line
}

func (s *sttStreamWriter) WriteHeader(code int) {
	// 1xx are interim (a backend's 100-continue on a large upload, forwarded by
	// ReverseProxy before the final status). Forward, do not decide on them.
	if code >= 100 && code < 200 {
		s.ResponseWriter.WriteHeader(code)
		return
	}
	if s.decided {
		if !s.buffering {
			s.ResponseWriter.WriteHeader(code)
		}
		return
	}
	s.decided, s.code = true, code
	switch {
	case code == http.StatusOK && isJSONContentType(s.Header().Get("Content-Type")):
		s.buffering = true
		return // headers held — see the type comment
	case code == http.StatusOK && isEventStreamContentType(s.Header().Get("Content-Type")):
		s.passing = true
		// finish() may append a terminator the backend's Content-Length did not
		// count, which would truncate the reply. An event stream is chunked in
		// practice (oidio's is); deleting a stray header is the cheap guard.
		s.Header().Del("Content-Length")
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *sttStreamWriter) Write(b []byte) (int, error) {
	if !s.decided {
		s.WriteHeader(http.StatusOK)
	}
	if s.buffering {
		return s.body.Write(b)
	}
	if s.passing {
		s.observe(b)
	}
	return s.ResponseWriter.Write(b)
}

// Flush is a no-op while buffering: there is nothing downstream to push yet,
// and ReverseProxy's 100 ms flush timer calls this whether or not anything was
// written.
func (s *sttStreamWriter) Flush() {
	if s.buffering {
		return
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observe reads the event lines going past and records what the backend has
// already said, so finish() can add only what is missing. It parses nothing it
// does not need and changes nothing: b has already been handed downstream.
//
// The tail holds a line split across two Writes. SSE frames arrive whole in
// practice — a backend flushes per event — but "in practice" is not a contract,
// and a delta cut in half by a TCP boundary would otherwise be read as two.
func (s *sttStreamWriter) observe(b []byte) {
	// Bound the tail: a backend emitting an unterminated megabyte is not a
	// stream this can characterise, and holding it would be the only place
	// passthrough allocates without limit. Drop it and keep forwarding.
	const maxTail = 1 << 20
	buf := b
	if len(s.tail) > 0 {
		buf = append(s.tail, b...)
		s.tail = nil
	}
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		s.observeLine(buf[:i])
		buf = buf[i+1:]
	}
	if len(buf) > 0 && len(buf) <= maxTail {
		s.tail = append([]byte(nil), buf...)
	}
}

// observeLine records one SSE line. Only `data:` matters here; `event:`, `id:`,
// `retry:`, comments and the blank frame separator are somebody else's business
// and are forwarded without being read.
func (s *sttStreamWriter) observeLine(line []byte) {
	t := bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(t, []byte("data:")) {
		return
	}
	// Accept generously: the single space after `data:` is optional in SSE, and
	// a backend that omits it is emitting the same event.
	data := bytes.TrimSpace(t[len("data:"):])
	if bytes.Equal(data, []byte("[DONE]")) {
		s.sawTerm = true
		return
	}
	var ev struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return // not an event this needs to understand
	}
	switch ev.Type {
	case "transcript.text.done":
		s.sawDone = true
	case "transcript.text.delta":
		s.deltas.WriteString(ev.Delta)
	}
}

// completePassthrough appends what the backend left off, and nothing else.
//
// The done event is built only from the deltas the backend itself emitted —
// concatenation of returned data, not a guess (P35 §9.0). It carries no usage
// block: a real stream reports no duration, and computing one from the upload
// would emit a number nobody measured, which is the same ruling §9.0b made for
// words[].end. A stream that sent no deltas and no done gets the terminator
// only; there is nothing to say a transcript was produced.
func (s *sttStreamWriter) completePassthrough() {
	if len(s.tail) > 0 {
		s.observeLine(s.tail)
		s.tail = nil
	}
	if s.sawTerm {
		return // the backend terminated its own stream; adding a second is not tolerance
	}
	var out bytes.Buffer
	if !s.sawDone && s.deltas.Len() > 0 {
		writeSSEData(&out, sseTranscriptDone{
			Type: "transcript.text.done", Text: s.deltas.String(),
		})
	}
	out.WriteString("data: [DONE]\n\n")
	_, _ = s.ResponseWriter.Write(out.Bytes())
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	s.normalized = true
}

// finish emits the synthesised stream, completes a passed-through one, or
// releases the original body unchanged. Safe to call on any exit path; a no-op
// when nothing was held and nothing needs finishing.
func (s *sttStreamWriter) finish() {
	if s.passing {
		s.passing = false
		s.completePassthrough()
		return
	}
	if !s.buffering {
		return
	}
	s.buffering = false
	raw := s.body.Bytes()
	events, ok := synthesizeTranscriptSSE(raw)
	if !ok {
		// Not a transcription body this can read. Forward it as it came: the
		// caller gets the same JSON it would have got with no remap layer at
		// all, which is the honest failure for a transform that could not run.
		s.ResponseWriter.WriteHeader(s.code)
		_, _ = s.ResponseWriter.Write(raw)
		return
	}
	h := s.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// The backend's Content-Length described the JSON, which is not what goes
	// out. Leaving it would truncate or hang the response.
	h.Del("Content-Length")
	s.ResponseWriter.WriteHeader(s.code)
	_, _ = s.ResponseWriter.Write(events)
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	s.synthesized = true
}

// isJSONContentType is true for a body corrallm may try to read as one JSON
// object. Parameters (charset) are ignored; text/event-stream is not JSON and
// is what makes a real stream pass through.
func isJSONContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// isEventStreamContentType is true for a backend that is really streaming, and
// it is what selects completion over synthesis.
func isEventStreamContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// The OpenAI transcription stream events, as published. Field names are theirs,
// not invented here: `transcript.text.delta` carries a plain string `delta` (and
// a `segment_id` on a speaker-labelled response), `transcript.text.segment`
// carries the completed segment with its speaker, and `transcript.text.done`
// carries the whole transcript with a duration usage block.
//
// What is deliberately NOT emitted: `logprobs` (no backend here reports
// per-token confidence on a batch transcription, and a caller must not read an
// absent field as certainty), and the `languages` on done (oidio returns one
// `language` for the request, which is not the same claim).
type (
	sseTranscriptDelta struct {
		Type      string `json:"type"`
		Delta     string `json:"delta"`
		SegmentID string `json:"segment_id,omitempty"`
	}
	sseTranscriptSegment struct {
		Type    string   `json:"type"`
		ID      string   `json:"id,omitempty"`
		Start   *float64 `json:"start,omitempty"`
		End     *float64 `json:"end,omitempty"`
		Text    string   `json:"text"`
		Speaker string   `json:"speaker"`
	}
	sseTranscriptUsage struct {
		Type    string  `json:"type"`
		Seconds float64 `json:"seconds"`
	}
	sseTranscriptDone struct {
		Type  string              `json:"type"`
		Text  string              `json:"text"`
		Usage *sseTranscriptUsage `json:"usage,omitempty"`
	}
)

// sttResponse is the part of a backend's transcription body this reads. Only
// the fields the event schema has a home for; everything else in the body
// (oidio's speakers[] voiceprints, language, task, words[]) has no carrier in
// the OpenAI event set and is DROPPED — stated here, in the manifest, and in
// P35 §12, because a caller that sets stream=true on a diarizing model is
// giving up data it would have received without the flag.
type sttResponse struct {
	Text     *string      `json:"text"`
	Duration *float64     `json:"duration"`
	Segments []sttRespSeg `json:"segments"`
}

type sttRespSeg struct {
	ID    json.RawMessage `json:"id"`
	Start *float64        `json:"start"`
	End   *float64        `json:"end"`
	Text  string          `json:"text"`
	// Raw because the two backends disagree on its type and both are legal
	// inputs here: oidio returns a per-request UUID string, NeMo-Speech.cpp
	// returns an integer index. Rendered to a string either way — the event
	// schema's `speaker` is a label, and a label is text.
	Speaker json.RawMessage `json:"speaker"`
}

// synthesizeTranscriptSSE turns a complete transcription body into the event
// stream. Reports false when the body is not one it can read, which is the
// signal to forward the original untouched.
//
// THE CHUNKING IS THE DATA'S, NOT A CADENCE. One delta per segment when the
// backend returned segments, one delta for the whole transcript when it did
// not. Splitting a finished string into word-sized deltas on a timer would
// manufacture the appearance of the progress this cannot deliver, which is the
// exact lie the fiction note exists to prevent.
func synthesizeTranscriptSSE(raw []byte) ([]byte, bool) {
	var b sttResponse
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, false
	}
	if b.Text == nil && len(b.Segments) == 0 {
		return nil, false // not a transcription reply
	}
	full := ""
	if b.Text != nil {
		full = *b.Text
	}
	if full == "" && len(b.Segments) > 0 {
		var sb strings.Builder
		for _, seg := range b.Segments {
			sb.WriteString(seg.Text)
		}
		full = sb.String()
	}

	var out bytes.Buffer
	switch {
	case len(b.Segments) > 0:
		for _, seg := range b.Segments {
			id, speaker := jsonScalarString(seg.ID), jsonScalarString(seg.Speaker)
			if seg.Text != "" {
				writeSSEData(&out, sseTranscriptDelta{
					Type: "transcript.text.delta", Delta: seg.Text, SegmentID: id,
				})
			}
			// The segment event is the DIARIZATION carrier: it exists to say who
			// spoke. A backend that returned segments without a speaker (plain
			// verbose_json) gets deltas only, rather than a segment event with an
			// empty speaker field that a client would have to guess about.
			if speaker == "" {
				continue
			}
			writeSSEData(&out, sseTranscriptSegment{
				Type: "transcript.text.segment", ID: id,
				Start: seg.Start, End: seg.End, Text: seg.Text, Speaker: speaker,
			})
		}
	case full != "":
		writeSSEData(&out, sseTranscriptDelta{Type: "transcript.text.delta", Delta: full})
	}
	done := sseTranscriptDone{Type: "transcript.text.done", Text: full}
	if b.Duration != nil {
		// Real data, not a derived number: the backend measured the audio. It is
		// the only usage an STT reply carries, and it is what corrallm bills on.
		done.Usage = &sseTranscriptUsage{Type: "duration", Seconds: *b.Duration}
	}
	writeSSEData(&out, done)
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes(), true
}

func writeSSEData(out *bytes.Buffer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(b)
	out.WriteString("\n\n")
}

// jsonScalarString renders a JSON scalar as a plain string: a string keeps its
// value, a number keeps its digits. Absent, null or anything structured is "".
func jsonScalarString(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	}
	if t[0] == '-' || (t[0] >= '0' && t[0] <= '9') {
		return t
	}
	return ""
}
