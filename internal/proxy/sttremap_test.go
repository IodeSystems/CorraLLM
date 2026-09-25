package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The oidio body these tests are written against was READ OFF THE WIRE on
// 2026-09-23 from oidio-stt-diarize with response_format=verbose_json, against
// examples/audio/speech.wav. Trimmed to 3 embedding floats from 512 so the
// fixture stays readable; nothing else is edited.
const oidioDiarizeVerbose = `{
 "duration": 2.560625, "language": "en", "task": "transcribe",
 "text": "The quick brown fox jumps over the lazy dog.",
 "segments": [{"id":0,"start":0,"end":2.528,
   "text":"The quick brown fox jumps over the lazy dog.",
   "speaker":"8569cf12-0acc-4690-a719-c7551b818d06"}],
 "words": [{"word":"The","start":0,"speaker":"8569cf12-0acc-4690-a719-c7551b818d06"},
           {"word":"quick","start":0.16,"speaker":"8569cf12-0acc-4690-a719-c7551b818d06"}],
 "speakers": [{"uuid":"8569cf12-0acc-4690-a719-c7551b818d06",
   "embedding":[-4.849704,5.588882,0.41448724],"similarity":{},
   "clean_seconds":2.5,"total_seconds":2.5,"blended":true}]
}`

// oidio-stt, verbose_json, same file: no speaker anywhere.
const oidioPlainVerbose = `{"duration":2.560625,"language":"en",
 "segments":[{"id":0,"start":0,"end":2.560625,"text":"The quick brown fox jumps over the lazy dog."}],
 "task":"transcribe","text":"The quick brown fox jumps over the lazy dog."}`

func remapOK(t *testing.T, raw string, q sttRequest, model string, stream bool) canonTranscription {
	t.Helper()
	out, refusal, ok := remapTranscription([]byte(raw), q, model, stream, sttDiarizeJoin{})
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.msg)
	}
	if !ok {
		t.Fatal("remap reported it could not read the body")
	}
	var c canonTranscription
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("canonical body does not parse: %v", err)
	}
	return c
}

