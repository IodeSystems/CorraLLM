package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
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

// audioIntake is everything a backend has said about what it can ingest,
// resolved for one served name and canonicalised: the containers its reader
// opens, and — phase 2 — the PCM parameters it will accept inside them.
//
// One struct rather than a widening parameter list because the two levels are
// one question ("can this backend read this file") answered at two depths, and
// because a target has to satisfy BOTH at once: choosing wav to satisfy the
// container and then s16 to satisfy the encoding is one decision, not two.
type audioIntake struct {
	formats []string // canonical container names; empty = nothing declared
	bounds  config.ModalitySpec
}

// known reports whether the backend said anything at all. Nothing said =
// nothing known = hands off, which is the honest reading of silence and is
// what keeps this layer invisible for a backend nobody has described.
func (a audioIntake) known() bool { return len(a.formats) > 0 || a.bounds.AudioBounded() }

// audioFormatsFor is what the backend behind a served name ingests. An empty
// intake means no constraint is known → never transcode.
//
// (The name predates the PCM bounds it now also carries; it is kept because
// proxy.go names it, and renaming that call site belongs to whoever owns that
// file.)
//
// Resolved against a lane's FIRST member, exactly as Config.ConvertFor does and
// for the same reason: the rewrite happens once, before the candidate walk, so
// the top-preference member's needs are the ones that can be honoured. The
// mismatch that leaves is real and bounded — see the note on the call site.
//
// Resolution goes through EffectiveModalities, so probed BEATS declared here
// exactly as it does everywhere else. Phase 2 adds fields to the spec it
// returns; it does not add a second way of answering the question.
func (p *Proxy) audioFormatsFor(cfg *config.Config, served string) audioIntake {
	name := served
	if lane, ok := cfg.Lanes[served]; ok && len(lane.Members) > 0 {
		name = lane.Members[0].Model
	}
	m, ok := cfg.Models[name]
	if !ok {
		return audioIntake{}
	}
	spec, ok := m.EffectiveModalities(name, p.cost.IsAudioType(m.Type))[config.ModalityAudio]
	if !ok {
		return audioIntake{}
	}
	out := make([]string, 0, len(spec.Formats))
	for _, f := range spec.Formats {
		if c := canonAudioFormat(f); c != "" {
			out = append(out, c)
		}
	}
	return audioIntake{formats: out, bounds: spec}
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

// parseWAVParams reads the fmt chunk of a RIFF/WAVE: sample rate, channel
// count, and the sample encoding as one of config's canonical PCM names.
// ok=false means the header could not be read, which the caller must treat as
// "not verified" and never as "fine" — an unparseable header is exactly the
// kind of file a strict reader refuses.
//
// It walks the chunks rather than reading a fixed offset because a real wav
// from a real encoder carries LIST/fact/id3 chunks before `fmt `, and a
// fixed-offset read of one of those produces a confident wrong answer.
// WAVE_FORMAT_EXTENSIBLE (0xFFFE) deliberately yields "" rather than being
// unwrapped to its sub-format: a backend that names format 1 or 3 is checking
// exactly that field (NeMo-Speech.cpp audio_file.cpp:83-89), so reporting the
// sub-format would claim an acceptance the backend does not grant.
func parseWAVParams(b []byte) (rate, channels int, encoding string, ok bool) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, "", false
	}
	for pos := 12; pos+8 <= len(b); {
		size := int64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if string(b[pos:pos+4]) == "fmt " {
			if size < 16 || int64(len(b)-body) < 16 {
				return 0, 0, "", false
			}
			p := b[body:]
			format := binary.LittleEndian.Uint16(p[0:2])
			channels = int(binary.LittleEndian.Uint16(p[2:4]))
			rate = int(binary.LittleEndian.Uint32(p[4:8]))
			bits := int(binary.LittleEndian.Uint16(p[14:16]))
			return rate, channels, pcmNameFor(format, bits), true
		}
		next := int64(body) + size + (size & 1)
		if next <= int64(pos) || next > int64(len(b)) {
			break
		}
		pos = int(next)
	}
	return 0, 0, "", false
}

