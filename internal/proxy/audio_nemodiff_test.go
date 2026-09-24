package proxy

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the differential half of the NeMo intake conformance test.
//
// `nemoAccepts` in audio_test.go is a TRANSCRIPTION of NeMo-Speech.cpp's
// parse_wav, written by reading the C++. A transcription checked only against
// its own author's reading proves nothing — it can only confirm what the author
// already believed. This file checks it against the real thing: it compiles
// NVIDIA/NeMo-Speech.cpp's own src/common/audio_file.cpp into a small harness
// and compares verdicts over the same bytes.
//
// What this does and does not prove: audio_file.cpp is the parser the HTTP
// intake actually uses — server/http/http_server.cpp calls
// audio::load_wav_memory on the uploaded body directly, with no decoder in
// front of it. So this tests the INTAKE. It does not run recognition, loads no
// model, and links no ggml or CUDA. No NeMo model has ever run here.
//
// The NeMo source is not vendored and is not a build dependency of corrallm.
// When it is absent these tests skip. See testdata/nemoconformance/README.md
// for where to get it and which commit the rules were read at.

// nemoSourceDir returns the NeMo-Speech.cpp checkout to build against, and
// whether one was found. Override with NEMO_SPEECH_SRC.
func nemoSourceDir(t *testing.T) (string, bool) {
	t.Helper()
	// An explicitly set NEMO_SPEECH_SRC is authoritative: pointing it somewhere
	// wrong must skip, not quietly test a different checkout than the one named.
	candidates := []string{"/tmp/nemo-scratch/NeMo-Speech.cpp"}
	if env := os.Getenv("NEMO_SPEECH_SRC"); env != "" {
		candidates = []string{env}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "src", "common", "audio_file.cpp")); err == nil {
			return dir, true
		}
	}
	return "", false
}

// buildNeMoHarness compiles harness.cpp against NeMo's real audio_file.cpp and
// returns the binary's path. It skips the test when the source or a C++20
// compiler is missing, so a clean checkout still passes.
func buildNeMoHarness(t *testing.T) string {
	t.Helper()
	src, ok := nemoSourceDir(t)
	if !ok {
		t.Skip("NeMo-Speech.cpp source not found; set NEMO_SPEECH_SRC " +
			"(see testdata/nemoconformance/README.md)")
	}
	compiler := os.Getenv("CXX")
	if compiler == "" {
		compiler = "g++"
	}
	if _, err := exec.LookPath(compiler); err != nil {
		t.Skipf("no C++ compiler (%s) to build the NeMo intake harness", compiler)
	}
	binary := filepath.Join(t.TempDir(), "nemo_wav")
	cmd := exec.Command(compiler, "-std=c++20", "-O1",
		"-I"+filepath.Join(src, "src", "common"), "-I"+filepath.Join(src, "src"),
		filepath.Join("testdata", "nemoconformance", "harness.cpp"),
		filepath.Join(src, "src", "common", "audio_file.cpp"),
		"-o", binary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the NeMo intake harness: %v\n%s", err, out)
	}
	return binary
}

// nemoVerdict is what the real parser did with one input.
type nemoVerdict struct {
	accepted bool
	reason   string // NeMo's own message, when refused
	rule     string // that message mapped back to a rule number
	rate     string
	channels string
	frames   string
}

// nemoRuleOf maps one of parse_wav's messages back to the rule number the Go
// spec labels it with, so the two can be compared on WHICH rule fired and not
// only on accept/reject. A verdict that agrees for the wrong reason is still a
// misread rule.
//
// Every message here is a literal from src/common/audio_file.cpp. An unknown
// message means upstream changed its text or added a check: the caller treats
// that as a failure rather than quietly scoring it as agreement.
func nemoRuleOf(message string) string {
	switch {
	case strings.HasPrefix(message, "file is too small"):
		return "R2"
	case strings.HasPrefix(message, "unsupported container"):
		return "R3"
	case strings.HasPrefix(message, "truncated WAV chunk"):
		return "R4"
	case strings.HasPrefix(message, "invalid WAV format chunk"),
		strings.HasPrefix(message, "WAV file is missing a format or data chunk"):
		return "R5"
	case strings.HasPrefix(message, "only mono and stereo"):
		return "R6"
	case strings.HasPrefix(message, "sample rate must be between"):
		return "R7"
	case strings.HasPrefix(message, "unsupported WAV encoding"):
		return "R8"
	case strings.HasPrefix(message, "invalid or truncated WAV sample data"):
		return "R9"
	case strings.HasPrefix(message, "audio data is empty"):
		return "R10"
	}
	return ""
}

