package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// The two-call diarized join (P35 §12.9(e) RULE 2).
//
// THE FIXTURES ARE REAL SHAPES, not invented ones. nemoWordsOnly is
// NeMo-Speech.cpp's verbose_json with diarization on: words carrying a 1-based
// integer speaker and NO segments[] at all. nemoDiarSegments is the same
// backend's /v1/audio/diarizations body: textless segments, 1-based integer
// speaker, and OVERLAPPING — simultaneous speech is real and this backend
// reports it (measured on AMI meeting audio: 80 words of 3,284 start inside
// two segments at once).
const nemoWordsOnly = `{"duration":10,"language":"","task":"transcribe",
 "text":"one two three four five",
 "words":[{"word":"one","start":0.0,"end":0.5,"speaker":1},
          {"word":"two","start":1.0,"end":1.5,"speaker":1},
          {"word":"three","start":2.5,"end":3.0,"speaker":1},
          {"word":"four","start":4.0,"end":4.5,"speaker":1},
          {"word":"five","start":9.0,"end":9.5,"speaker":1}]}`

// Segments in start order, with segment 1 (2.0-3.5, speaker 2) overlapping
// segment 2 (2.8-6.0, speaker 1).
const nemoDiarSegments = `{"segments":[
 {"start":0.0,"end":2.0,"speaker":1},
 {"start":2.0,"end":3.5,"speaker":2},
 {"start":2.8,"end":6.0,"speaker":1},
 {"start":7.0,"end":8.0,"speaker":3}]}`

func joinOK(t *testing.T, body, diar string, q sttRequest) canonTranscription {
	t.Helper()
	out, refusal, ok := remapTranscription([]byte(body), q, "nemo-joined", false,
		sttDiarizeJoin{Path: "/v1/audio/diarizations", Raw: []byte(diar)})
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.msg)
	}
	if !ok {
		t.Fatal("remap could not read the body")
	}
	var c canonTranscription
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The whole point: the separate call's segments replace whatever the fused
// body said, and RULE 2 marries them to the words.
func TestJoinUsesDiarizationSegmentsAsAuthority(t *testing.T) {
	c := joinOK(t, nemoWordsOnly, nemoDiarSegments, sttRequest{Format: sttFormatDiarizedJSON})
	if len(c.Segments) != 4 {
		t.Fatalf("segments = %d, want the diarization call's 4: %+v", len(c.Segments), c.Segments)
	}
	// Labels by order of first appearance in the DIARIZATION document.
	want := []struct{ id, speaker, text string }{
		{"0", "A", "one two"}, // 0.0-2.0
		{"1", "B", "three"},   // 2.0-3.5 — wins the overlap, it is listed first
		{"2", "A", "four"},    // 2.8-6.0
		// "five" starts at 9.0, past every segment: rule 3 appends it to the
		// nearest segment starting at or before it rather than dropping
		// transcript text into a gap in the backend's own segmentation.
		{"3", "C", "five"}, // 7.0-8.0
	}
	for i, w := range want {
		g := c.Segments[i]
		if g.ID != w.id || g.Speaker != w.speaker || g.Text != w.text {
			t.Errorf("segment %d = {%q %q %q}, want {%q %q %q}",
				i, g.ID, g.Speaker, g.Text, w.id, w.speaker, w.text)
		}
	}
	// The fused body put EVERY word on speaker 1. Every word's speaker is now
	// re-derived from the segment that owns it — keeping both would publish
	// two answers to one question.
	got := make([]string, len(c.Words))
	for i := range c.Words {
		got[i] = c.Words[i].Speaker
	}
	// "five" at 9.0 is past every segment: rule 3 sends it to the nearest
	// segment starting at or before it, which is 7.0-8.0 (speaker C).
	if strings.Join(got, ",") != "A,A,B,A,C" {
		t.Errorf("word speakers = %v, want the segments' answer A,A,B,A,C", got)
	}
}

