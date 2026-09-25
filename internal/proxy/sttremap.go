package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Remap-out, the canonical STT body (P35 §12). corrallm publishes ONE
// transcription shape and maps every backend onto it.
//
// THE THREE RULINGS THIS IMPLEMENTS, in the order they constrain the code:
//
//	§9.0  no backend's shape is canonical; corrallm publishes one.
//	§9.0b accept generously; produce and document strictly. Asymmetric — the
//	      tolerance is on the way IN only.
//	§9.0c and the shape published is OpenAI's, wherever OpenAI published one.
//
// So: the wire types below accept oidio's UUID speaker and NeMo's integer
// speaker, an integer or a string `id`, a word with or without an `end`. The
// canonical types emit exactly one of each, and a field no backend answered is
// ABSENT — never null, never a placeholder, never computed.
//
// WHAT THIS DELIBERATELY DOES NOT DO. It does not stabilise a speaker across
// calls: oidio mints a new UUID every request, and turning that into a durable
// identity is speaker recognition, not a remap (§9.0, §9.4). It does not
// compute `words[].end` from the next word's start (§12.6). It does not derive
// `num_speakers` (§12.8) — a caller counts the distinct `segments[].speaker`.
// It does not invent a duration for a backend that measured none (§12.10.2).

// The canonical response_format values. `text`, `srt` and `vtt` are OpenAI's
// too, but they are plain text forms with nothing to remap — they are
// forwarded untouched and never reach this file.
const (
	sttFormatJSON         = "json"
	sttFormatVerboseJSON  = "verbose_json"
	sttFormatDiarizedJSON = "diarized_json"
)

// sttUpstreamFormat is what corrallm asks a backend for, on every canonical
// request, whatever the caller asked for.
//
// ONE UPSTREAM SHAPE, NOT A PER-BACKEND SWITCH (§12.8). Three facts force it.
// `diarized_json` is corrallm's own name here and no backend on this box knows
// it — measured 2026-09-23: oidio silently IGNORES an unknown response_format
// and answers with its model default, which is the silently-wrong failure this
// layer exists to end. NeMo-Speech.cpp 400s on anything but verbose_json
// (source-read), so a caller asking for `json` from it would get an error for
// a format OpenAI defines. And projecting a verbose body down to `{"text":…}`
// is a field read, not an invention. The cost is bandwidth on a local socket:
// measured on oidio-stt, 56 bytes upstream becomes 213.
//
// Verified not to disturb a backend that really streams: oidio-stt with
// response_format=verbose_json and stream=true returns the same 648-byte event
// stream it returns for json.
const sttUpstreamFormat = sttFormatVerboseJSON

// sttRequest is what the caller asked of an STT route, read once from the
// multipart form before admission.
type sttRequest struct {
	// Format is the canonical response_format asked for, "" when the caller
	// asked for something this layer does not remap (text/srt/vtt) or the body
	// could not be read as a form.
	Format string
	// IncludeBackend is OpenAI's include[] opt-in, carrying corrallm's own
	// `backend` value (§12.5, §12.9(d)): the sidecar is available, never
	// ambient. 5.2 KB of voiceprint per speaker is not a default.
	IncludeBackend bool
	// Diarize records that the caller asked a BACKEND for speaker labels under
	// the backend's own parameter name — NeMo-Speech.cpp's `diarization=true`,
	// which corrallm forwards untouched like every other backend request arg.
	//
	// READ, NOT OWNED. corrallm does not define this parameter and does not
	// refuse it; it reads it because `verbose_json` alone does not say whether
	// the caller wants speakers, and the two-call join must not spend a second
	// pass over the audio on a caller who never asked for one.
	Diarize bool
	// Stream is the caller's stream=true, which decides whether the RESPONSE
	// can carry a speaker at all on response_format=json: the non-streaming
	// `json` floor is `{"text":…}` and nothing else (§12.1), while the
	// streaming form's output contract is the EVENT schema (§9.1) and that
	// does carry segments.
	Stream bool
}

// wantsSpeakers reports whether this request asked for speaker-labelled output
// AND will get a body that can carry it.
//
// Both halves matter, because this is what decides whether a second pass over
// the audio is spent (sttdiarize.go). `diarized_json` always wants them;
// `verbose_json` wants them when the caller asked a backend for them under its
// own flag; plain `json` returns text and nothing else, so paying a diarizer
// for it would buy the caller nothing — unless the caller is streaming, where
// the event schema is the contract and does carry segments.
func (q sttRequest) wantsSpeakers() bool {
	if q.Format == sttFormatJSON && !q.Stream {
		return false
	}
	return q.Format == sttFormatDiarizedJSON || q.Diarize
}

