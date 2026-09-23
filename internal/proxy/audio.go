package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// Transcode-in: making a client's audio readable by a backend that cannot read
// it (P35 phase 1).
//
// corrallm owns the OpenAI shape; a backend is whatever it is. On the STT
// surface those two facts collide at the container level. An OpenAI client
// uploads flac/mp3/mp4/mpeg/mpga/m4a/ogg/wav/webm — that list is the API's, not
// a suggestion — while NVIDIA's NeMo-Speech.cpp rejects everything that is not
// RIFF/WAVE before it looks at a single sample (`src/common/audio_file.cpp:38`),
// and then only PCM 16-bit or float 32-bit, mono or stereo, 8–96 kHz
// (`:89`). Point the official client at that backend with an .mp3 and it fails
// inside somebody else's program, with somebody else's message.
//
// So the transform is per-BACKEND, never blanket. oidio ingests every format
// listed above (verified on the wire, 2026-09-23: wav/mp3/m4a/flac/webm and a
// 48 kHz stereo 24-bit wav all returned 200 with the right text). Re-encoding
// for it would spend CPU to make a request no better, and would put a
// resampled file in front of a model that was perfectly happy with the
// original. corrallm therefore has to KNOW what a backend ingests before it
// touches the bytes, and the answer has to be able to be "everything".
//
// WHERE THAT KNOWLEDGE COMES FROM IS NOT A NEW CONCEPT. config.ModalitySpec
// already carries `formats` per accepted input modality — its own comment says
// "image/audio: accepted encodings" — it is already declared in config, already
// overridden by probe results through config.ProbedModalities, and already
// published on /v1/models (`"image":{"formats":["png","jpeg"]}` is live today).
// Audio format is the finer grain UNDER the `audio` modality that config.IO
// names coarsely, and it is the same field one level down. An empty list means
// no constraint is known, which is the honest reading of silence and is why
// this layer is invisible until an operator describes a picky backend.
//
// NOTHING HERE TOUCHES THE RESPONSE. Remapping a backend's reply into OpenAI's
// shape is the other half of P35 and is deliberately absent: each of its five
// known divergences carries a policy choice the owner owns, and guessing them
// is how a compatibility layer starts lying.

// Target encoding for a transcode. 16 kHz mono PCM s16 is what every ASR
// frontend in this class resamples to anyway — NeMo, whisper.cpp and parakeet
// all do it internally — so sending more is bytes the backend discards, and
// sending it as WAV is the one container a format-restricted STT backend is
// restricted TO. It also makes the size of a transcode arithmetic rather than
// a guess: 32,000 bytes per second of audio, regardless of what arrived.
const (
	defaultTranscodeSampleRate = 16000
	// defaultTranscodeMaxSeconds bounds one transcode. 30 minutes at the rate
	// above is 57.6 MB, which is just inside the 64 MiB cap handleInference
	// already imposes on an STT upload — so the bound is the one that already
	// exists, expressed in the units the refusal has to be phrased in. A
	// 2-hour m4a is a real request and this refuses it, out loud, in
	// milliseconds, rather than spending a minute of CPU to produce a 230 MB
	// body nothing downstream would accept.
	defaultTranscodeMaxSeconds = 1800
	// transcodeTimeout is a hard stop for a wedged ffmpeg, not a tuning knob.
	// ffmpeg decodes compressed speech faster than 100x realtime on this box,
	// so the 30-minute ceiling above is ~20 s of work; two minutes is an order
	// of magnitude of headroom. Deliberately NOT a flag: an operator has no
	// basis on which to set it, and the duration cap is the knob that actually
	// changes what a transcode costs.
	transcodeTimeout = 2 * time.Minute
)

// transcodeError is a refusal carrying the status the caller must return.
//
// A refusal, and not a silent pass-through. pdf.go leaves an unreadable PDF in
// the request because doing so still gets the caller an answer — the model
// reads the rest of the message and says it could not open the attachment. An
// STT request has no rest: passing an mp3 to a backend that cannot read one
// buys a 400 phrased by a foreign program about a file the caller never chose
// the encoding of. That is the worst of the three outcomes, so this layer
// diverges from its sibling and says what happened itself.
type transcodeError struct {
	status int
	msg    string
}

func (e *transcodeError) Error() string { return e.msg }