// Overlap is a TIE-BREAK, and it is the ratified one: the first candidate in
// the backend's document order. Word "three" at 2.5 is inside segment 1 only;
// a word at 3.0 is inside BOTH segment 1 (2.0-3.5, speaker 2) and segment 2
// (2.8-6.0, speaker 1), and segment 1 wins because it is listed first.
func TestJoinOverlapResolvesToFirstInDocumentOrder(t *testing.T) {
	body := `{"duration":10,"text":"x","words":[{"word":"x","start":3.0,"end":3.1,"speaker":1}]}`
	c := joinOK(t, body, nemoDiarSegments, sttRequest{Format: sttFormatDiarizedJSON})
	if c.Words[0].Speaker != "B" {
		t.Errorf("overlapped word went to %q, want B (the segment listed first)", c.Words[0].Speaker)
	}
	if c.Segments[1].Text != "x" || c.Segments[2].Text != "" {
		t.Errorf("overlap put the word in the wrong segment: %q / %q",
			c.Segments[1].Text, c.Segments[2].Text)
	}
	// Reverse the document order of the two overlapping segments and the
	// answer reverses with it. The rule is the ORDER, not the timing.
	rev := `{"segments":[{"start":2.8,"end":6.0,"speaker":1},{"start":2.0,"end":3.5,"speaker":2}]}`
	c2 := joinOK(t, body, rev, sttRequest{Format: sttFormatDiarizedJSON})
	if c2.Words[0].Speaker != "A" {
		t.Errorf("reversed document order gave %q, want A", c2.Words[0].Speaker)
	}
}

// Rule 6: a diarizer segment no word landed in keeps text:"". That is the
// honest rendering of "nobody was transcribed here", and it is real — on
// 1440 s of AMI meeting audio 49 of 491 segments came back empty, which is a
// diarizer hearing a backchannel the recogniser did not transcribe.
func TestJoinKeepsAnEmptySegmentEmpty(t *testing.T) {
	body := `{"duration":10,"text":"a b","words":[
	 {"word":"a","start":0.1,"end":0.2,"speaker":1},
	 {"word":"b","start":6.0,"end":6.2,"speaker":1}]}`
	diar := `{"segments":[{"start":0.0,"end":1.0,"speaker":1},
	 {"start":2.0,"end":3.0,"speaker":2},
	 {"start":5.0,"end":7.0,"speaker":1}]}`
	c := joinOK(t, body, diar, sttRequest{Format: sttFormatDiarizedJSON})
	if len(c.Segments) != 3 {
		t.Fatalf("segments = %d", len(c.Segments))
	}
	if c.Segments[1].Text != "" || c.Segments[1].Speaker != "B" {
		t.Errorf("the empty segment was not kept honestly: %+v", c.Segments[1])
	}
	if c.Segments[0].Text != "a" || c.Segments[2].Text != "b" {
		t.Errorf("words landed wrongly: %q / %q", c.Segments[0].Text, c.Segments[2].Text)
	}
}

// Rule 4: a word with no start cannot be placed, so it is not placed. It still
// appears in words[]; it carries no speaker, rather than a guessed one.
func TestJoinLeavesAnUnplaceableWordUnlabelled(t *testing.T) {
	body := `{"duration":10,"text":"a b","words":[{"word":"a","speaker":1},{"word":"b","start":1.0,"speaker":1}]}`
	c := joinOK(t, body, nemoDiarSegments, sttRequest{Format: sttFormatDiarizedJSON})
	if c.Words[0].Speaker != "" {
		t.Errorf("a word with no start got speaker %q", c.Words[0].Speaker)
	}
	if c.Words[1].Speaker != "A" {
		t.Errorf("word 1 speaker = %q, want A", c.Words[1].Speaker)
	}
	if c.Segments[0].Text != "b" {
		t.Errorf("segment 0 text = %q, want just the placeable word", c.Segments[0].Text)
	}
}

