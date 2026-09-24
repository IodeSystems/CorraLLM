package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// tinyWAV builds a real RIFF/WAVE file: 16-bit mono PCM at 16 kHz, a plain sine
// so ffmpeg has something to encode. Hand-built rather than a fixture on disk —
// the header is what every check here turns on, and a literal is easier to
// believe than a checked-in blob.
func tinyWAV(seconds int) []byte {
	const rate = 16000
	n := rate * seconds
	var pcm bytes.Buffer
	for i := 0; i < n; i++ {
		// A slow triangle; the content is irrelevant, only that it is not silence
		// (ffmpeg's encoders are free to be clever about digital silence).
		v := int16(((i % 200) - 100) * 200)
		_ = binary.Write(&pcm, binary.LittleEndian, v)
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+pcm.Len()))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))     // fmt chunk size
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))      // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))   // sample rate
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))      // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))     // bits
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(pcm.Len()))
	b.Write(pcm.Bytes())
	return b.Bytes()
}

// encodeWith runs ffmpeg to produce a real file in another container, so the
// tests below exercise the same decode path a client's upload would. Skips the
// test when ffmpeg is absent rather than asserting a fake.
func encodeWith(t *testing.T, wav []byte, ext string, args ...string) []byte {
	t.Helper()
	if !haveFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	out := filepath.Join(dir, "out."+ext)
	if err := os.WriteFile(in, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := append([]string{"-loglevel", "error", "-y", "-i", in}, args...)
	cmd = append(cmd, out)
	if b, err := exec.Command("ffmpeg", cmd...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v: %s", cmd, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sttUpload builds the multipart body an OpenAI audio client sends.
func sttUpload(t *testing.T, model, filename string, audio []byte, extra map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", model)
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// filePartOf reads back the file part of a multipart body: bytes + filename.
func filePartOf(t *testing.T, body []byte, contentType string) ([]byte, string) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatal("no file part")
		}
		if part.FormName() == "file" {
			b, _ := io.ReadAll(part)
			return b, part.FileName()
		}
	}
}

// noFFmpeg makes haveFFmpeg answer false for one test, restoring it after. The
// sync.Once is forced with a no-op so the real LookPath can never overwrite the
// answer mid-test.
func noFFmpeg(t *testing.T) {
	t.Helper()
	ffmpegOnce.Do(func() {})
	prev := ffmpegOK
	ffmpegOK = false
	t.Cleanup(func() { ffmpegOK = prev })
}

func TestCanonAudioFormat(t *testing.T) {
	for in, want := range map[string]string{
		"wav": "wav", "WAV": "wav", "audio/wav": "wav", "audio/x-wav": "wav", ".wav": "wav",
		"mp3": "mp3", "mpeg": "mp3", "mpga": "mp3", "audio/mpeg": "mp3",
		"m4a": "m4a", "mp4": "m4a", "aac": "m4a",
		"flac": "flac", "ogg": "ogg", "opus": "ogg", "webm": "webm", "aiff": "aiff",
		// Not a container corrallm knows: dropped, never guessed at, so a typo
		// cannot silently widen what a backend is believed to accept.
		"wvv": "", "": "", "audio/": "",
	} {
		if got := canonAudioFormat(in); got != want {
			t.Errorf("canonAudioFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSniffAudioFormat(t *testing.T) {
	wav := tinyWAV(1)
	if got := sniffAudioFormat(wav); got != "wav" {
		t.Errorf("wav sniffed as %q", got)
	}
	for name, hdr := range map[string]string{
		"flac": "fLaC\x00\x00\x00\x22",
		"ogg":  "OggS\x00\x02\x00\x00",
		"mp3":  "ID3\x04\x00\x00\x00",
	} {
		if got := sniffAudioFormat([]byte(hdr)); got != name {
			t.Errorf("%s header sniffed as %q", name, got)
		}
	}
	if got := sniffAudioFormat([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x01}); got != "webm" {
		t.Errorf("EBML sniffed as %q", got)
	}
	if got := sniffAudioFormat([]byte("\x00\x00\x00\x20ftypM4A ")); got != "m4a" {
		t.Errorf("ftyp sniffed as %q", got)
	}
	if got := sniffAudioFormat([]byte{0xFF, 0xFB, 0x90}); got != "mp3" {
		t.Errorf("bare MPEG sync sniffed as %q", got)
	}
	// A RIFF that is not WAVE (an AVI) must not pass as one — the thing
	// NeMo-Speech.cpp checks at audio_file.cpp:38 is both words, not the first.
	if got := sniffAudioFormat([]byte("RIFF....AVI LIST")); got != "" {
		t.Errorf("RIFF/AVI sniffed as %q, want unidentified", got)
	}
	if got := sniffAudioFormat([]byte("nope")); got != "" {
		t.Errorf("garbage sniffed as %q", got)
	}
}

// A backend that has declared nothing is a backend corrallm knows nothing
// about, and the layer must not touch its requests. This is what keeps oidio —
// which ingests every format a client sends — from paying for a re-encode.
func TestTranscodeSkippedWhenNothingDeclared(t *testing.T) {
	body, ct := sttUpload(t, "stt", "speech.mp3", []byte("ID3\x04junk"), nil)
	out, newCT, err := transcodeAudioUpload(context.Background(), body, ct, nil, config.DefaultConvert())
	if err != nil || out != nil || newCT != "" {
		t.Fatalf("declared nothing → out=%v ct=%q err=%v, want a no-op", out != nil, newCT, err)
	}
}

// Already in an accepted container → untouched, at the cost of one memcmp.
func TestTranscodeSkippedWhenFormatAccepted(t *testing.T) {
	body, ct := sttUpload(t, "stt", "speech.wav", tinyWAV(1), nil)
	out, _, err := transcodeAudioUpload(context.Background(), body, ct,
		[]string{"wav", "flac"}, config.DefaultConvert())
	if err != nil || out != nil {
		t.Fatalf("accepted format → out=%v err=%v, want a no-op", out != nil, err)
	}
}

// The layer's reason to exist: an mp3 the backend cannot read becomes a wav it
// can, with every other form field intact and the filename telling the truth.
func TestTranscodeMP3ToWAV(t *testing.T) {
	mp3 := encodeWith(t, tinyWAV(1), "mp3")
	if got := sniffAudioFormat(mp3); got != "mp3" {
		t.Fatalf("fixture is %q, not mp3", got)
	}
	body, ct := sttUpload(t, "nemo", "speech.mp3", mp3,
		map[string]string{"response_format": "verbose_json", "language": "en"})

	out, newCT, err := transcodeAudioUpload(context.Background(), body, ct,
		[]string{"wav"}, config.DefaultConvert())
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if out == nil {
		t.Fatal("transcode was a no-op on an unacceptable format")
	}
	if newCT == ct {
		t.Error("Content-Type unchanged; the rebuilt body has a new boundary")
	}
	got, filename := filePartOf(t, out, newCT)
	if f := sniffAudioFormat(got); f != "wav" {
		t.Errorf("file part is %q, want wav", f)
	}
	if filename != "speech.wav" {
		t.Errorf("filename = %q, want speech.wav (a backend may dispatch on it)", filename)
	}
	// Every other field survives: dropping response_format would silently change
	// the shape of the reply the caller asked for.
	form, err := multipart.NewReader(bytes.NewReader(out), boundaryOf(t, newCT)).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"model": "nemo", "response_format": "verbose_json", "language": "en"} {
		if len(form.Value[k]) != 1 || form.Value[k][0] != want {
			t.Errorf("field %q = %v, want %q", k, form.Value[k], want)
		}
	}
}

func TestTranscodeFromEveryClientFormat(t *testing.T) {
	wav := tinyWAV(1)
	for _, tc := range []struct {
		name string
		ext  string
		args []string
	}{
		{"m4a", "m4a", []string{"-c:a", "aac"}},
		{"flac", "flac", nil},
		{"webm", "webm", []string{"-c:a", "libopus"}},
		{"ogg", "ogg", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := encodeWith(t, wav, tc.ext, tc.args...)
			body, ct := sttUpload(t, "nemo", "speech."+tc.ext, src, nil)
			out, newCT, err := transcodeAudioUpload(context.Background(), body, ct,
				[]string{"wav"}, config.DefaultConvert())
			if err != nil {
				t.Fatalf("transcode %s: %v", tc.ext, err)
			}
			got, _ := filePartOf(t, out, newCT)
			if f := sniffAudioFormat(got); f != "wav" {
				t.Errorf("%s → %q, want wav", tc.ext, f)
			}
		})
	}
}

// ffmpeg missing is a configuration fault and says so. The alternative — pass
// the mp3 through — buys the caller a 400 from inside the backend about an
// encoding they did not choose, which is the outcome this layer exists to stop.
func TestTranscodeWithoutFFmpegRefuses(t *testing.T) {
	noFFmpeg(t)
	body, ct := sttUpload(t, "nemo", "speech.mp3", []byte("ID3\x04junk"), nil)
	_, _, err := transcodeAudioUpload(context.Background(), body, ct,
		[]string{"wav"}, config.DefaultConvert())
	var te *transcodeError
	if !asTranscodeError(err, &te) {
		t.Fatalf("err = %v, want a transcodeError", err)
	}
	if te.status != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", te.status)
	}
	for _, want := range []string{"mp3", "wav", "ffmpeg"} {
		if !bytes.Contains([]byte(te.msg), []byte(want)) {
			t.Errorf("message %q does not name %q", te.msg, want)
		}
	}
}

// Bytes ffmpeg cannot decode are the caller's problem and are reported as such,
// with what ffmpeg said, rather than forwarded for the backend to choke on.
func TestTranscodeUndecodableRefuses(t *testing.T) {
	if !haveFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	body, ct := sttUpload(t, "nemo", "speech.mp3", bytes.Repeat([]byte{0x01, 0x02}, 512), nil)
	_, _, err := transcodeAudioUpload(context.Background(), body, ct,
		[]string{"wav"}, config.DefaultConvert())
	var te *transcodeError
	if !asTranscodeError(err, &te) {
		t.Fatalf("err = %v, want a transcodeError", err)
	}
	if te.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", te.status)
	}
}