// Rule 1, the 27-speaker rollover. It is a RULE in the schema, not a runtime
// surprise, and it agrees with OpenAI exactly over the range OpenAI specifies.
func TestSpeakerLabelRollover(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "A"}, {2, "B"}, {26, "Z"},
		{27, "AA"}, {28, "AB"}, {52, "AZ"}, {53, "BA"},
		{702, "ZZ"}, {703, "AAA"},
	}
	for _, c := range cases {
		if got := sttSpeakerLabel(c.n); got != c.want {
			t.Errorf("sttSpeakerLabel(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	if got := sttSpeakerLabel(0); got != "" {
		t.Errorf("sttSpeakerLabel(0) = %q, want empty", got)
	}
	// Every label up to 1000 speakers is distinct: the rollover may not make
	// two speakers share one label.
	seen := map[string]bool{}
	for n := 1; n <= 1000; n++ {
		l := sttSpeakerLabel(n)
		if seen[l] {
			t.Fatalf("label %q repeats at n=%d", l, n)
		}
		seen[l] = true
	}
}

func TestRemapOidioDiarizeVerbose(t *testing.T) {
	c := remapOK(t, oidioDiarizeVerbose, sttRequest{Format: sttFormatVerboseJSON}, "oidio-stt-diarize", false)
	if c.Text != "The quick brown fox jumps over the lazy dog." {
		t.Errorf("text = %q", c.Text)
	}
	if c.Task != "transcribe" || c.Language != "en" {
		t.Errorf("task/language = %q/%q", c.Task, c.Language)
	}
	if c.Duration == nil || *c.Duration != 2.560625 {
		t.Errorf("duration = %v", c.Duration)
	}
	if c.Usage == nil || c.Usage.Type != "duration" || c.Usage.Seconds != 2.560625 {
		t.Errorf("usage = %+v", c.Usage)
	}
	if len(c.Segments) != 1 {
		t.Fatalf("segments = %d", len(c.Segments))
	}
	// The UUID is relabelled, and the id is a string on the non-streaming form
	// exactly as it is in the event.
	if c.Segments[0].Speaker != "A" {
		t.Errorf("speaker = %q, want A", c.Segments[0].Speaker)
	}
	if c.Segments[0].ID != "0" {
		t.Errorf("segment id = %q, want the string \"0\"", c.Segments[0].ID)
	}
	if len(c.Words) != 2 || c.Words[0].Speaker != "A" {
		t.Fatalf("words = %+v", c.Words)
	}
	// oidio reports when a word STARTS and nothing more. `end` is ABSENT, not
	// computed from the next word's start (§12.6).
	if c.Words[0].End != nil {
		t.Errorf("words[0].end = %v, want absent", *c.Words[0].End)
	}
	if c.Backend != nil {
		t.Errorf("backend sidecar present without include[]=backend: %v", c.Backend)
	}
	// The raw JSON must carry no `end` key at all — absent, never null.
	out, _, _ := remapTranscription([]byte(oidioDiarizeVerbose), sttRequest{Format: sttFormatVerboseJSON}, "m", false, sttDiarizeJoin{})
	var probe struct {
		Words []map[string]any `json:"words"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe.Words[0]["end"]; ok {
		t.Error("words[0] carries an `end` key; absent is the honest spelling of unknown")
	}
}

// The floor: a plain OpenAI client reads .text and is finished. No speaker, no
// segments, no sidecar, on any model.
func TestRemapJSONIsTheFloor(t *testing.T) {
	out, refusal, ok := remapTranscription([]byte(oidioDiarizeVerbose),
		sttRequest{Format: sttFormatJSON, IncludeBackend: true}, "oidio-stt-diarize", false, sttDiarizeJoin{})
	if refusal != nil || !ok {
		t.Fatal("remap failed")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("json body carries %d keys, want exactly text: %v", len(m), m)
	}
	if m["text"] != "The quick brown fox jumps over the lazy dog." {
		t.Errorf("text = %v", m["text"])
	}
}

func TestRemapBackendSidecarIsOptIn(t *testing.T) {
	c := remapOK(t, oidioDiarizeVerbose,
		sttRequest{Format: sttFormatDiarizedJSON, IncludeBackend: true}, "oidio-stt-diarize", false)
	if c.Backend == nil {
		t.Fatal("include[]=backend did not produce a sidecar")
	}
	if c.Backend["name"] != "oidio-stt-diarize" {
		t.Errorf("backend.name = %v", c.Backend["name"])
	}
	b, _ := json.Marshal(c.Backend)
	var side struct {
		Speakers []struct {
			UUID      string    `json:"uuid"`
			Label     string    `json:"label"`
			Embedding []float64 `json:"embedding"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(b, &side); err != nil {
		t.Fatal(err)
	}
	if len(side.Speakers) != 1 {
		t.Fatalf("backend.speakers = %d", len(side.Speakers))
	}
	// The label sits beside the uuid, so a caller wanting the voiceprint for
	// "A" has a lookup and not a join on ordering.
	if side.Speakers[0].Label != "A" || side.Speakers[0].UUID != "8569cf12-0acc-4690-a719-c7551b818d06" {
		t.Errorf("speaker = %+v", side.Speakers[0])
	}
	if len(side.Speakers[0].Embedding) != 3 {
		t.Errorf("voiceprint was dropped: %v", side.Speakers[0].Embedding)
	}
}

// §12.7: the caller asked for speaker labels and this model produces none.
// Degrading silently is the stream=true bug with a different field name.
func TestRemapDiarizedJSONOnPlainModelRefuses(t *testing.T) {
	_, refusal, ok := remapTranscription([]byte(oidioPlainVerbose),
		sttRequest{Format: sttFormatDiarizedJSON}, "oidio-stt", false, sttDiarizeJoin{})
	if !ok || refusal == nil {
		t.Fatal("want a refusal")
	}
	if refusal.status != http.StatusBadRequest {
		t.Errorf("status = %d", refusal.status)
	}
	if !strings.Contains(refusal.msg, "oidio-stt") {
		t.Errorf("refusal does not name the model: %s", refusal.msg)
	}
	// verbose_json on the same body is fine, and carries no speaker key at all.
	c := remapOK(t, oidioPlainVerbose, sttRequest{Format: sttFormatVerboseJSON}, "oidio-stt", false)
	if c.Segments[0].Speaker != "" {
		t.Errorf("a non-diarizing model got a speaker label: %q", c.Segments[0].Speaker)
	}
	out, _, _ := remapTranscription([]byte(oidioPlainVerbose), sttRequest{Format: sttFormatVerboseJSON}, "oidio-stt", false, sttDiarizeJoin{})
	var probe struct {
		Segments []map[string]any `json:"segments"`
	}
	_ = json.Unmarshal(out, &probe)
	if _, ok := probe.Segments[0]["speaker"]; ok {
		t.Error("segments[] carries a speaker key on a non-diarizing model")
	}
}

// Two speakers, labelled by order of FIRST APPEARANCE in the backend's own
// document order — not sorted, not by time.
func TestRemapLabelsByFirstAppearance(t *testing.T) {
	raw := `{"text":"a b","segments":[
      {"id":"x","start":0,"end":1,"text":"a","speaker":"zzz-late-uuid"},
      {"id":"y","start":1,"end":2,"text":"b","speaker":"aaa-early-uuid"},
      {"id":"z","start":2,"end":3,"text":"c","speaker":"zzz-late-uuid"}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "m", false)
	got := []string{c.Segments[0].Speaker, c.Segments[1].Speaker, c.Segments[2].Speaker}
	want := []string{"A", "B", "A"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
	// A backend id that is already a string keeps its value.
	if c.Segments[0].ID != "x" {
		t.Errorf("segment id = %q", c.Segments[0].ID)
	}
}

// Accept generously on the way IN: an integer speaker (NeMo) and an integer id
// (oidio) are both legal, and both come out as the one documented form.
func TestRemapAcceptsIntegerSpeakerAndID(t *testing.T) {
	raw := `{"text":"a b","segments":[
      {"id":0,"start":0,"end":1,"text":"a","speaker":1},
      {"id":1,"start":1,"end":2,"text":"b","speaker":0}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "m", false)
	if c.Segments[0].Speaker != "A" || c.Segments[1].Speaker != "B" {
		t.Errorf("speakers = %q/%q", c.Segments[0].Speaker, c.Segments[1].Speaker)
	}
	if c.Segments[0].ID != "0" || c.Segments[1].ID != "1" {
		t.Errorf("ids = %q/%q", c.Segments[0].ID, c.Segments[1].ID)
	}
}

// RULE 2, the NeMo word→segment join. NeMo-Speech.cpp is SOURCE-READ ONLY —
// its PR is unmerged and nothing on this box runs it — so this rule is covered
// here and by nothing else.
func TestJoinRuleTextlessSegments(t *testing.T) {
	// Two textless segments and five words. "two" starts exactly at the second
	// segment's start (half-open: it belongs to the segment that is STARTING),
	// "orphan" falls in the gap after segment two (appended to the nearest
	// segment starting at or before it), "early" starts before every segment.
	raw := `{"segments":[{"start":1,"end":2,"speaker":0},{"start":2,"end":3,"speaker":1}],
	 "words":[{"word":"early","start":0.1,"speaker":0},
	          {"word":"one","start":1.5,"end":1.6,"speaker":0},
	          {"word":"two","start":2,"end":2.1,"speaker":1},
	          {"word":"three","start":2.5,"end":2.6,"speaker":1},
	          {"word":"orphan","start":9,"end":9.1,"speaker":1}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "nemo", false)
	if len(c.Segments) != 2 {
		t.Fatalf("segments = %d", len(c.Segments))
	}
	if c.Segments[0].Text != "early one" {
		t.Errorf("segment 0 text = %q, want \"early one\"", c.Segments[0].Text)
	}
	if c.Segments[1].Text != "two three orphan" {
		t.Errorf("segment 1 text = %q, want \"two three orphan\"", c.Segments[1].Text)
	}
	if c.Segments[0].Speaker != "A" || c.Segments[1].Speaker != "B" {
		t.Errorf("speakers = %q/%q", c.Segments[0].Speaker, c.Segments[1].Speaker)
	}
	// NeMo DOES report words[].end, so it survives.
	if c.Words[1].End == nil || *c.Words[1].End != 1.6 {
		t.Errorf("words[1].end = %v, want 1.6", c.Words[1].End)
	}
	// The transcript is the joined segments when the backend returned no text.
	if c.Text != "early one two three orphan" {
		t.Errorf("text = %q", c.Text)
	}
}

// A segment with no `end` runs to the next segment's start (rule 2), and a
// segment that gathers no words keeps an empty text (rule 6).
func TestJoinRuleOpenEndedAndEmptySegment(t *testing.T) {
	raw := `{"segments":[{"start":0,"speaker":0},{"start":5,"end":6,"speaker":1}],
	 "words":[{"word":"a","start":0,"speaker":0},{"word":"b","start":4.9,"speaker":0}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "nemo", false)
	if c.Segments[0].Text != "a b" {
		t.Errorf("segment 0 text = %q", c.Segments[0].Text)
	}
	if c.Segments[1].Text != "" {
		t.Errorf("segment 1 gathered no words but has text %q", c.Segments[1].Text)
	}
	if c.Segments[0].End != nil {
		t.Errorf("an end the backend did not report was emitted: %v", *c.Segments[0].End)
	}
}

// Rule 5: a caller must not be able to tell by counting spaces whether the
// backend writes "The" or " The".
func TestJoinRuleSpacing(t *testing.T) {
	raw := `{"segments":[{"start":0,"end":9,"speaker":0}],
	 "words":[{"word":" The","start":0,"speaker":0},{"word":" quick","start":1,"speaker":0},
	          {"word":"brown","start":2,"speaker":0}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "nemo", false)
	if c.Segments[0].Text != "The quick brown" {
		t.Errorf("text = %q, want \"The quick brown\"", c.Segments[0].Text)
	}
}

// RULE 2b: a backend that says who spoke when entirely in words[], with no
// segments at all. Source-read only, like rule 2.
func TestSegmentsDerivedFromWords(t *testing.T) {
	raw := `{"words":[{"word":"hi","start":0,"end":0.5,"speaker":0},
	                  {"word":"there","start":0.5,"end":1,"speaker":0},
	                  {"word":"yes","start":1.2,"end":1.5,"speaker":1},
	                  {"word":"again","start":2,"speaker":0}]}`
	c := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON}, "nemo", false)
	if len(c.Segments) != 3 {
		t.Fatalf("segments = %d, want 3 maximal runs: %+v", len(c.Segments), c.Segments)
	}
	if c.Segments[0].Text != "hi there" || c.Segments[0].Speaker != "A" || c.Segments[0].ID != "0" {
		t.Errorf("segment 0 = %+v", c.Segments[0])
	}
	if c.Segments[0].End == nil || *c.Segments[0].End != 1 {
		t.Errorf("segment 0 end = %v, want the last word's end", c.Segments[0].End)
	}
	if c.Segments[1].Speaker != "B" || c.Segments[2].Speaker != "A" {
		t.Errorf("speakers = %q/%q", c.Segments[1].Speaker, c.Segments[2].Speaker)
	}
	// The last run's word reported no end, so the segment reports none: never
	// the next run's start, which nobody measured.
	if c.Segments[2].End != nil {
		t.Errorf("segment 2 end = %v, want absent", *c.Segments[2].End)
	}
	// The integer index survives in the sidecar beside its label.
	c2 := remapOK(t, raw, sttRequest{Format: sttFormatDiarizedJSON, IncludeBackend: true}, "nemo", false)
	b, _ := json.Marshal(c2.Backend)
	var side struct {
		Speakers []struct {
			Index *int   `json:"index"`
			Label string `json:"label"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(b, &side); err != nil {
		t.Fatal(err)
	}
	if len(side.Speakers) != 2 || side.Speakers[0].Index == nil || *side.Speakers[0].Index != 0 ||
		side.Speakers[0].Label != "A" {
		t.Errorf("backend.speakers = %s", b)
	}
}

// A body this cannot read leaves exactly as it arrived — the honest failure
// for a transform that could not run.
func TestRemapForwardsUnreadableBody(t *testing.T) {
	for _, raw := range []string{`{"error":{"message":"boom"}}`, `not json`, `{}`, `[]`} {
		if _, _, ok := remapTranscription([]byte(raw), sttRequest{Format: sttFormatJSON}, "m", false, sttDiarizeJoin{}); ok {
			t.Errorf("remap claimed it could read %q", raw)
		}
	}
}

// stream=true: the EVENT schema is the output contract, so the body is
// normalised for the synthesiser rather than projected down to {"text"}.
func TestRemapNormalizeOnlyForStream(t *testing.T) {
	c := remapOK(t, oidioDiarizeVerbose, sttRequest{Format: sttFormatJSON}, "m", true)
	if len(c.Segments) != 1 || c.Segments[0].Speaker != "A" {
		t.Fatalf("a streaming json request lost its segments: %+v", c.Segments)
	}
}

// The writer, end to end: what a caller actually receives.
func TestRemapWriterProducesCanonicalBody(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &sttRemapWriter{ResponseWriter: rec, req: sttRequest{Format: sttFormatJSON}, model: "oidio-stt-diarize"}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "9999")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oidioDiarizeVerbose))
	w.finish()
	if !w.remapped {
		t.Fatal("writer did not remap")
	}
	if rec.Code != 200 {
		t.Errorf("code = %d", rec.Code)
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("stale Content-Length would truncate the canonical body")
	}
	if got := rec.Body.String(); got != `{"text":"The quick brown fox jumps over the lazy dog."}` {
		t.Errorf("body = %s", got)
	}
}

func TestRemapWriterForwardsErrorsAndStreams(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &sttRemapWriter{ResponseWriter: rec, req: sttRequest{Format: sttFormatJSON}, model: "m"}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	w.finish()
	if rec.Code != 400 || rec.Body.String() != `{"error":{"message":"nope"}}` {
		t.Errorf("an error did not stay an error: %d %s", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	w2 := &sttRemapWriter{ResponseWriter: rec2, req: sttRequest{Format: sttFormatJSON}, model: "m"}
	w2.Header().Set("Content-Type", "text/event-stream")
	w2.WriteHeader(http.StatusOK)
	_, _ = w2.Write([]byte("data: {\"type\":\"transcript.text.delta\",\"delta\":\"hi\"}\n\n"))
	w2.finish()
	if !strings.Contains(rec2.Body.String(), "transcript.text.delta") {
		t.Error("a real event stream was not passed through")
	}
}

func TestRemapWriterRefusalReachesTheCaller(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &sttRemapWriter{ResponseWriter: rec, req: sttRequest{Format: sttFormatDiarizedJSON}, model: "oidio-stt"}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oidioPlainVerbose))
	w.finish()
	if rec.Code != http.StatusBadRequest || !w.refused {
		t.Fatalf("code = %d refused = %v", rec.Code, w.refused)
	}
	var e struct {
		Error struct {
			Message, Type, Param string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Type != "invalid_request_error" || e.Error.Param != "response_format" {
		t.Errorf("error = %+v", e.Error)
	}
}

// ---------------------------------------------------------------------------
// the request side
// ---------------------------------------------------------------------------

func sttForm(t *testing.T, fields map[string][]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("RIFFfake"))
	for k, vals := range fields {
		for _, v := range vals {
			w, err := mw.CreateFormField(k)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(w, v)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

func TestParseSTTRequest(t *testing.T) {
	body, ct := sttForm(t, map[string][]string{"model": {"m"}})
	q, refusal := parseSTTRequest(body, ct)
	if refusal != nil {
		t.Fatal(refusal.msg)
	}
	if q.Format != sttFormatJSON {
		t.Errorf("absent response_format = %q, want json", q.Format)
	}

	for _, spelling := range []string{"include", "include[]"} {
		body, ct := sttForm(t, map[string][]string{"response_format": {"diarized_json"}, spelling: {"backend"}})
		q, refusal := parseSTTRequest(body, ct)
		if refusal != nil {
			t.Fatal(refusal.msg)
		}
		if q.Format != sttFormatDiarizedJSON || !q.IncludeBackend {
			t.Errorf("%s: %+v", spelling, q)
		}
	}
	// A comma list and a repeated field mean the same request.
	body, ct = sttForm(t, map[string][]string{"include[]": {"backend,backend"}})
	if q, _ := parseSTTRequest(body, ct); !q.IncludeBackend {
		t.Error("comma list not accepted")
	}

	// text/srt/vtt are forwarded, not remapped.
	for _, f := range []string{"text", "srt", "vtt"} {
		body, ct := sttForm(t, map[string][]string{"response_format": {f}})
		q, _ := parseSTTRequest(body, ct)
		if q.remaps() {
			t.Errorf("%s should not be remapped", f)
		}
	}
	// A JSON body is not a form this reads.
	if q, _ := parseSTTRequest([]byte(`{"model":"m"}`), "application/json"); q.remaps() {
		t.Error("a non-multipart body was claimed as remappable")
	}
}

// Refusing at the door beats accepting and failing after the queue.
func TestParseSTTRequestRefusals(t *testing.T) {
	body, ct := sttForm(t, map[string][]string{"include[]": {"logprobs"}})
	_, refusal := parseSTTRequest(body, ct)
	if refusal == nil || refusal.param != "include" {
		t.Fatalf("include[]=logprobs was not refused: %+v", refusal)
	}

	body, ct = sttForm(t, map[string][]string{"known_speaker_names[]": {"Alice"}})
	_, refusal = parseSTTRequest(body, ct)
	if refusal == nil || refusal.param != "known_speaker_names" {
		t.Fatalf("known_speaker_names was not refused: %+v", refusal)
	}
	if !strings.Contains(refusal.msg, "not implemented") {
		t.Errorf("the refusal must say what is missing: %s", refusal.msg)
	}
	// Reserved and EMPTY is a no-op, not a request.
	body, ct = sttForm(t, map[string][]string{"known_speaker_names[]": {""}})
	if _, refusal := parseSTTRequest(body, ct); refusal != nil {
		t.Errorf("an empty known_speaker_names was refused: %s", refusal.msg)
	}
}

func TestRewriteSTTUpstream(t *testing.T) {
	body, ct := sttForm(t, map[string][]string{
		"model": {"m"}, "response_format": {"diarized_json"}, "include[]": {"backend"},
	})
	nb, nct, ok := rewriteSTTUpstream(body, ct)
	if !ok {
		t.Fatal("rewrite failed")
	}
	got := readForm(t, nb, nct)
	if got["response_format"] != sttUpstreamFormat {
		t.Errorf("response_format = %q, want %q", got["response_format"], sttUpstreamFormat)
	}
	if _, ok := got["include[]"]; ok {
		t.Error("include[] was forwarded upstream; it is corrallm's own parameter")
	}
	if got["model"] != "m" {
		t.Errorf("model = %q", got["model"])
	}
	if got["file"] != "RIFFfake" {
		t.Errorf("the audio part did not survive: %q", got["file"])
	}
	if got["file:filename"] != "speech.wav" {
		t.Errorf("the filename did not survive: %q", got["file:filename"])
	}

	// Absent response_format is ADDED, since the caller's default is json and
	// the backend's default is its own business.
	body, ct = sttForm(t, map[string][]string{"model": {"m"}})
	nb, nct, ok = rewriteSTTUpstream(body, ct)
	if !ok {
		t.Fatal("rewrite failed")
	}
	if got := readForm(t, nb, nct); got["response_format"] != sttUpstreamFormat {
		t.Errorf("response_format = %q", got["response_format"])
	}
}

func readForm(t *testing.T, body []byte, contentType string) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		v, _ := io.ReadAll(part)
		out[part.FormName()] = string(v)
		if fn := part.FileName(); fn != "" {
			out[part.FormName()+":filename"] = fn
		}
		_ = part.Close()
	}
	return out
}

// ---------------------------------------------------------------------------
// through the proxy, which is where the wiring is
// ---------------------------------------------------------------------------

// What corrallm ASKS the backend for, whatever the caller asked for. oidio
// silently answers its model default for a response_format it does not know,
// so forwarding `diarized_json` would be the silently-wrong failure this layer
// exists to end.
func TestProxySTTUpstreamFormatIsNormalised(t *testing.T) {
	for _, caller := range []string{"", "json", "verbose_json", "diarized_json"} {
		var gotFormat, gotInclude string
		fields := map[string][]string{"include[]": {"backend"}}
		if caller != "" {
			fields["response_format"] = []string{caller}
		}
		rec := sttRigForm(t, "/v1/audio/transcriptions", fields,
			func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseMultipartForm(1 << 20)
				gotFormat = r.FormValue("response_format")
				gotInclude = r.FormValue("include[]")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(oidioDiarizeVerbose))
			})
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", caller, rec.code, rec.body.String())
		}
		if gotFormat != sttUpstreamFormat {
			t.Errorf("caller asked %q, backend was asked %q, want %q", caller, gotFormat, sttUpstreamFormat)
		}
		if gotInclude != "" {
			t.Errorf("include[] reached the backend: %q", gotInclude)
		}
	}
}