// audioUploadRoute reports whether this path carries a multipart audio upload
// that a backend has to be able to decode. /v1/audio/speech is excluded: it is
// JSON in and audio OUT, so nothing arrives that needs converting.
func audioUploadRoute(path string) bool {
	return path == "/v1/audio/transcriptions" || path == "/v1/audio/translations"
}

// audioFormatsFor is what the backend behind a served name ingests, as
// canonical container names. Empty = no constraint known → never transcode.
//
// Resolved against a lane's FIRST member, exactly as Config.ConvertFor does and
// for the same reason: the rewrite happens once, before the candidate walk, so
// the top-preference member's needs are the ones that can be honoured. The
// mismatch that leaves is real and bounded — see the note on the call site.
func (p *Proxy) audioFormatsFor(cfg *config.Config, served string) []string {
	name := served
	if lane, ok := cfg.Lanes[served]; ok && len(lane.Members) > 0 {
		name = lane.Members[0].Model
	}
	m, ok := cfg.Models[name]
	if !ok {
		return nil
	}
	spec, ok := m.EffectiveModalities(name, p.cost.IsAudioType(m.Type))[config.ModalityAudio]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(spec.Formats))
	for _, f := range spec.Formats {
		if c := canonAudioFormat(f); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// canonAudioFormat folds the spellings a client or an operator might use onto
// one name per CONTAINER — which is the granularity a backend's reader
// actually refuses at. The left-hand column covers OpenAI's own documented
// upload set (flac, mp3, mp4, mpeg, mpga, m4a, ogg, wav, webm), the MIME
// subtypes a browser sends, and the words an operator would reach for.
// Unrecognised spellings return "" and are dropped rather than guessed at: a
// typo in a declaration must not quietly widen what a backend is believed to
// accept.
func canonAudioFormat(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "audio/")
	s = strings.TrimPrefix(s, "video/")
	s = strings.TrimPrefix(s, "x-")
	s = strings.TrimPrefix(s, ".")
	switch s {
	case "wav", "wave", "vnd.wave", "pcm", "riff", "l16":
		return "wav"
	case "mp3", "mpeg", "mpga", "mp3a":
		return "mp3"
	case "m4a", "mp4", "aac", "m4b", "mp4a":
		return "m4a"
	case "flac":
		return "flac"
	case "ogg", "oga", "opus", "vorbis":
		return "ogg"
	case "webm", "matroska", "mkv":
		return "webm"
	case "aiff", "aif", "aifc":
		return "aiff"
	default:
		return ""
	}
}

// sniffAudioFormat names the container from the bytes, never from the
// filename. A client picks the filename and an OpenAI client picks it from
// whatever the user dragged in; the magic number is what the backend's own
// reader will look at, so it is what this has to agree with. "" means
// unidentified, which the caller treats as "not one of the declared names" —
// transcoding something that turns out to have been acceptable costs CPU,
// while forwarding something unidentified costs the caller their request.
func sniffAudioFormat(b []byte) string {
	switch {
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "wav"
	case len(b) >= 4 && string(b[0:4]) == "fLaC":
		return "flac"
	case len(b) >= 4 && string(b[0:4]) == "OggS":
		return "ogg"
	case len(b) >= 4 && b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3:
		// EBML — Matroska and WebM share it, and a WebM upload is the one an
		// OpenAI browser client sends. Which of the two it is does not change
		// the answer: ffmpeg reads both and neither is RIFF.
		return "webm"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		return "m4a"
	case len(b) >= 12 && string(b[0:4]) == "FORM" && (string(b[8:12]) == "AIFF" || string(b[8:12]) == "AIFC"):
		return "aiff"
	case len(b) >= 3 && string(b[0:3]) == "ID3":
		return "mp3"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "mp3" // bare MPEG frame sync, no ID3 tag
	default:
		return ""
	}
}

func acceptsFormat(accepts []string, format string) bool {
	for _, a := range accepts {
		if a == format {
			return true
		}
	}
	return false
}