// A failed second call is a REFUSAL, never a fallback to the fused labels this
// path exists to distrust. Serving the known-bad answer under the good one's
// name is the failure the whole design is about.
func TestJoinFailureRefusesRatherThanFallingBack(t *testing.T) {
	cases := []struct {
		name string
		join sttDiarizeJoin
	}{
		{"call failed", sttDiarizeJoin{Path: "/d", Err: errors.New("connection refused")}},
		{"unreadable body", sttDiarizeJoin{Path: "/d", Raw: []byte(`not json`)}},
		{"no segments", sttDiarizeJoin{Path: "/d", Raw: []byte(`{"segments":[]}`)}},
		{"segments with no speaker", sttDiarizeJoin{Path: "/d", Raw: []byte(`{"segments":[{"start":0,"end":1}]}`)}},
	}
	for _, tc := range cases {
		_, refusal, ok := remapTranscription([]byte(nemoWordsOnly),
			sttRequest{Format: sttFormatDiarizedJSON}, "nemo-joined", false, tc.join)
		if !ok || refusal == nil {
			t.Fatalf("%s: want a refusal, got ok=%v refusal=%v", tc.name, ok, refusal)
		}
		if refusal.status != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", tc.name, refusal.status)
		}
		if !strings.Contains(refusal.msg, "nemo-joined") {
			t.Errorf("%s: refusal does not name the model: %s", tc.name, refusal.msg)
		}
	}
}

// OPT-IN, and the zero value is the whole off switch: no declared path means
// the request behaves exactly as it did before this existed — RULE 2b over the
// fused words.
func TestJoinIsOptInPerBackend(t *testing.T) {
	c := joinOK(t, nemoWordsOnly, nemoDiarSegments,
		sttRequest{Format: sttFormatDiarizedJSON, Diarize: true})
	if len(c.Segments) != 4 {
		t.Fatalf("declared path did not join: %d segments", len(c.Segments))
	}
	out, refusal, ok := remapTranscription([]byte(nemoWordsOnly),
		sttRequest{Format: sttFormatDiarizedJSON, Diarize: true}, "nemo-fused", false,
		sttDiarizeJoin{}) // no declared path
	if refusal != nil || !ok {
		t.Fatalf("undeclared backend refused: %v", refusal)
	}
	var plain canonTranscription
	if err := json.Unmarshal(out, &plain); err != nil {
		t.Fatal(err)
	}
	// RULE 2b: one maximal run of same-speaker words, so one segment.
	if len(plain.Segments) != 1 || plain.Segments[0].Speaker != "A" {
		t.Errorf("undeclared backend did not take the unchanged path: %+v", plain.Segments)
	}
}

// A second pass over the audio is only spent when the answer can carry a
// speaker. The non-streaming `json` floor is {"text":…} and nothing else.
func TestWantsSpeakersGatesTheSecondCall(t *testing.T) {
	cases := []struct {
		q    sttRequest
		want bool
	}{
		{sttRequest{Format: sttFormatDiarizedJSON}, true},
		{sttRequest{Format: sttFormatVerboseJSON, Diarize: true}, true},
		{sttRequest{Format: sttFormatVerboseJSON}, false},
		{sttRequest{Format: sttFormatJSON, Diarize: true}, false},
		{sttRequest{Format: sttFormatJSON, Diarize: true, Stream: true}, true},
		{sttRequest{Format: sttFormatJSON}, false},
	}
	for _, tc := range cases {
		if got := tc.q.wantsSpeakers(); got != tc.want {
			t.Errorf("wantsSpeakers(%+v) = %v, want %v", tc.q, got, tc.want)
		}
	}
	// And the path has to be declared as well as wanted.
	if (sttDiarizeJoin{}).wanted(sttRequest{Format: sttFormatDiarizedJSON}) {
		t.Error("an undeclared backend armed the second call")
	}
}

// The backend's own diarization flag, read but not owned. Anything that is not
// an explicit falsehood reads as asked-for, which is the reading the backend
// gives it.
func TestParseReadsTheBackendDiarizationFlag(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"true", true}, {"1", true}, {"TRUE", true}, {"yes", true},
		{"false", false}, {"0", false}, {"off", false}, {"", false}} {
		body, ct := formBody(map[string]string{"model": "m", "diarization": tc.val})
		q, refusal := parseSTTRequest(body, ct)
		if refusal != nil {
			t.Fatalf("diarization=%q refused: %s", tc.val, refusal.msg)
		}
		if q.Diarize != tc.want {
			t.Errorf("diarization=%q read as %v, want %v", tc.val, q.Diarize, tc.want)
		}
	}
}

