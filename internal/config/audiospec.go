package config

import (
	"fmt"
	"sort"
	"strings"
)

// The PCM vocabulary for ModalitySpec.PCM (P35 phase 2).
//
// WHY THIS LIVES IN config AND NOT BESIDE THE TRANSCODER. Dropping a spelling
// nobody recognises is safe for a CONTAINER list — an unknown name narrows
// what corrallm believes the backend accepts, so the worst case is a
// conversion that was not needed. It is NOT safe here: drop the only entry of
// `pcm: [s16le-typo]` and the list becomes empty, empty means "no limit", and
// a typo has just told corrallm that a picky backend will take anything. So
// the vocabulary is closed and Validate rejects a name outside it at load,
// which is the only point at which the operator is still in the room.
//
// The names are the ffmpeg codec names with the endianness suffix dropped.
// corrallm writes little-endian WAV and nothing else: big-endian PCM in a RIFF
// container is not a thing an STT backend in this class reads, and offering a
// name we cannot produce would be a declaration corrallm could not honour.
var pcmEncodings = map[string]string{
	// canonical → the ffmpeg -c:a that produces it
	"u8":  "pcm_u8",
	"s16": "pcm_s16le",
	"s24": "pcm_s24le",
	"s32": "pcm_s32le",
	"f32": "pcm_f32le",
	"f64": "pcm_f64le",
}

// pcmAliases folds the spellings an operator would reasonably write onto the
// canonical name. Left column: ffmpeg codec names, WAV-header vocabulary, and
// the plain words.
var pcmAliases = map[string]string{
	"pcm_u8": "u8", "u8": "u8", "uint8": "u8", "8": "u8", "unsigned8": "u8",
	"pcm_s16le": "s16", "s16le": "s16", "s16": "s16", "int16": "s16", "pcm16": "s16", "16": "s16",
	"pcm_s24le": "s24", "s24le": "s24", "s24": "s24", "int24": "s24", "pcm24": "s24", "24": "s24",
	"pcm_s32le": "s32", "s32le": "s32", "s32": "s32", "int32": "s32", "pcm32": "s32", "32": "s32",
	"pcm_f32le": "f32", "f32le": "f32", "f32": "f32", "float32": "f32", "float": "f32",
	"pcm_f64le": "f64", "f64le": "f64", "f64": "f64", "float64": "f64", "double": "f64",
}

// CanonPCMEncoding folds one declared sample encoding onto its canonical name,
// or returns "" when it is not a name corrallm knows. Callers that are reading
// config must treat "" as an error, not as a skip — see the note above.
func CanonPCMEncoding(s string) string {
	return pcmAliases[strings.ToLower(strings.TrimSpace(s))]
}

// FFmpegPCMCodec is the encoder that produces a canonical PCM name, or "" if
// corrallm cannot produce it. Separate from CanonPCMEncoding because "a name
// we understand" and "a thing we can make" are different questions, and a
// refusal has to be able to say which one failed.
func FFmpegPCMCodec(canonical string) string { return pcmEncodings[canonical] }

// KnownPCMEncodings lists the vocabulary, sorted, for error messages.
func KnownPCMEncodings() []string {
	out := make([]string, 0, len(pcmEncodings))
	for k := range pcmEncodings {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateAudioSpec is the load-time typo guard for the phase 2 fields. It
// refuses a declaration corrallm could only misread: an unknown encoding name
// (which would read as "no limit"), and a rate window with nothing in it
// (which would refuse every file while looking like a constraint).
func validateAudioSpec(model, modality string, s ModalitySpec) error {
	if modality != ModalityAudio {
		if s.AudioBounded() {
			return fmt.Errorf("model %q: minSampleRate/maxSampleRate/maxChannels/pcm apply to the audio modality, not %q", model, modality)
		}
		if s.DiarizationPath != "" {
			return fmt.Errorf("model %q: diarizationPath applies to the audio modality, not %q", model, modality)
		}
		return nil
	}
	// A path corrallm cannot build a URL from is refused at LOAD, not at the
	// first diarized request. The failure it prevents is quiet: a relative
	// path resolves against nothing, the second call 404s, and a caller who
	// asked for speakers gets an error hours later for a typo an operator was
	// still in the room for.
	if s.DiarizationPath != "" && !strings.HasPrefix(s.DiarizationPath, "/") {
		return fmt.Errorf("model %q: audio diarizationPath %q must begin with \"/\" (e.g. /v1/audio/diarizations)",
			model, s.DiarizationPath)
	}
	for _, p := range s.PCM {
		if CanonPCMEncoding(p) == "" {
			return fmt.Errorf("model %q: unknown audio pcm encoding %q (want %s)",
				model, p, strings.Join(KnownPCMEncodings(), "|"))
		}
	}
	if s.MinSampleRate < 0 || s.MaxSampleRate < 0 || s.MaxChannels < 0 {
		return fmt.Errorf("model %q: audio minSampleRate/maxSampleRate/maxChannels must not be negative", model)
	}
	if s.MinSampleRate > 0 && s.MaxSampleRate > 0 && s.MinSampleRate > s.MaxSampleRate {
		return fmt.Errorf("model %q: audio minSampleRate %d is above maxSampleRate %d, which accepts nothing",
			model, s.MinSampleRate, s.MaxSampleRate)
	}
	return nil
}