// transcodeAudioUpload rewrites a multipart STT body so its file part is in a
// container the backend can read, returning the new body and the new
// Content-Type (the boundary changes).
//
// (nil, "", nil) means nothing needed doing, and is the answer on every path
// this layer has no business on: a body it cannot parse, a body with no file
// part, a backend that declared nothing, and a file already in an accepted
// format. Only a request that genuinely cannot be served as it stands produces
// a *transcodeError.
func transcodeAudioUpload(ctx context.Context, body []byte, contentType string, accepts []string, cfg config.ConvertConfig) ([]byte, string, error) {
	if len(accepts) == 0 {
		return nil, "", nil // nothing declared → nothing known → hands off
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, "", nil // not a multipart we can read; the existing 400 path owns this
	}
	name, filename, audio, ok := audioPartFrom(body, params["boundary"])
	if !ok {
		return nil, "", nil
	}
	have := sniffAudioFormat(audio)
	if have != "" && acceptsFormat(accepts, have) {
		return nil, "", nil // the common case, and it costs one memcmp
	}

	wav, err := transcodeToWAV(ctx, audio, have, accepts, cfg)
	if err != nil {
		return nil, "", err
	}
	out, newCT, ok := replaceFilePart(body, params["boundary"], name, wavName(filename), wav)
	if !ok {
		return nil, "", &transcodeError{status: 500, msg: "audio transcode: could not rebuild the upload"}
	}
	return out, newCT, nil
}

// transcodeToWAV shells out to ffmpeg for 16 kHz mono PCM s16 WAV.
func transcodeToWAV(ctx context.Context, audio []byte, have string, accepts []string, cfg config.ConvertConfig) ([]byte, error) {
	described := have
	if described == "" {
		described = "an unrecognised format"
	}
	if !haveFFmpeg() {
		// Named, not swallowed. The operator declared that this backend reads
		// only certain containers and then did not install the one tool that
		// could honour that, so the request is unservable for a configuration
		// reason — which is what 415 says and what a 400 from inside the
		// backend would not.
		return nil, &transcodeError{status: 415, msg: fmt.Sprintf(
			"audio is %s and this model accepts only %s; corrallm can convert it but ffmpeg is not installed on this host",
			described, strings.Join(accepts, ", "))}
	}

	rate := cfg.AudioSampleRate
	if rate <= 0 {
		rate = defaultTranscodeSampleRate
	}
	maxSec := cfg.AudioMaxSeconds
	if maxSec <= 0 {
		maxSec = defaultTranscodeMaxSeconds
	}
	// bytes(t) = t * rate * 2 (mono, s16). The cap is carried in bytes from
	// here down so the two enforcement points — the cheap pre-check and
	// ffmpeg's own -fs — are the same number rather than two that can drift.
	maxBytes := int64(maxSec) * int64(rate) * 2

	dir, err := os.MkdirTemp("", "corrallm-audio-")
	if err != nil {
		return nil, &transcodeError{status: 500, msg: "audio transcode: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "in")
	outPath := filepath.Join(dir, "out.wav")
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return nil, &transcodeError{status: 500, msg: "audio transcode: " + err.Error()}
	}

	// Refuse long audio BEFORE spending anything on it. ffprobe costs
	// milliseconds and answers for every container ffmpeg can open, so a
	// 2-hour upload is turned away immediately instead of after a minute of
	// work whose only possible product is a body too large to forward. A file
	// whose duration ffprobe will not state is not refused here — it is bounded
	// below by -fs on the output, which is the same limit measured on the
	// artifact instead of on the claim.
	if secs, ok := probeDurationSeconds(ctx, in); ok && secs > float64(maxSec) {
		return nil, &transcodeError{status: 413, msg: fmt.Sprintf(
			"audio is %.0fs long and corrallm converts at most %ds of audio per request; "+
				"send a shorter file, or split it", secs, maxSec)}
	}

	cctx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-nostdin", "-loglevel", "error", "-y",
		"-i", in,
		"-vn",           // drop cover art; an mp3's embedded JPEG is not audio
		"-map", "0:a:0", // first audio stream only — a webm may carry video
		"-ac", "1", // mono: every accepting backend takes it, and it halves the bytes
		"-ar", strconv.Itoa(rate),
		"-c:a", "pcm_s16le", // the one PCM encoding NeMo-Speech.cpp's format 1 branch reads
		"-f", "wav",
		"-fs", strconv.FormatInt(maxBytes+1, 10), // stop rather than fill the disk
		outPath)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, &transcodeError{status: 504, msg: fmt.Sprintf(
				"audio transcode timed out after %s", transcodeTimeout)}
		}
		return nil, &transcodeError{status: 400, msg: fmt.Sprintf(
			"audio is %s and could not be converted to wav: %s", described, tail(errb.String(), 300))}
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return nil, &transcodeError{status: 400, msg: fmt.Sprintf(
			"audio is %s and produced no output when converted to wav", described)}
	}
	if st.Size() > maxBytes {
		// -fs stopped it, so the source was longer than the cap and ffprobe
		// had not said so. Same refusal as the pre-check, reached the
		// expensive way, and truncated audio is never handed on as if whole.
		return nil, &transcodeError{status: 413, msg: fmt.Sprintf(
			"audio exceeds the %ds corrallm converts per request; send a shorter file, or split it", maxSec)}
	}
	return os.ReadFile(outPath)
}