// canonicalWAVHeader rewrites a WAVE_FORMAT_EXTENSIBLE fmt chunk into the
// plain 16-byte PCMWAVEFORMAT its sub-format names, and returns the input
// untouched when there is nothing to do.
//
// THIS IS WHERE PRODUCE-STRICTLY COSTS SOMETHING (§9.0b). ffmpeg's wav muxer
// emits EXTENSIBLE — format tag 0xFFFE with the real encoding hidden in a
// sub-format GUID — for anything over 16 bits or over 48 kHz
// (libavformat/riffenc.c). Both are legal WAV and both are refused by a reader
// that checks the format tag, which NeMo-Speech.cpp does at
// audio_file.cpp:83-89. So corrallm would have honoured a backend's request
// for float32 by handing it a file that backend cannot open: a conversion
// that looks like generosity and lands as a 400 from inside somebody else's
// program, which is the exact failure this layer exists to remove.
//
// The rewrite is lossless. EXTENSIBLE's sub-format GUID begins with the very
// format tag PCMWAVEFORMAT would carry, and every other field is already in
// the same place; what is dropped is the valid-bits count and the channel
// mask, neither of which means anything for mono or stereo integer/float PCM.
// Declining to rewrite anything else — an unknown sub-format, a malformed
// chunk — is deliberate: the file goes on as it is rather than being
// rearranged on a guess.
func canonicalWAVHeader(b []byte) []byte {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return b
	}
	for pos := 12; pos+8 <= len(b); {
		size := int64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if string(b[pos:pos+4]) != "fmt " {
			next := int64(body) + size + (size & 1)
			if next <= int64(pos) || next > int64(len(b)) {
				return b
			}
			pos = int(next)
			continue
		}
		// The fmt chunk, at last. Anything but EXTENSIBLE is already canonical.
		if size < 40 || int64(len(b)-body) < 40 {
			return b
		}
		f := b[body:]
		if binary.LittleEndian.Uint16(f[0:2]) != 0xFFFE {
			return b
		}
		sub := binary.LittleEndian.Uint16(f[24:26]) // first field of the sub-format GUID
		if sub != 1 && sub != 3 {
			return b
		}
		next := int64(body) + size + (size & 1)
		if next > int64(len(b)) {
			return b
		}
		var out bytes.Buffer
		out.Grow(len(b))
		out.WriteString("RIFF")
		_ = binary.Write(&out, binary.LittleEndian, uint32(0)) // patched below
		out.WriteString("WAVE")
		out.Write(b[12:pos]) // whatever chunks preceded fmt, in place
		// The 16-byte form: the sub-format's tag, then the fields EXTENSIBLE
		// already carries, in the same order and at the same offsets.
		out.WriteString("fmt ")
		_ = binary.Write(&out, binary.LittleEndian, uint32(16))
		_ = binary.Write(&out, binary.LittleEndian, sub)
		out.Write(f[2:16]) // channels, rate, byte rate, block align, bits
		out.Write(b[next:])
		res := out.Bytes()
		binary.LittleEndian.PutUint32(res[4:8], uint32(len(res)-8))
		return res
	}
	return b
}

// pcmNameFor maps a WAV format tag and bit depth onto config's PCM vocabulary.
// "" = a combination corrallm has no name for, which is never treated as
// accepted.
func pcmNameFor(format uint16, bits int) string {
	switch {
	case format == 1 && bits == 8:
		return "u8"
	case format == 1 && bits == 16:
		return "s16"
	case format == 1 && bits == 24:
		return "s24"
	case format == 1 && bits == 32:
		return "s32"
	case format == 3 && bits == 32:
		return "f32"
	case format == 3 && bits == 64:
		return "f64"
	default:
		return ""
	}
}

// satisfiedBy reports whether the file can go to this backend untouched.
//
// Two levels, and the second is what phase 2 added. The container is checked
// from the magic number as before; then, where the backend has declared what
// it wants INSIDE that container, the wav header is read and compared. A
// 48 kHz stereo 24-bit wav passes the first check and fails the second, which
// is the whole point: before this, corrallm forwarded it and the caller was
// told about it by the backend.
func (a audioIntake) satisfiedBy(have string, audio []byte) bool {
	if len(a.formats) > 0 && (have == "" || !acceptsFormat(a.formats, have)) {
		return false
	}
	if !a.bounds.AudioBounded() {
		return true // nothing declared below the container → phase 1, unchanged
	}
	switch have {
	case "wav":
		rate, channels, encoding, ok := parseWAVParams(audio)
		return ok && a.allowsPCM(rate, channels, encoding)
	case "":
		// Unidentified bytes, and the backend has said something specific it
		// needs. Nothing here can confirm it, so convert rather than forward on
		// a hope.
		return false
	default:
		// A compressed container states its PCM parameters only after a decode,
		// and every backend that has ever declared bounds accepts only wav
		// anyway. Passing it through is phase 1's behaviour, unchanged; the
		// case is named in plan/p35-phase2-intake.md rather than guessed at.
		return true
	}
}