// remaps reports whether a response to this request gets the canonical
// treatment at all.
func (q sttRequest) remaps() bool {
	switch q.Format {
	case sttFormatJSON, sttFormatVerboseJSON, sttFormatDiarizedJSON:
		return true
	}
	return false
}

// sttRefusal is a refusal at the door, in OpenAI's error shape.
type sttRefusal struct {
	status int
	msg    string
	param  string
}

// parseSTTRequest reads the canonical request fields out of a buffered
// multipart body, and refuses the ones corrallm cannot honour.
//
// REFUSING AT THE DOOR IS THE POINT (§9.0b). Accept-generously governs the
// FORM of a request and never a capability the model does not have: a
// parameter corrallm reads, cannot serve, and drops in silence is the
// stream=true bug with a different field name on it. So an include[] value
// that produces nothing, and a known_speaker_names[] corrallm cannot map, are
// named and refused before the request queues — not answered with a plausible
// body after a backend has spent GPU time on it.
func parseSTTRequest(body []byte, contentType string) (sttRequest, *sttRefusal) {
	var q sttRequest
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return q, nil // not a form this reads; forwarded exactly as before
	}
	// Absent response_format is OpenAI's `json`, and `json` is the floor every
	// plain client stands on.
	q.Format = sttFormatJSON
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		name := part.FormName()
		if part.FileName() != "" || !sttFormField(name) {
			_ = part.Close()
			continue
		}
		v, _ := io.ReadAll(io.LimitReader(part, 4<<10))
		_ = part.Close()
		val := strings.TrimSpace(string(v))
		switch name {
		case "response_format":
			q.Format = strings.ToLower(val)
		case "diarization":
			// Accept generously on the FORM: a flag is a flag however the
			// client spelled it. Anything that is not an explicit falsehood
			// reads as asked-for, which is the same reading the backend gives
			// it — corrallm must not decide a caller wanted speakers when the
			// backend will not produce them, or the reverse.
			switch strings.ToLower(val) {
			case "", "0", "false", "no", "off":
			default:
				q.Diarize = true
			}
		case "include", "include[]":
			// Accept generously on the FORM: OpenAI repeats `include[]`, some
			// clients send `include`, some send one comma-separated value.
			// All three mean the same request.
			for _, item := range strings.Split(val, ",") {
				switch strings.ToLower(strings.TrimSpace(item)) {
				case "":
				case "backend":
					q.IncludeBackend = true
				default:
					return q, &sttRefusal{http.StatusBadRequest, "include[] value " +
						strings.TrimSpace(item) + " is not served on this route; corrallm " +
						"serves include[]=backend, and emits no logprobs (no backend here " +
						"reports per-token confidence on a batch transcription).", "include"}
				}
			}
		case "known_speaker_names", "known_speaker_names[]":
			if val == "" {
				break // reserved and empty is a no-op, not a request
			}
			// §12.9(h), §9.0c: the NAME is OpenAI's and corrallm must not spell
			// it differently — that half is settled and is why this case exists
			// rather than the field falling through as an unknown form field.
			// The MECHANISM is not implemented: OpenAI pairs names with
			// reference audio, oidio takes `known_speakers` UUIDs a caller kept
			// from a previous call, and mapping one onto the other is speaker
			// recognition rather than a rename. Answering with "A" when the
			// caller asked for "Alice" is the lie §12.7 refuses.
			return q, &sttRefusal{http.StatusBadRequest, "known_speaker_names is " +
				"recognised under OpenAI's spelling but not implemented: no backend " +
				"corrallm fronts maps a name onto a voice. Omit it and read the " +
				"ordinal labels (A, B, …), which are stable within one response only.",
				"known_speaker_names"}
		}
	}
	if !q.remaps() {
		q.Format = "" // text/srt/vtt and anything unknown: forwarded, not remapped
	}
	return q, nil
}

func sttFormField(name string) bool {
	switch name {
	case "response_format", "include", "include[]", "known_speaker_names", "known_speaker_names[]",
		"diarization":
		return true
	}
	return false
}

