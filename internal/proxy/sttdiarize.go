package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// The two-call diarized join (P35 §12.9(e) RULE 2, owner's ruling 2026-09-25).
//
// THE PROBLEM, MEASURED. NeMo-Speech.cpp has two endpoints that answer "who
// spoke when" and they do not agree. /v1/audio/diarizations returns speaker
// segments and is correct at every length tried. /v1/audio/transcriptions with
// diarization=true fuses the same information onto words[] and FAILS ON LONG
// AUDIO: on 1440 s of CC-BY AMI meeting audio (three speakers, real overlap,
// the 60 s excerpt NeMo ships in test_files/diar looped 24x) it put the first
// 597.6 s — 1,372 words — on a single speaker, then behaved plausibly. At
// 720 s the two endpoints agree on 97.1% of words; at 1440 s, on 62.1%.
//
// WHY THIS IS NOT A LAYERING VIOLATION. Remapping the fused body faithfully
// would publish that error in OpenAI's shape, where a client cannot see it —
// worse than an ugly shape, because it is undetectable downstream. Joining a
// backend's own words to that same backend's own speaker segments is
// arithmetic on returned data, which §9.0 permits; corralling wild-west audio
// backends into a reliable OpenAI API is the job.
//
// WHAT IT COSTS, MEASURED on the same box and files, NeMo at 97a15af on a
// 3080:
//
//	audio    ASR only   fused (1 call)   diarizations   two calls   delta
//	 720 s     7.49 s         16.77 s        11.51 s      19.00 s   +13.3%
//	1440 s    15.31 s         36.49 s        25.85 s      41.16 s   +12.8%
//
// It is +13% and not +100% because the transcription leg no longer asks for
// diarization at all (see stripDiarizationFlag): corrallm pays for one ASR
// pass and one diarizer pass, which is what the fused call already ran. Kept
// naive — leaving diarization=true on the transcription leg and discarding its
// answer — the same work costs +69%. The words are unaffected: the
// transcription bodies with and without diarization=true were verified
// word-for-word and timestamp-for-timestamp identical on both files.
//
// TWO ALLOCATIONS ARE NEVER LIVE AT ONCE. The diarization request is issued
// from the response path, after the transcription response body has been read
// to completion — strictly sequential, never concurrent, and never a second
// in-flight request against a backend whose compute buffer is per-process.
// Measured across a full three-call sequence on the 3080: VRAM rose to a
// 4,740 MiB plateau and the diarization call added nothing on top of it,
// because the buffer is retained and reused. corrallm also still holds the
// model's admission slot while the second call runs, so a maxConcurrent:1
// backend does not see a neighbour's request land beside this one.

// sttDiarizePath is the separate speaker-segmentation endpoint the model
// behind a served name declares, or "" for every model that declares none —
// which is every model until one opts in.
//
// PER-BACKEND AND OPT-IN, exactly as the intake declaration is (audio.go).
// EffectiveModalities already resolves probed-beats-declared, so a probe that
// learns this fact later overrides the config without this file changing.
func (p *Proxy) sttDiarizePath(cfg *config.Config, served string) string {
	if cfg == nil {
		return ""
	}
	// A lane resolves to its first member, exactly as audioFormatsFor does and
	// for the same reason: the intake and the diarization endpoint are facts
	// about the backend that will answer, and the lane's head is the one
	// ConvertFor already resolves against.
	name := served
	if lane, ok := cfg.Lanes[served]; ok && len(lane.Members) > 0 {
		name = lane.Members[0].Model
	}
	m, ok := cfg.Models[name]
	if !ok {
		return ""
	}
	return m.EffectiveModalities(name, p.cost.IsAudioType(m.Type))[config.ModalityAudio].DiarizationPath
}

// stripDiarizationFlag removes the caller's `diarization` form field from the
// request going upstream on the TRANSCRIPTION leg.
//
// IT IS A SAVING, NOT A LOSS. corrallm is about to get the speaker segments
// from the backend's own diarization endpoint and do the join itself, so the
// backend's fused pass would be work whose answer is discarded — and on this
// backend it is most of the added cost. Verified on 720 s and 1440 s: the
// words[] returned with and without the flag are identical, word for word and
// timestamp for timestamp, so nothing corrallm needs is in the flag's answer.
//
// `diarization` is NeMo-Speech.cpp's own parameter name and corrallm forwards
// it untouched on every other path (it is one of the backend request args the
// manifest lists). It is stripped HERE only, only on a model that declared a
// diarizationPath, and only on the leg that is about to be superseded.
func stripDiarizationFlag(body []byte, contentType string) ([]byte, string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", false
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	dropped := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", false
		}
		if part.FileName() == "" && part.FormName() == "diarization" {
			_, _ = io.Copy(io.Discard, part)
			dropped = true
			continue
		}
		w, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", false
		}
		if _, err := io.Copy(w, part); err != nil {
			return nil, "", false
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", false
	}
	if !dropped {
		return nil, "", false
	}
	return out.Bytes(), mw.FormDataContentType(), true
}

// diarizationBody builds the second request: the caller's audio and nothing
// else.
//
// NOTHING ELSE IS DELIBERATE. A diarization endpoint is not the transcription
// endpoint and has no reason to understand response_format, language, prompt,
// or the caller's own tuning parameters; forwarding them would be asking a
// second endpoint questions the caller asked the first one. The audio is taken
// from the body as it stands AFTER transcode-in, so the second call sends the
// same samples the backend just transcribed and never re-runs ffmpeg.
func diarizationBody(body []byte, contentType string) ([]byte, string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", false
	}
	field, filename, data, ok := audioPartFrom(body, params["boundary"])
	if !ok {
		return nil, "", false
	}
	if field == "" {
		field = "file"
	}
	if filename == "" {
		filename = "audio.wav"
	}
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	w, err := mw.CreateFormFile(field, filename)
	if err != nil {
		return nil, "", false
	}
	if _, err := w.Write(data); err != nil {
		return nil, "", false
	}
	if err := mw.Close(); err != nil {
		return nil, "", false
	}
	return out.Bytes(), mw.FormDataContentType(), true
}

