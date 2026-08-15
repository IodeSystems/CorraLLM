package proxy

import (
	"encoding/json"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

// qwenLike mirrors the shape a real card produces: two modes that disagree about
// three separate knobs, which is the whole reason one server flag cannot serve
// both.
func qwenLike() *config.SamplingConfig {
	return &config.SamplingConfig{
		Thinking: config.SamplingProfile{Temperature: f(1.0), TopP: f(0.95), TopK: i(20), PresencePenalty: f(0.0)},
		Instruct: config.SamplingProfile{Temperature: f(0.7), TopP: f(0.80), TopK: i(20), PresencePenalty: f(1.5)},
		Default:  "instruct",
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	return m
}

// TestModeSelectsProfile: each dialect's way of asking for thinking picks the
// thinking profile, and silence falls back to the configured default.
func TestModeSelectsProfile(t *testing.T) {
	cases := map[string]struct {
		body     string
		wantTemp float64
	}{
		"silence uses the default":     {`{"messages":[]}`, 0.7},
		"llama.cpp kwarg on":           {`{"messages":[],"chat_template_kwargs":{"enable_thinking":true}}`, 1.0},
		"llama.cpp kwarg off":          {`{"messages":[],"chat_template_kwargs":{"enable_thinking":false}}`, 0.7},
		"anthropic thinking enabled":   {`{"messages":[],"thinking":{"type":"enabled","budget_tokens":512}}`, 1.0},
		"openai reasoning_effort high": {`{"messages":[],"reasoning_effort":"high"}`, 1.0},
		"openai reasoning_effort none": {`{"messages":[],"reasoning_effort":"none"}`, 0.7},
		"positive budget means think":  {`{"messages":[],"reasoning_budget_tokens":1024}`, 1.0},
		// 0 is llama.cpp's "end reasoning immediately", i.e. do not think.
		"zero budget means do not":  {`{"messages":[],"reasoning_budget_tokens":0}`, 0.7},
		"negative budget is silent": {`{"messages":[],"reasoning_budget_tokens":-1}`, 0.7},
	}
	for name, tc := range cases {
		out, did := applySamplingProfile([]byte(tc.body), qwenLike())
		if !did {
			t.Errorf("%s: nothing applied", name)
			continue
		}
		if got := decode(t, out)["temperature"]; got != tc.wantTemp {
			t.Errorf("%s: temperature = %v, want %v", name, got, tc.wantTemp)
		}
	}
}

// TestCallerAlwaysWins is the load-bearing invariant. llm-bench's --pin-sampling
// sends temperature 0 and a seed SO THAT a probe is not a coin flip; a proxy
// that overwrote it would silently restore the coin flip and no output would say
// so.
func TestCallerAlwaysWins(t *testing.T) {
	body := `{"messages":[],"temperature":0,"top_p":0.1}`
	out, _ := applySamplingProfile([]byte(body), qwenLike())
	m := decode(t, out)
	if m["temperature"] != 0.0 {
		t.Errorf("caller's temperature 0 was overwritten with %v", m["temperature"])
	}
	if m["top_p"] != 0.1 {
		t.Errorf("caller's top_p was overwritten with %v", m["top_p"])
	}
	// The knobs the caller had no opinion about are still filled in.
	if m["presence_penalty"] != 1.5 {
		t.Errorf("presence_penalty = %v, want the profile's 1.5", m["presence_penalty"])
	}
}

// TestDefaultThinking: a backend launched in thinking mode must say so, or every
// unmarked request gets the sampler for the mode it is not in.
func TestDefaultThinking(t *testing.T) {
	cfg := qwenLike()
	cfg.Default = "thinking"
	out, _ := applySamplingProfile([]byte(`{"messages":[]}`), cfg)
	if got := decode(t, out)["temperature"]; got != 1.0 {
		t.Errorf("temperature = %v, want the thinking profile's 1.0", got)
	}
}

// TestNoConfigNoChange: absent config must be byte-identical passthrough, since
// every model behaved that way before this existed.
func TestNoConfigNoChange(t *testing.T) {
	body := []byte(`{"messages":[],"temperature":0.3}`)
	out, did := applySamplingProfile(body, nil)
	if did || string(out) != string(body) {
		t.Errorf("nil config must not touch the body: did=%v", did)
	}
	// An empty profile for the selected mode likewise sends nothing.
	empty := &config.SamplingConfig{Default: "instruct"}
	out, did = applySamplingProfile(body, empty)
	if did || string(out) != string(body) {
		t.Errorf("empty profile must not touch the body: did=%v", did)
	}
}

// TestUnparseableBodyPassesThrough: this rewrite is an improvement, never a
// precondition for serving. A body we cannot read must still reach the backend.
func TestUnparseableBodyPassesThrough(t *testing.T) {
	body := []byte(`not json at all`)
	out, did := applySamplingProfile(body, qwenLike())
	if did || string(out) != string(body) {
		t.Errorf("unparseable body must pass through untouched: did=%v", did)
	}
}

// TestZeroValuedProfileFieldIsSent: 0 is a REAL value for these knobs
// (presence_penalty 0 is off, temperature 0 is greedy). A profile that sets one
// must send it — the pointer exists so this is distinguishable from unset.
func TestZeroValuedProfileFieldIsSent(t *testing.T) {
	cfg := &config.SamplingConfig{
		Instruct: config.SamplingProfile{Temperature: f(0), PresencePenalty: f(0)},
		Default:  "instruct",
	}
	out, did := applySamplingProfile([]byte(`{"messages":[]}`), cfg)
	if !did {
		t.Fatal("a profile of zeroes still has an opinion and must be applied")
	}
	m := decode(t, out)
	if m["temperature"] != 0.0 || m["presence_penalty"] != 0.0 {
		t.Errorf("zero-valued fields were dropped: %+v", m)
	}
}

// TestUnsetProfileFieldsAreNotSent: a profile that names three knobs must not
// invent the other four, or it silently disables whatever the backend was
// launched with.
func TestUnsetProfileFieldsAreNotSent(t *testing.T) {
	cfg := &config.SamplingConfig{
		Instruct: config.SamplingProfile{Temperature: f(0.7)},
		Default:  "instruct",
	}
	out, _ := applySamplingProfile([]byte(`{"messages":[]}`), cfg)
	m := decode(t, out)
	for _, k := range []string{"top_p", "top_k", "min_p", "presence_penalty", "frequency_penalty", "repeat_penalty"} {
		if _, present := m[k]; present {
			t.Errorf("%s was sent despite being unset in the profile", k)
		}
	}
}