// rewriteSTTUpstream normalises the request corrallm sends on: one
// response_format for every canonical ask, and no corrallm-owned parameters.
//
// include[] is STRIPPED rather than forwarded. It is corrallm's own opt-in for
// corrallm's own sidecar; a backend that happens to define the same name would
// be answering a question it was not asked.
func rewriteSTTUpstream(body []byte, contentType string) ([]byte, string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", false
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	sawFormat := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", false
		}
		name := part.FormName()
		if part.FileName() == "" && (name == "include" || name == "include[]") {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		// CreatePart with the original header, not CreateFormField: the file
		// part's own Content-Type is a fact the backend may read, and this
		// rewrite runs on LOCAL backends too, where nothing re-encoded the form
		// before.
		w, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", false
		}
		if part.FileName() == "" && name == "response_format" {
			sawFormat = true
			if _, err := io.WriteString(w, sttUpstreamFormat); err != nil {
				return nil, "", false
			}
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		if _, err := io.Copy(w, part); err != nil {
			return nil, "", false
		}
	}
	if !sawFormat {
		w, err := mw.CreateFormField("response_format")
		if err != nil {
			return nil, "", false
		}
		if _, err := io.WriteString(w, sttUpstreamFormat); err != nil {
			return nil, "", false
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", false
	}
	return out.Bytes(), mw.FormDataContentType(), true
}

// ---------------------------------------------------------------------------
// The canonical body
// ---------------------------------------------------------------------------

type canonSegment struct {
	// String everywhere, streaming and not (§9.0b(c)). OpenAI contradicts
	// itself — the transcript.text.segment EVENT carries a string, whisper-1's
	// verbose_json an integer — and corrallm may not. The event schema wins
	// because it is the one actually specifying diarized output.
	ID      string   `json:"id"`
	Start   *float64 `json:"start,omitempty"`
	End     *float64 `json:"end,omitempty"`
	Text    string   `json:"text"`
	Speaker string   `json:"speaker,omitempty"`
}

type canonWord struct {
	Word  string   `json:"word"`
	Start *float64 `json:"start,omitempty"`
	// OMITTED when the backend does not report it (§12.6, §9.0b(f)). oidio says
	// when a word STARTS and nothing more; computing an end from the next
	// word's start emits a number nobody measured, and is wrong exactly where
	// it matters — a pause, a breath, the end of a turn.
	End     *float64 `json:"end,omitempty"`
	Speaker string   `json:"speaker,omitempty"`
}

type canonUsage struct {
	Type    string  `json:"type"`
	Seconds float64 `json:"seconds"`
}

type canonTranscription struct {
	Task     string         `json:"task,omitempty"`
	Language string         `json:"language,omitempty"`
	Duration *float64       `json:"duration,omitempty"`
	Text     string         `json:"text"`
	Segments []canonSegment `json:"segments,omitempty"`
	Words    []canonWord    `json:"words,omitempty"`
	Usage    *canonUsage    `json:"usage,omitempty"`
	Backend  map[string]any `json:"backend,omitempty"`
}

// The backend's body, read generously. Every type here is wider than the
// canonical on purpose: this is the IN side of §9.0b.
type wireSegment struct {
	ID      json.RawMessage `json:"id"`
	Start   *float64        `json:"start"`
	End     *float64        `json:"end"`
	Text    *string         `json:"text"`
	Speaker json.RawMessage `json:"speaker"`
}

type wireWord struct {
	Word    string          `json:"word"`
	Start   *float64        `json:"start"`
	End     *float64        `json:"end"`
	Speaker json.RawMessage `json:"speaker"`
}

type wireTranscription struct {
	Task     *string       `json:"task"`
	Language *string       `json:"language"`
	Duration *float64      `json:"duration"`
	Text     *string       `json:"text"`
	Segments []wireSegment `json:"segments"`
	Words    []wireWord    `json:"words"`
}

// ---------------------------------------------------------------------------
// RULE 1 — the speaker label, and what happens after Z
// ---------------------------------------------------------------------------