// The transcription leg stops paying for a diarizer pass whose answer is about
// to be discarded. Measured: this is what makes the second call +13% and not
// +69%. Every other field survives untouched.
func TestStripDiarizationFlag(t *testing.T) {
	body, ct := formBody(map[string]string{
		"model": "m", "diarization": "true", "response_format": "verbose_json", "language": "en"})
	out, nct, ok := stripDiarizationFlag(body, ct)
	if !ok {
		t.Fatal("strip reported no change on a body that carries the flag")
	}
	got := formFields(t, out, nct)
	if _, present := got["diarization"]; present {
		t.Error("diarization survived the strip")
	}
	for k, v := range map[string]string{"model": "m", "response_format": "verbose_json", "language": "en"} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// A body without the flag is left alone: ok=false means "nothing to do",
	// so the caller keeps the body it had.
	plain, pct := formBody(map[string]string{"model": "m"})
	if _, _, ok := stripDiarizationFlag(plain, pct); ok {
		t.Error("strip claimed a change on a body with no diarization field")
	}
}

// The second request carries the audio and NOTHING else: a diarization
// endpoint has no reason to understand response_format, language or the
// caller's tuning parameters.
func TestDiarizationBodyCarriesOnlyTheAudio(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "m")
	_ = mw.WriteField("response_format", "verbose_json")
	_ = mw.WriteField("num_clusters", "2")
	fw, _ := mw.CreateFormFile("file", "speech.wav")
	_, _ = fw.Write([]byte("RIFFxxxxWAVEfmt "))
	_ = mw.Close()

	out, ct, ok := diarizationBody(buf.Bytes(), mw.FormDataContentType())
	if !ok {
		t.Fatal("could not build the diarization request")
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	names := []string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		names = append(names, part.FormName())
		if part.FormName() == "file" {
			b, _ := io.ReadAll(part)
			if string(b) != "RIFFxxxxWAVEfmt " {
				t.Errorf("audio was not carried verbatim: %q", b)
			}
			if part.FileName() != "speech.wav" {
				t.Errorf("filename = %q", part.FileName())
			}
		}
	}
	if strings.Join(names, ",") != "file" {
		t.Errorf("diarization request carried %v, want just the audio", names)
	}
}

// fetchDiarization against a real listener: the happy path, and the two ways
// it must fail LOUDLY rather than return an empty answer.
func TestFetchDiarization(t *testing.T) {
	var gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotAuth = r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		if r.URL.Path != "/v1/audio/diarizations" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such route"}`))
			return
		}
		_, _ = w.Write([]byte(nemoDiarSegments))
	}))
	defer srv.Close()
	target := proxyTargetFor(t, srv.URL)
	target.Headers = map[string]string{"Authorization": "Bearer k"}

	raw, err := fetchDiarization(t.Context(), target, "/v1/audio/diarizations",
		[]byte("body"), "multipart/form-data; boundary=zz")
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if string(raw) != nemoDiarSegments {
		t.Errorf("body = %s", raw)
	}
	if gotCT != "multipart/form-data; boundary=zz" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("the target's headers were not carried: %q", gotAuth)
	}
	// A non-200 is an error naming the path, not an empty success.
	if _, err := fetchDiarization(t.Context(), target, "/nope", []byte("b"), "x"); err == nil {
		t.Error("a 404 was not reported as an error")
	} else if !strings.Contains(err.Error(), "/nope") {
		t.Errorf("error does not name the endpoint: %v", err)
	}
	if _, err := fetchDiarization(t.Context(), nil, "/d", []byte("b"), "x"); err == nil {
		t.Error("a nil target was not reported as an error")
	}
}

// ---------------------------------------------------------------------------

func proxyTargetFor(t *testing.T, raw string) *config.ProxyTarget {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &config.ProxyTarget{URL: u}
}

func formBody(fields map[string]string) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func formFields(t *testing.T, body []byte, contentType string) map[string]string {
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
		b, _ := io.ReadAll(part)
		out[part.FormName()] = string(b)
	}
	return out
}
