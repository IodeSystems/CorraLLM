package config

import (
	"sort"
	"strings"
)

// What a model takes and what it gives back, as two lists rather than one
// label.
//
// corrallm classified models with a flat capability string — chat, audio.tts,
// audio.stt — and that encoding cannot say the thing the image surface needs
// said. "Does this model handle images" has two different answers: a vision
// chat model ACCEPTS them and emits text; a diffusion model accepts text and
// EMITS one. Under a flat string those are two unrelated names and nothing
// stops the wrong one from being routed to /v1/images/generations except a
// rule somebody remembered to write. Under IO the question is `Emits(image)`
// and the vision model answers no without any rule existing:
//
//	chat (text)    in [text]        out [text]
//	chat (vision)  in [text image]  out [text]
//	STT            in [audio]       out [text]
//	TTS            in [text]        out [audio]
//	image generate in [text]        out [image]
//	image edit     in [text image]  out [image]
//
// The capability strings are the same information encoded worse, and are now
// derived from this (Capability → TypeIO.capability) rather than maintained
// beside it.
type IO struct {
	In  []string `json:"in"`
	Out []string `json:"out"`
}

// The modality vocabulary. text/image/audio are media and are the vocabulary
// the probe path already speaks (proc.recordCapabilities translates llama.cpp's
// vision/video/audio into them). vector and ranking are not media — they are
// what embeddings and rerank return, and they are here because the alternative
// is an IO model that covers five of corrallm's seven surfaces and leaves the
// other two to the old string. A route asks what it needs by name either way.
const (
	ModalityText    = "text"
	ModalityImage   = "image"
	ModalityAudio   = "audio"
	ModalityVector  = "vector"
	ModalityRanking = "ranking"
)

// Accepts reports whether this IO takes the modality as input.
func (io IO) Accepts(m string) bool { return containsMod(io.In, m) }

// Emits reports whether this IO returns the modality. This is the predicate a
// route gates on: /v1/images/generations needs Emits(ModalityImage), and a
// vision chat model fails it for the right reason rather than by a name check.
func (io IO) Emits(m string) bool { return containsMod(io.Out, m) }

func containsMod(all []string, want string) bool {
	for _, s := range all {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// TypeIO infers what a backend of this cost `type` takes and returns.
//
// WHERE THE TWO HALVES COME FROM IS NOT THE SAME, and that asymmetry is the
// honest state of this rather than a design. INPUTS are probed: the backend is
// asked (llama.cpp's /props `modalities`), the answer is cached per placement,
// and ModelIO prefers it over anything written down — which is why a vision
// model nobody declared still gets vision probes. OUTPUTS have no such source.
// Nothing corrallm talks to reports what it emits: /props does not carry it,
// and sd-server has no capability route at all. So outputs are inferred from
// the operator's `type:`, which is a DECLARATION wearing an inference's
// clothes — exactly what the probing work exists to replace.
//
// The observable that would replace it: which ROUTES a backend answers.
// A server that answers /v1/images/generations emits images; one that answers
// /v1/audio/speech emits audio. That is a fact about the running process, not
// about a string in a config file, and it is a probe corrallm could actually
// run. Until it does, this function is where the declaration is read, and it
// is one function rather than a field on every model so there is one place to
// replace.
func TypeIO(typ string) IO {
	t := strings.ToLower(typ)
	switch {
	case isImageGenType(t):
		return IO{In: []string{ModalityText}, Out: []string{ModalityImage}}
	case strings.Contains(t, "tts") || strings.Contains(t, "speech"):
		return IO{In: []string{ModalityText}, Out: []string{ModalityAudio}}
	case strings.Contains(t, "stt") || strings.Contains(t, "asr") ||
		strings.Contains(t, "whisper") || strings.Contains(t, "transcri") || strings.Contains(t, "parakeet"):
		return IO{In: []string{ModalityAudio}, Out: []string{ModalityText}}
	case strings.Contains(t, "embed"):
		return IO{In: []string{ModalityText}, Out: []string{ModalityVector}}
	case strings.Contains(t, "rerank"):
		return IO{In: []string{ModalityText}, Out: []string{ModalityRanking}}
	default:
		return IO{In: []string{ModalityText}, Out: []string{ModalityText}}
	}
}

// capability names the point in IO space this pair sits at, for the flat
// string the rest of corrallm still speaks. Keyed on the OUTPUT first, because
// output is what distinguishes the surfaces a caller is choosing between:
// text-out with audio-in is transcription, audio-out is synthesis.
func (io IO) capability() string {
	switch {
	case io.Emits(ModalityImage):
		return "image.generate"
	case io.Emits(ModalityAudio):
		return "audio.tts"
	case io.Emits(ModalityVector):
		return "embeddings"
	case io.Emits(ModalityRanking):
		return "rerank"
	case io.Accepts(ModalityAudio):
		return "audio.stt" // audio in, text out
	default:
		return "chat"
	}
}

// isImageGenType reports whether a lowercased cost type names a text-to-image
// GENERATOR.
//
// The word "image" ON ITS OWN is deliberately not enough, and the IO model is
// why it can be said precisely: "image" is an INPUT modality name — it is what
// proc.recordCapabilities writes for a vision model — so a type containing it
// is at least as likely to mean "takes pictures" as "makes them". A generator
// is the thing that puts image in Out, and until a backend can be asked, the
// type has to say so with a word that only a generator would carry.
// `diffusion` is the recommended spelling: it covers `stable-diffusion`, the
// backend this surface was built for.
func isImageGenType(t string) bool {
	for _, w := range []string{"diffusion", "txt2img", "text2img", "imagegen", "image-gen", "image_gen", "sdxl"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// ModelIO is what a served model takes and returns.
//
// The two halves are sourced differently and this is where that shows. Out
// comes from TypeIO — inferred, see there. In comes from the model's
// modalities, which is probed-beats-declared (EffectiveModalities), so a
// vision model the probe found reports [image text] even though its type says
// "chat" and its type-level IO says [text]. That override is the whole point:
// the input half is already real data and must not be flattened back onto the
// cost class.
//
// audioDefault is passed through to EffectiveModalities for the same reason it
// exists there — audio-ness lives in the cost model, not in config.
func ModelIO(served string, m Model, audioDefault bool) IO {
	io := TypeIO(m.Type)
	if mods := m.EffectiveModalities(served, audioDefault); len(mods) > 0 {
		in := make([]string, 0, len(mods))
		for k := range mods {
			in = append(in, k)
		}
		sort.Strings(in) // map order is random; a shuffling list reads as a change
		io.In = in
	}
	return io
}