// fetchDiarization POSTs the audio to the backend's diarization endpoint and
// returns its raw body.
//
// Called from the response path, after the transcription response is complete.
// Its errors are returned rather than swallowed: a diarized request whose
// speaker segments could not be fetched must FAIL, not fall back to the fused
// labels this whole path exists to distrust. Quietly serving the known-bad
// answer under the good one's name is the exact failure §9.0b calls forwarding
// a bug with a new hostname on it.
func fetchDiarization(ctx context.Context, target *config.ProxyTarget, path string,
	body []byte, contentType string) ([]byte, error) {
	if target == nil || target.URL == nil {
		return nil, fmt.Errorf("no upstream target")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		target.BaseURLString()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 8 MiB is ~130k segments of this shape; a body past that is not a
	// diarization answer and reading it whole would be the backend deciding
	// how much of corrallm's memory to use.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", path, resp.StatusCode, tail(strings.TrimSpace(string(raw)), 200))
	}
	return raw, nil
}

// wireDiarization is the separate endpoint's body, read as generously as every
// other wire type here: NeMo emits an integer speaker, another backend may
// emit a string, and a segment may carry an id of either type or none.
type wireDiarization struct {
	Segments []wireSegment `json:"segments"`
}

// applyDiarization replaces a transcription's speaker information with the
// separate diarization call's, and runs RULE 2 to marry the two.
//
// THE SEGMENTS ARE THE AUTHORITY. Every word's speaker is re-derived from the
// segment that owns it, and any speaker the fused transcription put on a word
// is discarded — keeping both would publish two answers to one question and
// let a client pick the wrong one. A word RULE 2 declines to place (no start,
// clause 4) carries no speaker at all rather than a guessed one.
//
// Labels are assigned in the DIARIZATION document's order of first appearance,
// which is the order of first appearance in the transcript for a backend that
// emits segments in start order. §12.4's rule is unchanged: "A", "B", … stable
// within one response and explicitly not across calls.
//
// It RETURNS the labeller it built, because that labeller — keyed by the
// DIARIZATION endpoint's own speaker identifiers — is now the one that
// describes the response. The transcription body's labeller no longer does,
// and the backend sidecar's back-reference from a voiceprint to "A" has to be
// keyed on whatever the endpoint that decided the labels actually said.
//
// ok=false means the body was not a diarization answer this can read. The
// caller turns that into a refusal, never into a silent fallback.
func applyDiarization(c *canonTranscription, raw []byte) (*speakerLabeller, bool) {
	var w wireDiarization
	if json.Unmarshal(raw, &w) != nil {
		return nil, false
	}
	if len(w.Segments) == 0 {
		return nil, false
	}
	lab := newSpeakerLabeller()
	segs := make([]canonSegment, 0, len(w.Segments))
	for i, s := range w.Segments {
		id := jsonScalarString(s.ID)
		if id == "" {
			id = itoaString(i)
		}
		seg := canonSegment{ID: id, Start: s.Start, End: s.End, Speaker: lab.label(s.Speaker)}
		if s.Text != nil {
			seg.Text = *s.Text
		}
		segs = append(segs, seg)
	}
	if !lab.any() {
		return nil, false // segments with no speaker in them are not a diarization
	}
	owner := assignWordsToSegments(segs, c.Words)
	buckets := make([][]string, len(segs))
	for wi, idx := range owner {
		if idx < 0 {
			c.Words[wi].Speaker = "" // rule 4: unplaced, so unlabelled
			continue
		}
		c.Words[wi].Speaker = segs[idx].Speaker
		buckets[idx] = append(buckets[idx], c.Words[wi].Word)
	}
	for i := range segs {
		if segs[i].Text == "" {
			segs[i].Text = joinWordText(buckets[i]) // rules 5 and 6
		}
	}
	c.Segments = segs
	return lab, true
}

// sttDiarizeJoin is what the response path carries from the request side to
// the remap: whether a second call is owed, and what it answered.
type sttDiarizeJoin struct {
	// Path is the declared endpoint. Empty means this model does not opt in
	// and nothing in this file runs.
	Path string
	// Raw is the diarization endpoint's body, set once the call has been made.
	Raw []byte
	// Err is why it could not be made or could not be read. A diarized
	// response is refused rather than served without it.
	Err error
}

// wanted reports whether this request should make the second call at all: the
// model declared an endpoint AND the caller asked for speaker-labelled output.
// A plain `json` or `verbose_json` transcription with no diarization asked for
// pays nothing.
func (j sttDiarizeJoin) wanted(q sttRequest) bool { return j.Path != "" && q.wantsSpeakers() }

// diarizeTimeout bounds the second call.
//
// It is layered ON the request context rather than replacing it: a client that
// hangs up, and a preemption, still abort this call, because a second pass
// over the audio nobody is waiting for is GPU time spent on nothing. What it
// adds is an upper bound for the usual case, where corrallm has no configured
// deadline at all. Sized off measurement with headroom — the diarizer ran
// 25.8 s on 1440 s of audio, ~56x realtime, and the longest upload the box
// admits is an hour. Where an operator HAS configured a request deadline, the
// tighter of the two wins, which is what configuring one meant.
const diarizeTimeout = 15 * time.Minute