// sttSpeakerLabel is the ordinal speaker label, 1-based: 1→"A" … 26→"Z",
// 27→"AA", 28→"AB" … 52→"AZ", 53→"BA" … 702→"ZZ", 703→"AAA".
//
// THE 27-SPEAKER ROLLOVER IS A RULE, NOT A RUNTIME SURPRISE (§9.0b). OpenAI
// prints capital letters and documents no answer for what follows Z, so
// corrallm has to have one and it has to be in the schema. Bijective base-26 —
// spreadsheet columns — is chosen because it agrees with OpenAI EXACTLY over
// the whole range OpenAI specifies, extends without a separator or a second
// character class, is unbounded, and is a form every caller has already seen.
//
// Rejected: falling back to `speaker_27` after Z (two label forms in one
// response, so a client must parse both); wrapping to "A" (two speakers with
// one label, which is the schema lying); refusing the 27th speaker (a refusal
// for a transcript the backend produced correctly).
//
// A label is still STABLE WITHIN ONE RESPONSE ONLY. "A" in two responses is
// two different assertions, at 2 speakers and at 200.
func sttSpeakerLabel(n int) string {
	if n <= 0 {
		return ""
	}
	var b []byte
	for n > 0 {
		n--
		b = append(b, byte('A'+n%26))
		n /= 26
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// speakerLabeller assigns labels BY ORDER OF FIRST APPEARANCE, where the order
// is the backend's own document order: every `segments[]` entry in array
// order, then every `words[]` entry in array order. Nothing is sorted — a
// backend's array order is the transcript's order, and re-sorting it by start
// time would be corrallm asserting a chronology the backend did not.
type speakerLabeller struct {
	labels map[string]string
	order  []string // raw keys, in the order they were first seen
}

func newSpeakerLabeller() *speakerLabeller {
	return &speakerLabeller{labels: map[string]string{}}
}

// label maps one raw backend speaker key onto its canonical label. An absent
// or unreadable key stays "" — a segment with no speaker gets no `speaker`
// field, rather than a placeholder that asserts one (§12.7).
func (l *speakerLabeller) label(raw json.RawMessage) string {
	key := jsonScalarString(raw)
	if key == "" {
		return ""
	}
	if lab, ok := l.labels[key]; ok {
		return lab
	}
	lab := sttSpeakerLabel(len(l.order) + 1)
	l.labels[key] = lab
	l.order = append(l.order, key)
	return lab
}

func (l *speakerLabeller) any() bool { return len(l.order) > 0 }

// ---------------------------------------------------------------------------
// RULE 2 — building a segment from a backend that returned only words
// ---------------------------------------------------------------------------

// joinWordsIntoSegments fills the `text` of segments a backend returned
// WITHOUT one, from the words it returned beside them.
//
// THE RULE, written here because two implementations will otherwise disagree
// at a boundary (§12.9(e), ratified with the three cases the proposal left
// undefined):
//
//  1. A word belongs to the FIRST segment, in `segments[]` document order,
//     whose half-open interval [start, end) contains the word's `start`.
//     Half-open, so a word starting exactly at a boundary belongs to the
//     segment that is starting, not the one that is ending.
//  2. A segment with no `end` runs to the next segment's `start`, or to
//     infinity when it is the last. A backend that reports a turn's beginning
//     and not its end has still said where the next one starts.
//  3. A word matching no segment is appended to the nearest segment STARTING
//     AT OR BEFORE it. A word starting before every segment goes to the first
//     — "nearest preceding" has no referent there, and dropping the word would
//     lose transcript text to a gap in the backend's own segmentation.
//  4. A word with no `start` at all cannot be placed and is not placed. It
//     still appears in `words[]`; it is absent from every segment's text.
//  5. A segment's text is its words in the order they were RETURNED, not
//     re-sorted. A single space joins two of them only when the left does not
//     already end with whitespace and the right does not begin with it — oidio
//     writes "The", the same recogniser's stream writes " The", and a caller
//     must not be able to tell which by counting spaces. The result is
//     trimmed.
//  6. A segment that gathers no words keeps `text: ""`. That is the honest
//     rendering of "no word landed here" and is the only case where a
//     canonical segment's text is empty.
//
// NOTHING IS INVENTED HERE. Every character emitted was returned by the
// backend; this is arithmetic on returned data, which §9.0 permits, and it is
// the whole of what it permits.
//
// NO LONGER UNEXERCISED. It was recorded as unit-covered-only because the
// backend that motivated it answers /v1/audio/transcriptions with no
// `segments[]` at all (p35-nemo-live §4), so only RULE 2b ever fired. The
// two-call join (sttdiarize.go) is what makes RULE 2 fire on real data: the
// segments come from the backend's own /v1/audio/diarizations and the words
// from its transcription, and this is the rule that marries them.
func joinWordsIntoSegments(segs []canonSegment, words []canonWord) {
	if len(segs) == 0 || len(words) == 0 {
		return
	}
	need := false
	for i := range segs {
		if segs[i].Text == "" {
			need = true
			break
		}
	}
	if !need {
		return
	}
	owner := assignWordsToSegments(segs, words)
	buckets := make([][]string, len(segs))
	for wi, idx := range owner {
		if idx >= 0 {
			buckets[idx] = append(buckets[idx], words[wi].Word)
		}
	}
	for i := range segs {
		if segs[i].Text == "" {
			segs[i].Text = joinWordText(buckets[i]) // rule 5, rule 6
		}
	}
}

// assignWordsToSegments is RULE 2 clauses 1–4 on their own: which segment owns
// each word, by index, or -1 for a word the rule declines to place.
//
// SEPARATE FROM THE TEXT JOIN because two callers want two different halves of
// the same answer. joinWordsIntoSegments wants the words, to fill a textless
// segment. The two-call join (sttdiarize.go) also wants the SPEAKER, because
// there the segments are the authority on who spoke and the word's own
// speaker field — if the backend even set one — is the thing being replaced.
//
// OVERLAPPING SEGMENTS, DELIBERATELY. Simultaneous speech is real and a
// diarizer that reports it emits segments that overlap in time: measured on
// 1440 s of AMI meeting audio, 80 words of 3,284 (2.4%) start inside two
// segments at once. For those words the backend has said two people were
// talking and has NOT said which one produced the word, so corrallm cannot
// know — every possible answer is a tie-break, and the only real requirements
// are that it be deterministic and written down. RULE 2 clause 1 is that
// tie-break: the FIRST candidate in the backend's own document order wins,
// which for a diarizer that emits segments in start order means the speaker
// whose turn began EARLIER keeps the word.
//
// The alternative considered and rejected: prefer a candidate that also
// contains the word's END (more of the word inside it). It is defensible and
// it is not worth a second rule — measured on the same audio it moves 3 words
// of 3,284, it only applies when the backend reports word ends, and a rule
// that fires on 0.09% of words is a second thing every reimplementation has to
// get right for no measurable gain.
func assignWordsToSegments(segs []canonSegment, words []canonWord) []int {
	owner := make([]int, len(words))
	if len(segs) == 0 {
		for i := range owner {
			owner[i] = -1
		}
		return owner
	}
	// Rule 2: a missing end runs to the next segment's start.
	ends := make([]float64, len(segs))
	const inf = 1e308
	for i := range segs {
		switch {
		case segs[i].End != nil:
			ends[i] = *segs[i].End
		case i+1 < len(segs) && segs[i+1].Start != nil:
			ends[i] = *segs[i+1].Start
		default:
			ends[i] = inf
		}
	}
	for wi := range words {
		if words[wi].Start == nil {
			owner[wi] = -1 // rule 4: cannot be placed, so it is not placed
			continue
		}
		at := *words[wi].Start
		idx := -1
		for i := range segs {
			if segs[i].Start == nil {
				continue
			}
			// Rule 1, half-open, FIRST match in document order: a word
			// starting exactly on a boundary belongs to the segment that is
			// starting, and a word inside two overlapping segments belongs to
			// the one the backend listed first. The break is the tie-break.
			if at >= *segs[i].Start && at < ends[i] {
				idx = i
				break
			}
		}
		if idx < 0 { // rule 3: the nearest segment starting at or before it
			for i := range segs {
				if segs[i].Start != nil && *segs[i].Start <= at {
					idx = i
				}
			}
		}
		if idx < 0 {
			idx = 0 // before every segment
		}
		owner[wi] = idx
	}
	return owner
}

// segmentsFromWords derives segments for a backend that returned
// speaker-bearing words and NO segments at all.
//
// RULE 2b, the other half of the same question. The canonical `diarized_json`
// requires `segments[]` because OpenAI's does, and a backend can satisfy
// "who spoke when" entirely in `words[]` — NeMo-Speech.cpp's verbose_json does.
// Each MAXIMAL RUN of consecutive words sharing a speaker becomes one segment:
// `start` is the run's first word's start; `end` is its last word's end when
// the backend reported one and is OMITTED otherwise (never the next run's
// start — that would be a measurement nobody took); `text` joins by rule 5;
// `id` is the run's zero-based index, rendered as a string.
//
// Consecutive in RETURNED order, not in time. Re-sorting would assert a
// chronology the backend did not, and a backend that returns words out of
// order has a bug this must not paper over.
//
// Also unit-covered only — see joinWordsIntoSegments.
func segmentsFromWords(words []canonWord) []canonSegment {
	var out []canonSegment
	i := 0
	for i < len(words) {
		if words[i].Speaker == "" {
			i++
			continue
		}
		j := i
		var texts []string
		for j < len(words) && words[j].Speaker == words[i].Speaker {
			texts = append(texts, words[j].Word)
			j++
		}
		seg := canonSegment{
			ID:      itoaString(len(out)),
			Start:   words[i].Start,
			End:     words[j-1].End,
			Text:    joinWordText(texts),
			Speaker: words[i].Speaker,
		}
		out = append(out, seg)
		i = j
	}
	return out
}

func joinWordText(parts []string) string {
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if sb.Len() > 0 && !endsSpace(sb.String()) && !startsSpace(p) {
			sb.WriteByte(' ')
		}
		sb.WriteString(p)
	}
	return strings.TrimSpace(sb.String())
}

func startsSpace(s string) bool {
	for _, r := range s {
		return unicode.IsSpace(r)
	}
	return false
}

func endsSpace(s string) bool {
	r := []rune(s)
	return len(r) > 0 && unicode.IsSpace(r[len(r)-1])
}

func itoaString(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// The remap itself
// ---------------------------------------------------------------------------

// remapTranscription turns a backend's transcription body into the canonical
// one. ok=false means "not a body this can read" — the signal to forward the
// original untouched, which is the honest failure for a transform that could
// not run.
//
// normalizeOnly is the stream=true case: the EVENT schema is the output
// contract there (§9.1), not response_format, so the body is relabelled and
// type-normalised but never projected down to `{"text":…}` — a client that
// asked for a stream still gets its segment events.
//
// join carries the second upstream call (sttdiarize.go) when the model
// declared a separate diarization endpoint. When it is armed, that call's
// segments REPLACE whatever speaker information the transcription carried,
// and a failure to get them is a refusal rather than a quiet fallback.
func remapTranscription(raw []byte, q sttRequest, model string, normalizeOnly bool,
	join sttDiarizeJoin) (out []byte, refusal *sttRefusal, ok bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil, nil, false
	}
	var w wireTranscription
	if json.Unmarshal(raw, &w) != nil {
		return nil, nil, false
	}
	if w.Text == nil && len(w.Segments) == 0 && len(w.Words) == 0 {
		return nil, nil, false // not a transcription reply
	}

	lab := newSpeakerLabeller()
	var c canonTranscription
	for _, s := range w.Segments {
		seg := canonSegment{
			ID:      jsonScalarString(s.ID),
			Start:   s.Start,
			End:     s.End,
			Speaker: lab.label(s.Speaker),
		}
		if s.Text != nil {
			seg.Text = *s.Text
		}
		c.Segments = append(c.Segments, seg)
	}
	for _, x := range w.Words {
		c.Words = append(c.Words, canonWord{
			Word: x.Word, Start: x.Start, End: x.End, Speaker: lab.label(x.Speaker),
		})
	}
	// A segment with no id of its own gets its index, so `id` is present on
	// every canonical segment as OpenAI's shape requires.
	for i := range c.Segments {
		if c.Segments[i].ID == "" {
			c.Segments[i].ID = itoaString(i)
		}
	}
	// THE TWO-CALL JOIN COMES FIRST, because it is the authority on who spoke
	// and everything below would otherwise derive segments from words whose
	// speakers are the ones being replaced. A model that declared no
	// diarization endpoint never reaches this branch.
	joined := false
	if join.wanted(q) {
		diarLab, applied := (*speakerLabeller)(nil), false
		if join.Err == nil {
			diarLab, applied = applyDiarization(&c, join.Raw)
		}
		switch {
		case join.Err != nil:
			return nil, &sttRefusal{http.StatusBadGateway,
				"model " + model + " serves speaker segments from a separate endpoint and " +
					"that call failed, so this response cannot be speaker-labelled: " +
					join.Err.Error() + ". The transcript itself succeeded — retry, or ask " +
					"for response_format=verbose_json without diarization.", "response_format"}, true
		case !applied:
			return nil, &sttRefusal{http.StatusBadGateway,
				"model " + model + " serves speaker segments from a separate endpoint and " +
					"that endpoint returned no readable segments, so this response cannot " +
					"be speaker-labelled. The transcript itself succeeded — retry, or ask " +
					"for response_format=verbose_json without diarization.", "response_format"}, true
		default:
			joined = true
			// The labels now come from the diarization document, so the
			// labeller that read the transcription body no longer describes
			// the response. Anything downstream that asks it (the §12.7
			// refusal, the backend sidecar's back-references) must ask the
			// diarization endpoint's labeller instead.
			lab = diarLab
		}
	}
	switch {
	case joined:
		// Already done: segments from the separate call, text by RULE 2.
	case len(c.Segments) == 0 && len(c.Words) > 0 && lab.any():
		c.Segments = segmentsFromWords(c.Words) // rule 2b
	default:
		joinWordsIntoSegments(c.Segments, c.Words) // rule 2
	}

	if w.Task != nil {
		c.Task = *w.Task
	}
	if w.Language != nil {
		c.Language = *w.Language
	}
	c.Duration = w.Duration
	if w.Text != nil {
		c.Text = *w.Text
	}
	if c.Text == "" && len(c.Segments) > 0 {
		// A field read, not an invention: the transcript is the segments the
		// backend returned, in the order it returned them.
		parts := make([]string, 0, len(c.Segments))
		for _, s := range c.Segments {
			parts = append(parts, s.Text)
		}
		c.Text = joinWordText(parts)
	}
	if c.Duration != nil {
		// Real data: the backend measured the audio. `usage` is OPTIONAL in the
		// published contract for the same reason the streaming form made it
		// optional (§12.10.2) — a backend that measured no duration gets no
		// usage block rather than a computed one.
		c.Usage = &canonUsage{Type: "duration", Seconds: *c.Duration}
	}

	// §12.7: the caller asked for speaker labels and this model produces none.
	// Returning a body without them at 200 is the stream=true bug with a
	// different field name; degrading silently is what §9.0b calls forwarding a
	// bug with a new hostname on it.
	if q.Format == sttFormatDiarizedJSON && !lab.any() {
		return nil, &sttRefusal{http.StatusBadRequest,
			"model " + model + " does not return speaker-labelled output, so " +
				"response_format=diarized_json cannot be served for it. Use " +
				"response_format=verbose_json.", "response_format"}, true
	}

	if q.IncludeBackend {
		c.Backend = buildBackendSidecar(top, model, lab, c.Segments)
	}

	if !normalizeOnly && q.Format == sttFormatJSON {
		// The floor, and it is the whole floor: a plain OpenAI client reads
		// `.text` and is finished. No speaker, no segments, no sidecar, on any
		// model (§12.1).
		c = canonTranscription{Text: c.Text}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, nil, false
	}
	return b, nil, true
}

// buildBackendSidecar collects everything the canonical has no home for, under
// the name of the model that produced it (§12.5).
//
// NAMESPACED, NOT MERGED. A caller cannot tell a corrallm field from a backend
// field once they are mixed, and the next backend's `speakers[]` will not mean
// what this one's means. The namespace makes "this is that model's, and it may
// be gone tomorrow" a property of where the data sits instead of a paragraph
// someone has to have read.
//
// The name is the MODEL corrallm served, not a vendor string. §12.2 wrote
// "oidio"; corrallm has no vendor field to read and inventing one would be the
// manifest's fourth false claim. The served model name is a fact corrallm
// knows for certain and the caller can correlate with /v1/models.
func buildBackendSidecar(top map[string]json.RawMessage, model string, lab *speakerLabeller,
	canon []canonSegment) map[string]any {
	side := map[string]any{}
	// Anything at the top level the canonical drops, verbatim. A future
	// backend's extras survive without this file learning their names.
	canonical := map[string]bool{
		"task": true, "language": true, "duration": true, "text": true,
		"segments": true, "words": true, "usage": true, "speakers": true,
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !canonical[k] && k != "name" {
			side[k] = top[k]
		}
	}
	side["name"] = model

	// speakers[]: the backend's own objects, each carrying the canonical label
	// beside its own identity, so a caller wanting the voiceprint for "A" has a
	// lookup and not a join on ordering.
	byKey := lab.labels
	var speakers []map[string]json.RawMessage
	if rawSp, ok := top["speakers"]; ok {
		if json.Unmarshal(rawSp, &speakers) != nil {
			speakers = nil
		}
	}
	if speakers != nil {
		for _, sp := range speakers {
			for _, idField := range []string{"uuid", "id", "index", "speaker"} {
				if v, ok := sp[idField]; ok {
					if l, ok := byKey[jsonScalarString(v)]; ok {
						sp["label"], _ = json.Marshal(l) //nolint:errcheck // a string always marshals
					}
					break
				}
			}
		}
	} else if lab.any() {
		// A backend with no speakers[] of its own — NeMo's integer index is the
		// motivating case (§12.8: "the integer survives in
		// backend.speakers[].index"). The raw key is preserved beside the
		// label, under the name its own type implies.
		for _, key := range lab.order {
			entry := map[string]json.RawMessage{}
			entry["label"], _ = json.Marshal(lab.labels[key]) //nolint:errcheck
			field := "uuid"
			if isJSONNumberString(key) {
				field = "index"
			}
			entry[field] = json.RawMessage(quoteOrRaw(key, field == "index"))
			speakers = append(speakers, entry)
		}
	}
	if len(speakers) > 0 {
		side["speakers"] = speakers
	}

	// segments[]: ONLY the keys the canonical dropped, keyed back to the
	// canonical id. oidio's documented `overlap` is the case this exists for.
	var extras []map[string]json.RawMessage
	if rawSegs, ok := top["segments"]; ok {
		var objs []map[string]json.RawMessage
		if json.Unmarshal(rawSegs, &objs) == nil {
			for i, o := range objs {
				e := map[string]json.RawMessage{}
				for k, v := range o {
					switch k {
					case "id", "start", "end", "text", "speaker":
					default:
						e[k] = v
					}
				}
				if len(e) == 0 {
					continue
				}
				id := itoaString(i)
				if i < len(canon) {
					id = canon[i].ID
				}
				e["id"], _ = json.Marshal(id) //nolint:errcheck
				extras = append(extras, e)
			}
		}
	}
	if len(extras) > 0 {
		side["segments"] = extras
	}
	if len(side) == 1 { // name only: nothing the canonical could not carry
		return nil
	}
	return side
}

func isJSONNumberString(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c == '-' || (c >= '0' && c <= '9')
}

func quoteOrRaw(s string, raw bool) []byte {
	if raw {
		return []byte(s)
	}
	b, _ := json.Marshal(s) //nolint:errcheck
	return b
}

// ---------------------------------------------------------------------------
// The response writer
// ---------------------------------------------------------------------------

// sttRemapWriter holds a JSON transcription body, remaps it, and writes the
// canonical one — or gets out of the way.
//
// IT HOLDS THE HEADERS for the same reason sttStreamWriter does: whether the
// body is really a transcription is only knowable once it has parsed, and a
// body that turns out not to be one must still leave with its original headers
// and status. The failure mode is "exactly what corrallm did before".
//
// IT SITS OUTSIDE statusCapture, so metering still records the BACKEND's body
// and its status. One consequence is named rather than hidden: when the remap
// refuses a diarized_json request that the backend answered 200, the activity
// row records the backend's 200 while the caller receives 400. The work was
// really done and really cost GPU time; the row says what was served upstream.
// Moving that refusal to the door needs a model to be able to DECLARE that it
// diarizes, which nothing in corrallm's model record does today.
type sttRemapWriter struct {
	http.ResponseWriter
	req   sttRequest
	model string
	// streaming: the caller asked for a stream, so the event schema is the
	// output contract and the body is normalised rather than projected down.
	streaming bool
	// join is the second upstream call, for a model that declared a separate
	// diarization endpoint (sttdiarize.go). Zero value = this model did not
	// opt in and nothing about its path changes.
	join sttDiarizeJoin

	decided   bool
	buffering bool
	code      int
	body      bytes.Buffer

	remapped bool
	refused  bool
}

func (s *sttRemapWriter) WriteHeader(code int) {
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
	if code == http.StatusOK && isJSONContentType(s.Header().Get("Content-Type")) {
		s.buffering = true
		return
	}
	// A real event stream, or an error: forwarded. An error must stay a JSON
	// error with its own status.
	s.ResponseWriter.WriteHeader(code)
}

func (s *sttRemapWriter) Write(b []byte) (int, error) {
	if !s.decided {
		s.WriteHeader(http.StatusOK)
	}
	if s.buffering {
		return s.body.Write(b)
	}
	return s.ResponseWriter.Write(b)
}

func (s *sttRemapWriter) Flush() {
	if s.buffering {
		return
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish releases the held body — canonical if it remapped, verbatim if it
// could not, a refusal if the caller asked for something this model cannot
// answer. Safe on any exit path.
func (s *sttRemapWriter) finish() {
	if !s.buffering {
		return
	}
	s.buffering = false
	raw := s.body.Bytes()
	out, refusal, ok := remapTranscription(raw, s.req, s.model, s.streaming, s.join)
	h := s.Header()
	switch {
	case refusal != nil:
		body := sttErrorBody(refusal)
		h.Set("Content-Type", "application/json")
		h.Del("Content-Length")
		s.ResponseWriter.WriteHeader(refusal.status)
		_, _ = s.ResponseWriter.Write(body)
		s.refused = true
	case !ok:
		s.ResponseWriter.WriteHeader(s.code)
		_, _ = s.ResponseWriter.Write(raw)
	default:
		h.Set("Content-Type", "application/json")
		// The backend's Content-Length described the backend's body, which is
		// not what goes out.
		h.Del("Content-Length")
		s.ResponseWriter.WriteHeader(s.code)
		_, _ = s.ResponseWriter.Write(out)
		s.remapped = true
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// sttErrorBody renders a refusal in OpenAI's error shape, which is what a
// client is already parsing when a request is rejected.
func sttErrorBody(r *sttRefusal) []byte {
	type errObj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param,omitempty"`
		Code    string `json:"code,omitempty"`
	}
	b, err := json.Marshal(struct {
		Error errObj `json:"error"`
	}{errObj{Message: r.msg, Type: "invalid_request_error", Param: r.param, Code: "unsupported_value"}})
	if err != nil {
		return []byte(`{"error":{"message":"invalid request","type":"invalid_request_error"}}`)
	}
	return b
}