// goRuleOf pulls the rule label off a nemoAccepts error, which formats every
// refusal as "R<n>: ...".
func goRuleOf(err error) string {
	if err == nil {
		return ""
	}
	rule, _, found := strings.Cut(err.Error(), ":")
	if !found {
		return ""
	}
	return rule
}

// runNeMo feeds each input to the real parser and returns the verdicts in the
// same order. Inputs are batched so a large sweep cannot overrun ARG_MAX.
func runNeMo(t *testing.T, binary string, inputs [][]byte) []nemoVerdict {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(inputs))
	for i, in := range inputs {
		paths[i] = filepath.Join(dir, "v"+strconv.Itoa(i)+".wav")
		if err := os.WriteFile(paths[i], in, 0o600); err != nil {
			t.Fatalf("write vector %d: %v", i, err)
		}
	}
	const batch = 500
	verdicts := make([]nemoVerdict, 0, len(inputs))
	for start := 0; start < len(paths); start += batch {
		end := min(start+batch, len(paths))
		out, err := exec.Command(binary, paths[start:end]...).Output()
		if err != nil {
			t.Fatalf("run NeMo harness: %v", err)
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) != end-start {
			t.Fatalf("harness reported %d verdicts for %d inputs", len(lines), end-start)
		}
		for i, line := range lines {
			fields := strings.Split(line, "\t")
			if len(fields) < 2 {
				t.Fatalf("unparsable harness line %d: %q", start+i, line)
			}
			switch fields[1] {
			case "ACCEPT":
				if len(fields) != 5 {
					t.Fatalf("unparsable ACCEPT line %d: %q", start+i, line)
				}
				verdicts = append(verdicts, nemoVerdict{
					accepted: true,
					rate:     strings.TrimPrefix(fields[2], "rate="),
					channels: strings.TrimPrefix(fields[3], "channels="),
					frames:   strings.TrimPrefix(fields[4], "frames="),
				})
			case "REJECT":
				rule := nemoRuleOf(fields[2])
				if rule == "" {
					t.Errorf("parse_wav refused vector %d with a message no rule covers: %q "+
						"— upstream changed the contract", start+i, fields[2])
				}
				verdicts = append(verdicts, nemoVerdict{reason: fields[2], rule: rule})
			default:
				t.Fatalf("harness could not read vector %d: %q", start+i, line)
			}
		}
	}
	return verdicts
}

// ---------------------------------------------------------------------------
// Vectors the Go spec asserts a rule for but nothing had ever exercised.
// ---------------------------------------------------------------------------

// wavChunks assembles a RIFF/WAVE from raw chunks, so a vector can carry
// padding, ordering and sizes that wavPCM's canonical layout cannot express.
// riffSize is written verbatim — no parser here checks it, and saying so is the
// point of being able to set it wrong.
func wavChunks(riffSize uint32, chunks ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, riffSize)
	b.WriteString("WAVE")
	for _, c := range chunks {
		b.Write(c)
	}
	return b.Bytes()
}

// chunk builds one chunk with the declared size taken from the body, plus the
// pad byte RIFF requires after an odd body.
func chunk(id string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	if len(body)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// rawChunk builds a chunk whose DECLARED size is stated independently of the
// body actually present, which is how a truncated chunk is made.
func rawChunk(id string, declared uint32, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	_ = binary.Write(&b, binary.LittleEndian, declared)
	b.Write(body)
	return b.Bytes()
}

// fmtBody is a PCM fmt chunk body of exactly 16 bytes.
func fmtBody(format, channels, bits uint16, rate uint32) []byte {
	align := uint16(bits/8) * channels
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, format)
	_ = binary.Write(&b, binary.LittleEndian, channels)
	_ = binary.Write(&b, binary.LittleEndian, rate)
	_ = binary.Write(&b, binary.LittleEndian, rate*uint32(align))
	_ = binary.Write(&b, binary.LittleEndian, align)
	_ = binary.Write(&b, binary.LittleEndian, bits)
	return b.Bytes()
}

// fmtExtensible is the 40-byte WAVE_FORMAT_EXTENSIBLE fmt body ffmpeg emits for
// anything over 16 bits or over 48 kHz, sub-format GUID and all. The first two
// bytes of the GUID are the format tag the file would have had.
func fmtExtensible(subFormat, channels, bits uint16, rate uint32) []byte {
	b := bytes.NewBuffer(fmtBody(0xFFFE, channels, bits, rate))
	_ = binary.Write(b, binary.LittleEndian, uint16(22)) // cbSize
	_ = binary.Write(b, binary.LittleEndian, bits)       // valid bits
	_ = binary.Write(b, binary.LittleEndian, uint32(0))  // channel mask
	_ = binary.Write(b, binary.LittleEndian, subFormat)  // GUID: format tag
	b.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00,
		0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}) // GUID: the fixed KSDATAFORMAT tail
	return b.Bytes()
}

