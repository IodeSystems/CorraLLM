package proxy

import (
	"encoding/json"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func boolp(b bool) *bool { return &b }

func thinkingModel() *config.SamplingConfig {
	return &config.SamplingConfig{
		Thinking: config.SamplingProfile{Temperature: f(1.0), TopP: f(0.95)},
		Instruct: config.SamplingProfile{Temperature: f(0.7), TopP: f(0.8)},
		Default:  "thinking",
	}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func enableThinking(t *testing.T, body []byte) (val bool, present bool) {
	t.Helper()
	kw, ok := decodeBody(t, body)["chat_template_kwargs"].(map[string]any)
	if !ok {
		return false, false
	}
	v, ok := kw["enable_thinking"].(bool)
	return v, ok
}

// The case this was built for: one model, two callers who want opposite things.
//
// An agentic caller — every request a tool call — is throttled by reasoning it
// did not ask for. On this box turning thinking on took sk-aw4 from 9.89
// requests a minute to 0.85, a third of them exhausting an 8k budget in full.
// The mode was a property of the MODEL, so it could not differ per caller.
func TestAnAgenticCallerOptsOutWhileTheModelStaysThinking(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)

	agent, did := applySamplingProfile(body, thinkingModel(), config.KeyPolicy{Thinking: boolp(false)})
	if !did {
		t.Fatal("no rewrite for a caller that opted out")
	}
	// The BACKEND has to be told, not just the sampler picked: llama-server was
	// launched thinking, and choosing the instruct sampler without saying so
	// would sample for a mode the model is not in.
	if v, present := enableThinking(t, agent); !present || v {
		t.Errorf("enable_thinking = %v/%v, want an explicit false for the backend", v, present)
	}
	if got := decodeBody(t, agent)["temperature"]; got != 0.7 {
		t.Errorf("temperature = %v, want the instruct 0.7", got)
	}

	// And a caller with no preference is untouched by any of it.
	plain, did := applySamplingProfile(body, thinkingModel(), config.KeyPolicy{})
	if !did {
		t.Fatal("no rewrite for a plain caller")
	}
	if _, present := enableThinking(t, plain); present {
		t.Error("wrote a mode switch for a caller that expressed no preference")
	}
	if got := decodeBody(t, plain)["temperature"]; got != 1.0 {
		t.Errorf("temperature = %v, want the model's thinking default 1.0", got)
	}
}

// The reverse: a model defaulted to instruct, one caller who wants reasoning.
func TestACallerCanOptINOnAnInstructModel(t *testing.T) {
	cfg := thinkingModel()
	cfg.Default = "instruct"
	out, did := applySamplingProfile([]byte(`{"model":"m","messages":[]}`), cfg, config.KeyPolicy{Thinking: boolp(true)})
	if !did {
		t.Fatal("no rewrite")
	}
	if v, present := enableThinking(t, out); !present || !v {
		t.Errorf("enable_thinking = %v/%v, want true", v, present)
	}
	if got := decodeBody(t, out)["temperature"]; got != 1.0 {
		t.Errorf("temperature = %v, want the thinking 1.0", got)
	}
}

// THE REQUEST OUTRANKS THE POLICY. The caller knows which of its own calls is
// the hard one, and that judgement beats any default configured for it.
func TestTheRequestOutranksTheCallersPolicy(t *testing.T) {
	body := []byte(`{"model":"m","chat_template_kwargs":{"enable_thinking":true},"messages":[]}`)
	out, _ := applySamplingProfile(body, thinkingModel(), config.KeyPolicy{Thinking: boolp(false)})
	if v, _ := enableThinking(t, out); !v {
		t.Error("the policy overwrote the request's own enable_thinking")
	}
	if got := decodeBody(t, out)["temperature"]; got != 1.0 {
		t.Errorf("temperature = %v, want the thinking sampler the REQUEST asked for", got)
	}
}

// The mode switch leaves the body self-describing, so the budget stage reads
// the decision rather than re-deriving it — and a caller opted out of thinking
// gets no reasoning budget, which would otherwise turn reasoning back on.
func TestOptingOutAlsoSuppressesTheReasoningBudget(t *testing.T) {
	cfg := thinkingModel()
	cfg.ReasoningBudget = &config.ReasoningBudget{FractionOfRemaining: 0.2, Min: 1024}
	out, _ := applySamplingProfile([]byte(`{"model":"m","messages":[]}`), cfg, config.KeyPolicy{Thinking: boolp(false)})
	if _, did := applyReasoningBudget(out, cfg, 188_000); did {
		t.Error("set a reasoning budget on a caller that opted out of thinking")
	}
}
