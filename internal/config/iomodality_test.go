package config

import (
	"reflect"
	"testing"
)

// TestTypeIOAndDerivedCapability pins both halves of the claim: a cost type maps
// to an inputs/outputs pair, and the flat capability string is DERIVED from it
// rather than maintained beside it. The table is one table on purpose — the next
// surface added has to answer "what does it return" in the same place it picks a
// name.
func TestTypeIOAndDerivedCapability(t *testing.T) {
	cases := []struct {
		typ  string
		in   []string
		out  []string
		want string
	}{
		{"chat", []string{"text"}, []string{"text"}, "chat"},
		{"local", []string{"text"}, []string{"text"}, "chat"},
		{"embed", []string{"text"}, []string{"vector"}, "embeddings"},
		{"rerank", []string{"text"}, []string{"ranking"}, "rerank"},
		{"tts", []string{"text"}, []string{"audio"}, "audio.tts"},
		{"kokoro-speech", []string{"text"}, []string{"audio"}, "audio.tts"},
		{"stt", []string{"audio"}, []string{"text"}, "audio.stt"},
		{"parakeet", []string{"audio"}, []string{"text"}, "audio.stt"},
		{"diffusion", []string{"text"}, []string{"image"}, "image.generate"},
		{"stable-diffusion", []string{"text"}, []string{"image"}, "image.generate"},
		{"sdxl", []string{"text"}, []string{"image"}, "image.generate"},
		{"txt2img", []string{"text"}, []string{"image"}, "image.generate"},
		// Delivery, not IO: realtime is audio-in/text-out exactly like batch STT.
		// The label splits them because the ROUTE differs (WebSocket vs POST),
		// and that axis is not a modality.
		{"realtime-stt", []string{"audio"}, []string{"text"}, "audio.realtime"},
	}
	for _, c := range cases {
		io := TypeIO(c.typ)
		if !reflect.DeepEqual(io.In, c.in) || !reflect.DeepEqual(io.Out, c.out) {
			t.Errorf("TypeIO(%q) = in %v out %v, want in %v out %v", c.typ, io.In, io.Out, c.in, c.out)
		}
		if got := Capability(c.typ); got != c.want {
			t.Errorf("Capability(%q) = %q, want %q", c.typ, got, c.want)
		}
	}
}

// TestImageInputIsNotImageOutput is the distinction the flat capability string
// could not make, and the reason this file exists. Four models, two of which
// touch images, on opposite sides.
func TestImageInputIsNotImageOutput(t *testing.T) {
	vision := Model{Type: "chat", Modalities: map[string]ModalitySpec{"text": {}, "image": {}}}
	textOnly := Model{Type: "chat"}
	gen := Model{Type: "diffusion"}
	// An image EDITOR: takes a picture and a prompt, returns a picture. The one
	// shape that needs both halves at once, and the IO model carries it without
	// a new capability name.
	editor := Model{Type: "diffusion", Modalities: map[string]ModalitySpec{"text": {}, "image": {}}}

	for _, c := range []struct {
		name          string
		m             Model
		accepts, emit bool
	}{
		{"vision", vision, true, false},
		{"text-only", textOnly, false, false},
		{"generator", gen, false, true},
		{"editor", editor, true, true},
	} {
		io := ModelIO(c.name, c.m, false)
		if io.Accepts(ModalityImage) != c.accepts {
			t.Errorf("%s accepts(image) = %v, want %v (in %v)", c.name, !c.accepts, c.accepts, io.In)
		}
		if io.Emits(ModalityImage) != c.emit {
			t.Errorf("%s emits(image) = %v, want %v (out %v)", c.name, !c.emit, c.emit, io.Out)
		}
	}

	// The trap the type vocabulary is built to avoid: "image" alone reads as an
	// INPUT modality name — it is what the probe writes for a vision model — so
	// a type carrying only that word must NOT be taken for a generator.
	if Capability("image") == "image.generate" {
		t.Error(`bare type "image" was read as a generator; it is the input-modality spelling`)
	}
	if Capability("vision-image") == "image.generate" {
		t.Error(`"vision-image" was read as a generator`)
	}
}

// TestModelIOInputsBeatTheType covers the asymmetry ModelIO documents: inputs
// are real data (probed or declared) and override the cost class; outputs are
// inferred from the class and have no such source.
func TestModelIOInputsBeatTheType(t *testing.T) {
	m := Model{Type: "chat", Modalities: map[string]ModalitySpec{"audio": {}, "text": {}}}
	io := ModelIO("omni", m, false)
	if !io.Accepts(ModalityAudio) {
		t.Errorf("declared audio input lost to the cost type: %v", io.In)
	}
	// Sorted, because a map's order is random and a shuffling list reads as a
	// change to anything diffing /v1/models.
	if !reflect.DeepEqual(io.In, []string{"audio", "text"}) {
		t.Errorf("inputs = %v, want sorted [audio text]", io.In)
	}
	if !reflect.DeepEqual(io.Out, []string{"text"}) {
		t.Errorf("outputs = %v, want [text] from the cost type", io.Out)
	}
}