// allowsPCM checks one file's parameters against the declared bounds. Every
// bound is zero-means-silent, so a backend that declared two of the four is
// held to two.
func (a audioIntake) allowsPCM(rate, channels int, encoding string) bool {
	b := a.bounds
	if b.MinSampleRate > 0 && rate < b.MinSampleRate {
		return false
	}
	if b.MaxSampleRate > 0 && rate > b.MaxSampleRate {
		return false
	}
	if b.MaxChannels > 0 && channels > b.MaxChannels {
		return false
	}
	if len(b.PCM) > 0 {
		if encoding == "" {
			return false
		}
		for _, p := range b.PCM {
			if config.CanonPCMEncoding(p) == encoding {
				return true
			}
		}
		return false
	}
	return true
}

// describe renders the declaration in the words a refusal has to use. The
// container list alone stopped being the whole answer in phase 2, and a 415
// that names half of what the backend asked for sends the operator looking in
// the wrong place.
func (a audioIntake) describe() []string {
	out := append([]string(nil), a.formats...)
	b := a.bounds
	if len(b.PCM) > 0 {
		out = append(out, strings.Join(b.PCM, "/")+" samples")
	}
	switch {
	case b.MinSampleRate > 0 && b.MaxSampleRate > 0:
		out = append(out, fmt.Sprintf("%d-%d Hz", b.MinSampleRate, b.MaxSampleRate))
	case b.MinSampleRate > 0:
		out = append(out, fmt.Sprintf("%d Hz and up", b.MinSampleRate))
	case b.MaxSampleRate > 0:
		out = append(out, fmt.Sprintf("up to %d Hz", b.MaxSampleRate))
	}
	if b.MaxChannels > 0 {
		out = append(out, fmt.Sprintf("up to %d channels", b.MaxChannels))
	}
	return out
}

// transcodeTarget is the one file corrallm will produce for this backend.
type transcodeTarget struct {
	rate     int
	channels int
	pcm      string // canonical name, for the log line and the refusal
	codec    string // the ffmpeg -c:a that produces it
}

// bytesPerSecond is the size arithmetic the duration cap is expressed in.
func (t transcodeTarget) bytesPerSecond() int64 {
	return int64(t.rate) * int64(t.channels) * int64(pcmWidth(t.pcm))
}

func pcmWidth(pcm string) int {
	switch pcm {
	case "u8":
		return 1
	case "s16":
		return 2
	case "s24":
		return 3
	case "s32", "f32":
		return 4
	case "f64":
		return 8
	}
	return 2
}

