package config

import "testing"

func TestCanonPCMEncoding(t *testing.T) {
	for in, want := range map[string]string{
		"s16": "s16", "S16": "s16", " pcm_s16le ": "s16", "s16le": "s16", "int16": "s16", "16": "s16",
		"f32": "f32", "float32": "f32", "pcm_f32le": "f32", "float": "f32",
		"u8": "u8", "s24": "s24", "s32": "s32", "f64": "f64", "double": "f64",
		// Not a name corrallm knows. It must NOT canonicalise: an unrecognised
		// entry that silently vanished would turn a one-item pcm list into an
		// empty one, and empty means "no limit" — a typo that widens what a
		// picky backend is believed to accept. Validate rejects these at load
		// for exactly that reason.
		"pcm_s16be": "", "adpcm_ima_wav": "", "mulaw": "", "": "", "s17": "",
	} {
		if got := CanonPCMEncoding(in); got != want {
			t.Errorf("CanonPCMEncoding(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every name in the vocabulary is one corrallm can actually produce. Offering
// a name with no encoder behind it would be a declaration corrallm could not
// honour, and it would be found out at the worst moment: mid-request.
func TestEveryKnownPCMEncodingHasAnEncoder(t *testing.T) {
	names := KnownPCMEncodings()
	if len(names) == 0 {
		t.Fatal("the vocabulary is empty")
	}
	for _, n := range names {
		if CanonPCMEncoding(n) != n {
			t.Errorf("%q is not its own canonical form", n)
		}
		if FFmpegPCMCodec(n) == "" {
			t.Errorf("%q is offered but has no encoder", n)
		}
	}
	if FFmpegPCMCodec("nonsense") != "" {
		t.Error("FFmpegPCMCodec invented an encoder")
	}
}

func TestAudioBounded(t *testing.T) {
	if (ModalitySpec{Formats: []string{"wav"}}).AudioBounded() {
		t.Error("a container list is not a bound below the container")
	}
	for _, s := range []ModalitySpec{
		{MinSampleRate: 8000}, {MaxSampleRate: 96000}, {MaxChannels: 2}, {PCM: []string{"s16"}},
	} {
		if !s.AudioBounded() {
			t.Errorf("%+v reads as unbounded", s)
		}
	}
}

// The load-time guard. Each of these is a declaration corrallm could only
// misread, so it is refused while the operator is still in the room.
func TestValidateAudioSpec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modality string
		spec     ModalitySpec
		wantErr  bool
	}{
		{"NeMo's real contract", ModalityAudio, ModalitySpec{
			Formats: []string{"wav"}, MinSampleRate: 8000, MaxSampleRate: 96000,
			MaxChannels: 2, PCM: []string{"s16", "f32"}}, false},
		{"nothing declared", ModalityAudio, ModalitySpec{}, false},
		{"containers only", ModalityAudio, ModalitySpec{Formats: []string{"wav"}}, false},
		{"a pcm name nobody knows", ModalityAudio, ModalitySpec{PCM: []string{"s16be"}}, true},
		{"a window that accepts nothing", ModalityAudio, ModalitySpec{
			MinSampleRate: 48000, MaxSampleRate: 16000}, true},
		{"a negative rate", ModalityAudio, ModalitySpec{MinSampleRate: -1}, true},
		{"audio bounds on the image modality", ModalityImage, ModalitySpec{PCM: []string{"s16"}}, true},
		// The two-call diarized join's opt-in (P35 §12.9(e)). A path corrallm
		// cannot build a URL from is refused at LOAD, because the failure it
		// causes otherwise is quiet: the second call 404s and a caller who
		// asked for speakers gets an error hours after the typo.
		{"a diarization endpoint", ModalityAudio, ModalitySpec{
			Formats: []string{"wav"}, DiarizationPath: "/v1/audio/diarizations"}, false},
		{"a relative diarization endpoint", ModalityAudio, ModalitySpec{
			DiarizationPath: "v1/audio/diarizations"}, true},
		{"a diarization endpoint on the image modality", ModalityImage, ModalitySpec{
			DiarizationPath: "/v1/audio/diarizations"}, true},
		{"an image spec, untouched", ModalityImage, ModalitySpec{
			Formats: []string{"png"}, MaxResolution: 1024}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAudioSpec("m", tc.modality, tc.spec)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