// probeDurationSeconds asks ffprobe how long the file is. ok=false when
// ffprobe is absent, fails, or reports a duration the container did not carry
// (streamed webm, a truncated mp3) — all of which mean "unknown", never "zero".
func probeDurationSeconds(ctx context.Context, path string) (float64, bool) {
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, false
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return secs, true
}

// audioPartFrom finds the uploaded audio in a buffered multipart body: the
// "file" field OpenAI clients send, else the first part that carries a
// filename. Returns the field name so the rebuild puts it back under the name
// the client used.
func audioPartFrom(body []byte, boundary string) (field, filename string, data []byte, ok bool) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		fn := part.FileName()
		if fn == "" {
			_ = part.Close()
			continue
		}
		b, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			return "", "", nil, false
		}
		if part.FormName() == "file" {
			return "file", fn, b, true
		}
		if !ok { // remember the first file part in case "file" never appears
			field, filename, data, ok = part.FormName(), fn, b, true
		}
	}
	return field, filename, data, ok
}

// replaceFilePart rewrites the body with one file part's bytes swapped,
// preserving every other field and its order. Modelled on
// rewriteModelMultipart — same walk, same "rebuild rather than patch" — and it
// returns a new Content-Type because the writer mints a new boundary.
func replaceFilePart(body []byte, boundary, field, filename string, data []byte) ([]byte, string, bool) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	replaced := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", false
		}
		var w io.Writer
		if part.FormName() == field && part.FileName() != "" {
			// The declared content type goes with the bytes. A backend that
			// sniffs the part header rather than the payload would otherwise
			// be told the wav is still an mp3.
			h := make(map[string][]string)
			h["Content-Disposition"] = []string{fmt.Sprintf(
				`form-data; name=%q; filename=%q`, field, filename)}
			h["Content-Type"] = []string{"audio/wav"}
			w, err = mw.CreatePart(h)
			if err != nil {
				return nil, "", false
			}
			if _, err := w.Write(data); err != nil {
				return nil, "", false
			}
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, "", false
			}
			replaced = true
			continue
		}
		if fn := part.FileName(); fn != "" {
			w, err = mw.CreateFormFile(part.FormName(), fn)
		} else {
			w, err = mw.CreateFormField(part.FormName())
		}
		if err != nil {
			return nil, "", false
		}
		if _, err := io.Copy(w, part); err != nil {
			return nil, "", false
		}
	}
	if err := mw.Close(); err != nil || !replaced {
		return nil, "", false
	}
	return out.Bytes(), mw.FormDataContentType(), true
}

// wavName renames the upload so the extension matches its contents. Backends
// that dispatch on the extension rather than the bytes exist, and handing one
// a file still called speech.mp3 recreates the failure this layer removes.
func wavName(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" {
		base = "audio"
	}
	return base + ".wav"
}

// jsonString quotes a message for embedding in a hand-built JSON error body.
// An ffmpeg stderr tail carries quotes and newlines, and splicing it raw
// produces a body a client cannot parse — turning a clear refusal into a
// mystery at the exact moment the caller most needs to read it.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

var (
	ffmpegOnce sync.Once
	ffmpegOK   bool
)

// haveFFmpeg reports whether ffmpeg is on PATH (checked once, like
// haveTesseract).
func haveFFmpeg() bool {
	ffmpegOnce.Do(func() {
		_, err := exec.LookPath("ffmpeg")
		ffmpegOK = err == nil
	})
	return ffmpegOK
}