// chooseTarget picks the file corrallm will make, so that it satisfies
// everything the backend declared.
//
// §9.0b, applied where it actually costs something: ACCEPT GENEROUSLY means
// that anything corrallm can convert INTO the declared bounds it converts,
// rather than refusing because its favourite target happens not to fit. So a
// backend that wants 44.1 kHz gets 44.1 kHz and a backend that wants float32
// gets float32 — the preferred 16 kHz mono s16 is a default, not a demand.
// Only a backend corrallm genuinely cannot produce a file for is refused, and
// then at the door and by name.
func (a audioIntake) chooseTarget(have string, cfg config.ConvertConfig) (transcodeTarget, error) {
	described := have
	if described == "" {
		described = "an unrecognised format"
	}
	// Container first: corrallm makes wav and nothing else. A backend that
	// reads only flac was previously handed a wav it could not open — a
	// conversion that could never have worked, discovered by the caller as a
	// 400 from inside the backend.
	if len(a.formats) > 0 && !acceptsFormat(a.formats, "wav") {
		return transcodeTarget{}, &transcodeError{status: 415, msg: fmt.Sprintf(
			"audio is %s and this model accepts only %s; corrallm converts audio with ffmpeg to wav, "+
				"which this model does not accept", described, strings.Join(a.formats, ", "))}
	}

	t := transcodeTarget{channels: 1, pcm: "s16"}
	t.rate = cfg.AudioSampleRate
	if t.rate <= 0 {
		t.rate = defaultTranscodeSampleRate
	}
	b := a.bounds
	if b.MinSampleRate > 0 && t.rate < b.MinSampleRate {
		t.rate = b.MinSampleRate
	}
	if b.MaxSampleRate > 0 && t.rate > b.MaxSampleRate {
		t.rate = b.MaxSampleRate
	}
	if b.MinSampleRate > 0 && t.rate < b.MinSampleRate {
		// Only reachable from a min above a max, which Validate rejects in
		// config and a probe record could still carry. An empty window accepts
		// nothing, and saying so beats producing a file that cannot satisfy it.
		return transcodeTarget{}, &transcodeError{status: 415, msg: fmt.Sprintf(
			"audio is %s and this model accepts only %d-%d Hz, a range that contains no sample rate; "+
				"corrallm cannot convert into it", described, b.MinSampleRate, b.MaxSampleRate)}
	}

	if len(b.PCM) > 0 && !a.allowsPCM(t.rate, t.channels, t.pcm) {
		t.pcm = ""
		for _, p := range b.PCM {
			c := config.CanonPCMEncoding(p)
			if c != "" && config.FFmpegPCMCodec(c) != "" {
				t.pcm = c
				break
			}
		}
		if t.pcm == "" {
			return transcodeTarget{}, &transcodeError{status: 415, msg: fmt.Sprintf(
				"audio is %s and this model accepts only %s sample encodings; corrallm converts audio with "+
					"ffmpeg and can produce %s, none of which this model accepts",
				described, strings.Join(b.PCM, ", "), strings.Join(config.KnownPCMEncodings(), ", "))}
		}
	}
	t.codec = config.FFmpegPCMCodec(t.pcm)
	if t.codec == "" {
		return transcodeTarget{}, &transcodeError{status: 500, msg: "audio transcode: no encoder for " + t.pcm}
	}
	return t, nil
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
func transcodeAudioUpload(ctx context.Context, body []byte, contentType string, accepts audioIntake, cfg config.ConvertConfig) ([]byte, string, error) {
	if !accepts.known() {
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
	if accepts.satisfiedBy(have, audio) {
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

// transcodeToWAV shells out to ffmpeg for the target chosen above — 16 kHz
// mono PCM s16 WAV unless the backend declared bounds that ask for something
// else.
func transcodeToWAV(ctx context.Context, audio []byte, have string, accepts audioIntake, cfg config.ConvertConfig) ([]byte, error) {
	described := have
	if described == "" {
		described = "an unrecognised format"
	}
	// What corrallm would make, BEFORE asking whether it can. A backend no
	// conversion could ever satisfy is refused for that reason and not for the
	// missing tool, which would be true but not the fault.
	target, err := accepts.chooseTarget(have, cfg)
	if err != nil {
		return nil, err
	}
	if !haveFFmpeg() {
		// Named, not swallowed. The operator declared that this backend reads
		// only certain containers and then did not install the one tool that
		// could honour that, so the request is unservable for a configuration
		// reason — which is what 415 says and what a 400 from inside the
		// backend would not.
		return nil, &transcodeError{status: 415, msg: fmt.Sprintf(
			"audio is %s and this model accepts only %s; corrallm can convert it but ffmpeg is not installed on this host",
			described, strings.Join(accepts.describe(), ", "))}
	}

	maxSec := cfg.AudioMaxSeconds
	if maxSec <= 0 {
		maxSec = defaultTranscodeMaxSeconds
	}
	// bytes(t) = t * rate * channels * sampleWidth. The cap is carried in
	// bytes from here down so the two enforcement points — the cheap
	// pre-check and ffmpeg's own -fs — are the same number rather than two
	// that can drift. It follows the TARGET: a backend that asked for 44.1 kHz
	// float32 produces 5.5x the bytes of the default, and a cap computed off
	// the default would let the 64 MiB body limit be crossed silently.
	maxBytes := int64(maxSec) * target.bytesPerSecond()

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
		"-ac", strconv.Itoa(target.channels), // mono: every accepting backend takes it, and it halves the bytes
		"-ar", strconv.Itoa(target.rate),
		"-c:a", target.codec, // s16 by default: what NeMo-Speech.cpp's format 1 branch reads
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
	wav, err := os.ReadFile(outPath)
	if err != nil {
		return nil, err
	}
	// What corrallm SAYS it produced has to be what it produced — see
	// canonicalWAVHeader for the one place ffmpeg and a strict reader disagree.
	// A no-op for the 16 kHz s16 default, which is every request today.
	return canonicalWAVHeader(wav), nil
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