func ramp(n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = byte(i * 7)
	}
	return d
}

// nemoDiffVectors is every input both parsers are run over: the thirteen the Go
// test already built, plus the cases the spec states a rule for and nothing had
// exercised.
func nemoDiffVectors() []struct {
	name string
	in   []byte
} {
	type v = struct {
		name string
		in   []byte
	}
	return []v{
		// The thirteen from TestNeMoIntakeSpecIsNotVacuous, verbatim.
		{"16-bit mono 16 kHz", wavPCM(1, 1, 16, 16000, 100)},
		{"16-bit stereo 44.1 kHz", wavPCM(1, 2, 16, 44100, 100)},
		{"float32 mono 48 kHz", wavPCM(3, 1, 32, 48000, 100)},
		{"too small", []byte("RIFF")},
		{"not RIFF/WAVE", []byte("RIFF....AVI LIST....")},
		{"24-bit stereo 48 kHz", wavPCM(1, 2, 24, 48000, 100)},
		{"8-bit mono", wavPCM(1, 1, 8, 16000, 100)},
		{"32-bit integer PCM", wavPCM(1, 1, 32, 16000, 100)},
		{"WAVE_FORMAT_EXTENSIBLE (16-byte fmt)", wavPCM(0xFFFE, 1, 16, 16000, 100)},
		{"5.1 channels", wavPCM(1, 6, 16, 48000, 100)},
		{"192 kHz", wavPCM(1, 1, 16, 192000, 100)},
		{"4 kHz", wavPCM(1, 1, 16, 4000, 100)},
		{"no frames", wavPCM(1, 1, 16, 16000, 0)},

		// Never exercised before this file.
		{"EXTENSIBLE 40-byte fmt, s16 sub-format",
			wavChunks(200, chunk("fmt ", fmtExtensible(1, 1, 16, 16000)), chunk("data", ramp(200)))},
		{"EXTENSIBLE 40-byte fmt, f32 sub-format",
			wavChunks(200, chunk("fmt ", fmtExtensible(3, 1, 32, 96000)), chunk("data", ramp(200)))},
		{"fmt 18 bytes (cbSize=0), plain PCM",
			wavChunks(200, chunk("fmt ", append(fmtBody(1, 1, 16, 16000), 0, 0)), chunk("data", ramp(200)))},
		{"fmt 40 bytes but plain format tag 1",
			wavChunks(200, chunk("fmt ", append(fmtBody(1, 1, 16, 16000), make([]byte, 24)...)), chunk("data", ramp(200)))},
		{"fmt 15 bytes, one short of the minimum",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)[:15]), chunk("data", ramp(200)))},
		{"LIST chunk before fmt",
			wavChunks(200, chunk("LIST", []byte("INFOISFTLavf")), chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("data", ramp(200)))},
		{"data chunk before fmt",
			wavChunks(200, chunk("data", ramp(200)), chunk("fmt ", fmtBody(1, 1, 16, 16000)))},
		{"odd-length chunk, padded correctly",
			wavChunks(200, chunk("junk", ramp(3)), chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("data", ramp(200)))},
		{"odd-length chunk, pad byte MISSING",
			wavChunks(200, rawChunk("junk", 3, ramp(3)), chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("data", ramp(200)))},
		{"odd-length data chunk",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("data", ramp(201)))},
		{"data declares more than remains",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), rawChunk("data", 4096, ramp(200)))},
		{"data declares 0xFFFFFFFF",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), rawChunk("data", 0xFFFFFFFF, ramp(200)))},
		{"data chunk header runs off the end",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), []byte("dat"))},
		{"empty data chunk at EOF",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), rawChunk("data", 0, nil))},
		{"two data chunks, first one wins",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("data", ramp(200)), chunk("data", ramp(400)))},
		{"two fmt chunks, last one wins",
			wavChunks(200, chunk("fmt ", fmtBody(1, 1, 16, 16000)), chunk("fmt ", fmtBody(1, 2, 16, 44100)), chunk("data", ramp(200)))},
		{"no chunks at all",
			wavChunks(4)},
		{"exactly 12 bytes",
			[]byte("RIFF\x00\x00\x00\x00WAVE")},
		{"blockAlign disagrees with channels",
			wavChunks(200, chunk("fmt ", func() []byte { b := fmtBody(1, 2, 16, 16000); b[12] = 2; return b }()), chunk("data", ramp(200)))},
		{"dataSize not a multiple of blockAlign",
			wavChunks(200, chunk("fmt ", fmtBody(1, 2, 16, 16000)), chunk("data", ramp(201)))},
		{"8 kHz, the lower bound exactly", wavPCM(1, 1, 16, 8000, 100)},
		{"7999 Hz, one below the bound", wavPCM(1, 1, 16, 7999, 100)},
		{"96 kHz, the upper bound exactly", wavPCM(1, 1, 16, 96000, 100)},
		{"96001 Hz, one above the bound", wavPCM(1, 1, 16, 96001, 100)},
		{"zero channels", wavPCM(1, 0, 16, 16000, 100)},
		{"format 3 at 16 bits", wavPCM(3, 1, 16, 16000, 100)},
		{"format 1 at 32 bits", wavPCM(1, 1, 32, 16000, 100)},
		{"format 0", wavPCM(0, 1, 16, 16000, 100)},
	}
}