func TestProxySTTCanonicalBodies(t *testing.T) {
	cases := []struct {
		format string
		check  func(t *testing.T, body string)
	}{
		{"json", func(t *testing.T, b string) {
			if b != `{"text":"The quick brown fox jumps over the lazy dog."}` {
				t.Errorf("json body = %s", b)
			}
		}},
		{"verbose_json", func(t *testing.T, b string) {
			var c canonTranscription
			if err := json.Unmarshal([]byte(b), &c); err != nil {
				t.Fatal(err)
			}
			if c.Segments[0].Speaker != "A" || c.Segments[0].ID != "0" {
				t.Errorf("segment = %+v", c.Segments[0])
			}
			if c.Usage == nil {
				t.Error("usage missing from a body whose backend measured a duration")
			}
			if strings.Contains(b, "embedding") {
				t.Error("voiceprints rode out without include[]=backend")
			}
		}},
		{"diarized_json", func(t *testing.T, b string) {
			var c canonTranscription
			if err := json.Unmarshal([]byte(b), &c); err != nil {
				t.Fatal(err)
			}
			if len(c.Segments) == 0 || c.Segments[0].Speaker == "" {
				t.Errorf("diarized_json without a speaker: %s", b)
			}
		}},
	}
	for _, c := range cases {
		rec := sttRigForm(t, "/v1/audio/transcriptions",
			map[string][]string{"response_format": {c.format}},
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(oidioDiarizeVerbose))
			})
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", c.format, rec.code, rec.body.String())
		}
		c.check(t, rec.body.String())
	}
}