// Too long is refused with the number, before any CPU is spent on it — a
// 2-hour upload must not buy a minute of ffmpeg and then a body nothing
// downstream would take.
func TestTranscodeDurationCapRefuses(t *testing.T) {
	src := encodeWith(t, tinyWAV(3), "mp3")
	cfg := config.DefaultConvert()
	cfg.AudioMaxSeconds = 1
	body, ct := sttUpload(t, "nemo", "speech.mp3", src, nil)
	_, _, err := transcodeAudioUpload(context.Background(), body, ct, []string{"wav"}, cfg)
	var te *transcodeError
	if !asTranscodeError(err, &te) {
		t.Fatalf("err = %v, want a transcodeError", err)
	}
	if te.status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", te.status)
	}
	if !bytes.Contains([]byte(te.msg), []byte("1")) {
		t.Errorf("message %q does not carry the cap", te.msg)
	}
}

// The configured rate is honoured, because a backend that wants something other
// than 16 kHz is the only reason the knob exists.
func TestTranscodeHonoursSampleRate(t *testing.T) {
	src := encodeWith(t, tinyWAV(1), "mp3")
	cfg := config.DefaultConvert()
	cfg.AudioSampleRate = 8000
	body, ct := sttUpload(t, "nemo", "speech.mp3", src, nil)
	out, newCT, err := transcodeAudioUpload(context.Background(), body, ct, []string{"wav"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := filePartOf(t, out, newCT)
	if rate := wavSampleRate(got); rate != 8000 {
		t.Errorf("sample rate = %d, want 8000", rate)
	}
	if ch := wavChannels(got); ch != 1 {
		t.Errorf("channels = %d, want mono", ch)
	}
}

// End to end through the proxy: a client posts an mp3, the backend receives
// RIFF/WAVE, and the response says what the conversion cost so a bench does not
// read corrallm's ffmpeg as a slow model.
func TestProxyTranscodesUploadForRestrictedBackend(t *testing.T) {
	src := encodeWith(t, tinyWAV(1), "mp3")
	var gotFormat, gotFilename string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		f, hdr, err := formFile(r)
		if err != nil {
			t.Errorf("upstream file part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(f)
		gotFormat, gotFilename = sniffAudioFormat(b), hdr
		// What NeMo-Speech.cpp does at audio_file.cpp:38, reproduced: anything
		// that is not RIFF/WAVE is refused before a sample is read.
		if gotFormat != "wav" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"not a RIFF/WAVE file"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer upstream.Close()

	rec := postSTT(t, upstream.URL, []string{"wav"}, "speech.mp3", src)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotFormat != "wav" {
		t.Errorf("backend received %q, want wav", gotFormat)
	}
	if gotFilename != "speech.wav" {
		t.Errorf("backend saw filename %q, want speech.wav", gotFilename)
	}
	if rec.Header().Get(HeaderTranscodeMS) == "" {
		t.Errorf("%s missing; a transcode folded into nothing is a latency mystery", HeaderTranscodeMS)
	}
}

// The same backend without a declaration forwards the mp3 untouched — the
// capability is per backend, and silence means "not my business".
func TestProxyLeavesUploadAloneWhenNothingDeclared(t *testing.T) {
	src := []byte("ID3\x04not-really-an-mp3")
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		f, _, err := formFile(r)
		if err != nil {
			t.Errorf("upstream file part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer upstream.Close()

	rec := postSTT(t, upstream.URL, nil, "speech.mp3", src)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(got, src) {
		t.Errorf("backend received %q, want the original bytes unchanged", got)
	}
	if v := rec.Header().Get(HeaderTranscodeMS); v != "0" {
		t.Errorf("%s = %q on an untouched request, want 0", HeaderTranscodeMS, v)
	}
}

// A refusal reaches the CALLER as the status the layer chose, with a parseable
// JSON body — and the backend is never dialled, because the request was never
// servable.
func TestProxyRefusalReachesCaller(t *testing.T) {
	noFFmpeg(t)
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	rec := postSTT(t, upstream.URL, []string{"wav"}, "speech.mp3", []byte("ID3\x04junk"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rec.Code, rec.Body.String())
	}
	if reached {
		t.Error("the backend was dialled for a request corrallm had already refused")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"invalid_request_error"`)) {
		t.Errorf("body is not an OpenAI-shaped error: %s", rec.Body.String())
	}
}

// --- helpers -------------------------------------------------------------

func asTranscodeError(err error, out **transcodeError) bool {
	te, ok := err.(*transcodeError)
	if ok {
		*out = te
	}
	return ok
}

func boundaryOf(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	return params["boundary"]
}

func formFile(r *http.Request) (io.Reader, string, error) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, "", err
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, "", err
	}
	return f, hdr.Filename, nil
}

func wavSampleRate(b []byte) uint32 {
	if len(b) < 28 {
		return 0
	}
	return binary.LittleEndian.Uint32(b[24:28])
}

func wavChannels(b []byte) uint16 {
	if len(b) < 24 {
		return 0
	}
	return binary.LittleEndian.Uint16(b[22:24])
}

// postSTT runs one transcription request through a proxy whose single model
// declares `formats` (or does not), and returns the recorded response.
func postSTT(t *testing.T, upstreamURL string, formats []string, filename string, audio []byte) *httptest.ResponseRecorder {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := proc.NewManager(&config.Config{})
	t.Cleanup(mgr.Shutdown)

	cfg := mkConfig(t, "nemo", upstreamURL)
	m := cfg.Models["nemo"]
	m.Type = "stt"
	m.Modalities = map[string]config.ModalitySpec{
		config.ModalityAudio: {Formats: formats},
	}
	cfg.Models["nemo"] = m

	r := chi.NewRouter()
	px := New(cfg, mgr, sched.New(), st)
	px.SetConvert(true, config.DefaultConvert())
	px.Mount(r)

	body, ct := sttUpload(t, "nemo", filename, audio, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// --- NeMo-Speech.cpp intake conformance -----------------------------------
//
// THIS IS NOT AN INTEGRATION TEST, AND NOTHING HERE HAS EVER CONTACTED A NeMo
// BACKEND. NeMo-Speech.cpp is not built, installed or running on this box; its
// PR is unmerged. What follows is a CONFORMANCE test against its intake
// contract: the rules were read out of its source at one named commit and
// restated in Go below, each carrying the line it came from so a later reader
// can re-verify them instead of trusting this comment. When upstream changes
// the contract this file goes stale silently — the citations are the only
// defence against that, which is why every rule has one.
//
//	source: NVIDIA/NeMo-Speech.cpp @ 302ebc93f096d03395e5d86c643897b8bf0fd9a8
//	        (2026-09-21), read 2026-09-23
//
//	R1  the multipart field must literally be named "file"
//	      server/http/http_server.cpp:195-198 (uploaded_file)
//	R2  at least 12 bytes                       src/common/audio_file.cpp:36-37
//	R3  RIFF....WAVE magic — both words         src/common/audio_file.cpp:38-39
//	R4  no chunk may declare more than remains  src/common/audio_file.cpp:53-54
//	R5  a "fmt " chunk of >= 16 bytes, and a "data" chunk
//	      src/common/audio_file.cpp:55-57, 75-76
//	R6  channels is 1 or 2                      src/common/audio_file.cpp:77-78
//	R7  8000 <= sampleRate <= 96000             src/common/audio_file.cpp:79-80
//	      and again on the recognizer: src/asr/recognizer.cpp:351 →
//	      supported_input_sample_rate, src/common/audio_resampler.cpp:15-16,63-65
//	R8  (format 1 and 16 bits) or (format 3 and 32 bits), and NOTHING else —
//	    8-, 24- and 32-bit integer PCM and WAVE_FORMAT_EXTENSIBLE (0xFFFE) are
//	    all refused here                        src/common/audio_file.cpp:83-89
//	R9  blockAlign == bytesPerSample*channels, and dataSize divides by it
//	      src/common/audio_file.cpp:90-92
//	R10 at least one frame                      src/common/audio_file.cpp:110-111
//
// R1 is stated for completeness and is NOT asserted against corrallm here:
// replaceFilePart puts the audio back under the field name the client used, so
// a client that uploads under another name is passed through unchanged and
// would still be refused by R1. Named as a gap rather than fixed by guessing —
// see plan/p35-phase2-intake.md.

// nemoAccepts reports what NeMo-Speech.cpp's parse_wav would do with these
// bytes: nil when it would load them, an error naming the rule when it would
// throw. A transcription of the C++, kept in the same order as the C++ so the
// two can be read side by side.
func nemoAccepts(b []byte) error {
	if len(b) < 12 { // R2
		return fmt.Errorf("R2: %d bytes, too small to be a RIFF/WAVE file", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" { // R3
		return fmt.Errorf("R3: unsupported container (magic %q/%q)", b[0:4], b[8:12])
	}
	var format, channels, bits, blockAlign uint16
	var rate, dataSize uint32
	haveFormat, haveData := false, false
	for pos := 12; pos+8 <= len(b); {
		declared := binary.LittleEndian.Uint32(b[pos+4 : pos+8])
		body := pos + 8
		if int64(declared) > int64(len(b)-body) { // R4
			return fmt.Errorf("R4: truncated WAV chunk %q", b[pos:pos+4])
		}
		switch {
		case string(b[pos:pos+4]) == "fmt ":
			if declared < 16 { // R5
				return fmt.Errorf("R5: invalid WAV format chunk (%d bytes)", declared)
			}
			p := b[body:]
			format = binary.LittleEndian.Uint16(p[0:2])
			channels = binary.LittleEndian.Uint16(p[2:4])
			rate = binary.LittleEndian.Uint32(p[4:8])
			blockAlign = binary.LittleEndian.Uint16(p[12:14])
			bits = binary.LittleEndian.Uint16(p[14:16])
			haveFormat = true
		case string(b[pos:pos+4]) == "data" && !haveData:
			dataSize = declared
			haveData = true
		}
		next := int64(body) + int64(declared) + int64(declared&1)
		if next > int64(len(b)) {
			break
		}
		pos = int(next)
	}
	if !haveFormat || !haveData { // R5
		return fmt.Errorf("R5: WAV file is missing a format or data chunk")
	}
	if channels != 1 && channels != 2 { // R6
		return fmt.Errorf("R6: %d channels; only mono and stereo are supported", channels)
	}
	if rate < 8000 || rate > 96000 { // R7
		return fmt.Errorf("R7: sample rate %d must be between 8 and 96 kHz", rate)
	}
	var bytesPerSample uint16 // R8
	switch {
	case format == 1 && bits == 16:
		bytesPerSample = 2
	case format == 3 && bits == 32:
		bytesPerSample = 4
	default:
		return fmt.Errorf("R8: unsupported WAV encoding (format %d, %d-bit)", format, bits)
	}
	align := bytesPerSample * channels // R9
	if blockAlign != align || dataSize%uint32(align) != 0 {
		return fmt.Errorf("R9: invalid or truncated WAV sample data (blockAlign %d, want %d; %d bytes of data)",
			blockAlign, align, dataSize)
	}
	if dataSize/uint32(align) == 0 { // R10
		return fmt.Errorf("R10: audio data is empty")
	}
	return nil
}

// wavPCM builds a RIFF/WAVE with the fmt parameters asked for, so the intake
// rules can be exercised one at a time. The samples are a ramp rather than
// silence — some of these files are fed through ffmpeg for real, and encoders
// are allowed to be clever about digital silence.
func wavPCM(formatCode, channels, bits uint16, rate uint32, frames int) []byte {
	align := int(bits) / 8 * int(channels)
	data := make([]byte, frames*align)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, formatCode)
	_ = binary.Write(&b, binary.LittleEndian, channels)
	_ = binary.Write(&b, binary.LittleEndian, rate)
	_ = binary.Write(&b, binary.LittleEndian, rate*uint32(align)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(align))
	_ = binary.Write(&b, binary.LittleEndian, bits)
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// The checker must not be vacuous. Every rule corrallm can trip is exercised
// in both directions here, so a later "corrallm's output is accepted"
// assertion means something.
func TestNeMoIntakeSpecIsNotVacuous(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		rule string // "" = must be accepted
	}{
		{"16-bit mono 16 kHz", wavPCM(1, 1, 16, 16000, 100), ""},
		{"16-bit stereo 44.1 kHz", wavPCM(1, 2, 16, 44100, 100), ""},
		{"float32 mono 48 kHz", wavPCM(3, 1, 32, 48000, 100), ""},
		{"too small", []byte("RIFF"), "R2"},
		{"not RIFF/WAVE", []byte("RIFF....AVI LIST...."), "R3"},
		// The §2 gap, stated the way NeMo states it: the container is right and
		// the encoding inside it is not.
		{"24-bit stereo 48 kHz", wavPCM(1, 2, 24, 48000, 100), "R8"},
		{"8-bit mono", wavPCM(1, 1, 8, 16000, 100), "R8"},
		{"32-bit integer PCM", wavPCM(1, 1, 32, 16000, 100), "R8"},
		{"WAVE_FORMAT_EXTENSIBLE", wavPCM(0xFFFE, 1, 16, 16000, 100), "R8"},
		{"5.1 channels", wavPCM(1, 6, 16, 48000, 100), "R6"},
		{"192 kHz", wavPCM(1, 1, 16, 192000, 100), "R7"},
		{"4 kHz", wavPCM(1, 1, 16, 4000, 100), "R7"},
		{"no frames", wavPCM(1, 1, 16, 16000, 0), "R10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nemoAccepts(tc.in)
			switch {
			case tc.rule == "" && err != nil:
				t.Errorf("NeMo would refuse a file it accepts: %v", err)
			case tc.rule != "" && err == nil:
				t.Errorf("checker accepted what NeMo refuses at %s", tc.rule)
			case tc.rule != "" && err != nil && !strings.HasPrefix(err.Error(), tc.rule+":"):
				t.Errorf("refused as %v, want rule %s", err, tc.rule)
			}
		})
	}
}

// nemoIntake is what an operator would declare for NeMo-Speech.cpp today: the
// one container its reader opens. It is deliberately NOT enough — see the
// 24-bit case below, which is the gap phase 2 closes.
func nemoIntake() []string { return []string{"wav"} }

// What corrallm produces has to be loadable by the backend it produced it for.
// Sniffing "wav" is not that test: RIFF/WAVE is rule R3 of ten.
func TestTranscodeOutputSatisfiesNeMoIntake(t *testing.T) {
	wav := tinyWAV(1)
	for _, tc := range []struct {
		name string
		ext  string
		args []string
	}{
		{"mp3", "mp3", nil},
		{"m4a", "m4a", []string{"-c:a", "aac"}},
		{"flac", "flac", nil},
		{"webm", "webm", []string{"-c:a", "libopus"}},
		{"ogg", "ogg", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := encodeWith(t, wav, tc.ext, tc.args...)
			body, ct := sttUpload(t, "nemo", "speech."+tc.ext, src, nil)
			out, newCT, err := transcodeAudioUpload(context.Background(), body, ct,
				nemoIntake(), config.DefaultConvert())
			if err != nil {
				t.Fatalf("transcode %s: %v", tc.ext, err)
			}
			got, _ := filePartOf(t, out, newCT)
			if err := nemoAccepts(got); err != nil {
				t.Errorf("NeMo would refuse what corrallm produced from %s: %v", tc.ext, err)
			}
		})
	}
}

// THE GAP, AS A TEST. A 48 kHz stereo 24-bit wav satisfies `formats: [wav]`,
// so phase 1 forwards it untouched — and NeMo refuses it at R8, from inside a
// process the caller has never heard of. corrallm must settle this at its own
// door: either convert the file into something the backend loads, or refuse it
// by name. Passing it on is the one outcome this layer exists to remove.
func TestWAVOutsideNeMoPCMBoundsIsHandledAtTheDoor(t *testing.T) {
	if !haveFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	src := wavPCM(1, 2, 24, 48000, 16000)
	if err := nemoAccepts(src); err == nil {
		t.Fatal("fixture is not the problem case: NeMo would accept it")
	}
	body, ct := sttUpload(t, "nemo", "speech.wav", src, nil)
	out, newCT, err := transcodeAudioUpload(context.Background(), body, ct,
		nemoIntake(), config.DefaultConvert())
	if err != nil {
		// A refusal is an acceptable answer, as long as it is corrallm's own
		// and it names what is wrong.
		var te *transcodeError
		if !asTranscodeError(err, &te) {
			t.Fatalf("err = %v, want a transcodeError", err)
		}
		return
	}
	if out == nil {
		t.Fatal("forwarded untouched: the backend refuses this at audio_file.cpp:89, and " +
			"the caller reads somebody else's error about an encoding they never chose")
	}
	got, _ := filePartOf(t, out, newCT)
	if err := nemoAccepts(got); err != nil {
		t.Errorf("NeMo would still refuse what corrallm produced: %v", err)
	}
}