// TestNeMoIntakeSpecMatchesRealParser is the point of this file. Every
// disagreement is a bug in the Go spec, a stale citation, or a misread rule —
// each of which is worth more than the test passing.
func TestNeMoIntakeSpecMatchesRealParser(t *testing.T) {
	binary := buildNeMoHarness(t)
	vectors := nemoDiffVectors()
	inputs := make([][]byte, len(vectors))
	for i, v := range vectors {
		inputs[i] = v.in
	}
	real := runNeMo(t, binary, inputs)

	agreed := 0
	for i, v := range vectors {
		got := nemoAccepts(v.in)
		want := real[i]
		switch {
		case want.accepted && got != nil:
			t.Errorf("%s: the real parser ACCEPTS (rate=%s channels=%s frames=%s), "+
				"the Go spec refuses it at %v", v.name, want.rate, want.channels, want.frames, got)
		case !want.accepted && got == nil:
			t.Errorf("%s: the real parser REFUSES (%q), the Go spec accepts it",
				v.name, want.reason)
		case !want.accepted && goRuleOf(got) != want.rule:
			// Same verdict, different rule: the Go spec refuses this file for a
			// reason the real parser never reached.
			t.Errorf("%s: both refuse, but the real parser stops at %s (%q) and the "+
				"Go spec at %s", v.name, want.rule, want.reason, goRuleOf(got))
		default:
			agreed++
		}
	}
	t.Logf("verdicts and rules agreed on %d of %d vectors", agreed, len(vectors))
}

// TestNeMoIntakeSpecMatchesRealParserOnMutations sweeps mutated headers at the
// two parsers. Hand-written vectors only cover the cases someone thought of;
// this covers the ones nobody did, which is where a transcription drifts.
func TestNeMoIntakeSpecMatchesRealParserOnMutations(t *testing.T) {
	binary := buildNeMoHarness(t)

	// A fixed seed, so a failure names a vector that can be reproduced.
	rng := rand.New(rand.NewSource(20260923))
	const count = 5000
	base := wavPCM(1, 1, 16, 16000, 64)
	inputs := make([][]byte, 0, count)
	for len(inputs) < count {
		b := append([]byte(nil), base...)
		// Mutate one to three bytes anywhere in the header region, which is
		// where every rule but R2 is decided.
		for n := 1 + rng.Intn(3); n > 0; n-- {
			b[rng.Intn(44)] = byte(rng.Intn(256))
		}
		// Sometimes truncate, to reach R2, R4 and the loop's exit paths.
		if rng.Intn(4) == 0 {
			b = b[:rng.Intn(len(b))]
		}
		inputs = append(inputs, b)
	}

	real := runNeMo(t, binary, inputs)
	agreed, disagreed := 0, 0
	for i, in := range inputs {
		got := nemoAccepts(in)
		switch {
		case real[i].accepted && got != nil:
			disagreed++
			if disagreed <= 5 {
				t.Errorf("mutation %d: real parser ACCEPTS, Go spec refuses at %v\n  bytes: %x",
					i, got, in)
			}
		case !real[i].accepted && got == nil:
			disagreed++
			if disagreed <= 5 {
				t.Errorf("mutation %d: real parser REFUSES (%q), Go spec accepts\n  bytes: %x",
					i, real[i].reason, in)
			}
		case !real[i].accepted && goRuleOf(got) != real[i].rule:
			disagreed++
			if disagreed <= 5 {
				t.Errorf("mutation %d: both refuse, real parser at %s (%q), Go spec at %s\n  bytes: %x",
					i, real[i].rule, real[i].reason, goRuleOf(got), in)
			}
		default:
			agreed++
		}
	}
	t.Logf("verdicts and rules agreed on %d of %d mutated headers", agreed, len(inputs))
}