func TestProxySTTIncludeBackend(t *testing.T) {
	rec := sttRigForm(t, "/v1/audio/transcriptions",
		map[string][]string{"response_format": {"diarized_json"}, "include[]": {"backend"}},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(oidioDiarizeVerbose))
		})
	if !strings.Contains(rec.body.String(), "embedding") {
		t.Errorf("include[]=backend did not carry the voiceprint: %s", rec.body.String())
	}
	if !strings.Contains(rec.body.String(), `"label":"A"`) {
		t.Errorf("the sidecar carries no label back-reference: %s", rec.body.String())
	}
}

// Refused at the door, before the request queues behind a saturated backend.
func TestProxySTTRefusesAtTheDoor(t *testing.T) {
	for _, fields := range []map[string][]string{
		{"known_speaker_names[]": {"Alice"}},
		{"include[]": {"logprobs"}},
	} {
		reached := false
		rec := sttRigForm(t, "/v1/audio/transcriptions", fields,
			func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(oidioDiarizeVerbose))
			})
		if rec.code != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", fields, rec.code)
		}
		if reached {
			t.Errorf("%v: the backend was asked anyway", fields)
		}
	}
}

// text/srt/vtt have nothing to remap and must leave exactly as they arrived.
func TestProxySTTPlainFormatsUntouched(t *testing.T) {
	rec := sttRigForm(t, "/v1/audio/transcriptions",
		map[string][]string{"response_format": {"srt"}},
		func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseMultipartForm(1 << 20)
			if got := r.FormValue("response_format"); got != "srt" {
				t.Errorf("srt was rewritten upstream to %q", got)
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("1\n00:00:00,000 --> 00:00:02,560\nhello\n"))
		})
	if !strings.HasPrefix(rec.body.String(), "1\n00:00:00,000") {
		t.Errorf("srt body = %q", rec.body.String())
	}
}
