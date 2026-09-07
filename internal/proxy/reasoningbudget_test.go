package proxy

import (
	"encoding/json"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func budgetCfg(frac float64, min, max int) *config.SamplingConfig {
	return &config.SamplingConfig{
		Thinking: config.SamplingProfile{},
		Default:  "thinking",
		ReasoningBudget: &config.ReasoningBudget{
			FractionOfRemaining: frac, Min: min, Max: max,
		},
	}
}

// The point of computing it per request: the same model, the same setting, and
// a budget that shrinks as the prompt grows. A fixed number cannot do this —
// it is either most of the window on a long prompt or a stub on a short one.
func TestBudgetShrinksAsThePromptGrows(t *testing.T) {
	cfg := &config.ReasoningBudget{FractionOfRemaining: 0.2, Min: 1024}
	ctx := 188_000

	small := reasoningBudgetFor(4_000, ctx, cfg)   // ~1k tokens of prompt
	large := reasoningBudgetFor(600_000, ctx, cfg) // ~150k tokens of prompt
	if small <= large {
		t.Fatalf("small prompt budget %d should exceed large prompt budget %d", small, large)
	}
	// 20% of (188000 - 1000) and of (188000 - 150000).
	if small != 37_400 {
		t.Errorf("small = %d, want 20%% of the 187k remaining", small)
	}
	if large != 7_600 {
		t.Errorf("large = %d, want 20%% of the 38k remaining", large)
	}
}

// A prompt at or past the window gets NO budget. There is nothing to divide,
// and the context limit refuses the request on its own terms with a better
// message than a zero budget would produce.
func TestNoBudgetWhenThePromptFillsTheWindow(t *testing.T) {
	cfg := &config.ReasoningBudget{FractionOfRemaining: 0.2, Min: 1024}
	if got := reasoningBudgetFor(800_000, 188_000, cfg); got != 0 {
		t.Errorf("got %d, want 0 for a prompt past the window", got)
	}
}

// Below the floor it goes UNRESTRICTED rather than clamping up. Raising a
// nearly-full window's budget to the floor would hand it tokens it cannot
// afford — the opposite of what the cap is for.
func TestBelowTheFloorLeavesItUnrestricted(t *testing.T) {
	cfg := &config.ReasoningBudget{FractionOfRemaining: 0.2, Min: 8_000}
	// 20% of the ~10k remaining is 2k, under the 8k floor.
	if got := reasoningBudgetFor(712_000, 188_000, cfg); got != 0 {
		t.Errorf("got %d, want 0 (unrestricted) below the floor", got)
	}
}

func TestMaxCapsALargeWindow(t *testing.T) {
	cfg := &config.ReasoningBudget{FractionOfRemaining: 0.2, Min: 1024, Max: 16_000}
	if got := reasoningBudgetFor(4_000, 1_000_000, cfg); got != 16_000 {
		t.Errorf("got %d, want the 16000 cap", got)
	}
}

// Zero fraction is "leave it alone", not "budget of nothing" — llama.cpp reads
// a 0 token budget as end reasoning immediately, so the two must not collide.
func TestZeroFractionDisablesRatherThanSilences(t *testing.T) {
	if got := reasoningBudgetFor(4_000, 188_000, &config.ReasoningBudget{}); got != 0 {
		t.Errorf("got %d, want 0 meaning 'set nothing'", got)
	}
	body := []byte(`{"model":"m","messages":[]}`)
	out, did := applyReasoningBudget(body, budgetCfg(0, 1024, 0), 188_000)
	if did {
		t.Errorf("a zero fraction wrote a budget: %s", out)
	}
}

func budgetOf(t *testing.T, body []byte) (float64, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m["reasoning_budget_tokens"].(float64)
	return v, ok
}

// THE CALLER ALWAYS WINS, including a caller that sent 0 — which means "do not
// think" and must not be overwritten with a generous budget.
func TestTheCallersOwnBudgetStands(t *testing.T) {
	for _, sent := range []string{"0", "500"} {
		body := []byte(`{"model":"m","reasoning_budget_tokens":` + sent + `,"messages":[]}`)
		out, did := applyReasoningBudget(body, budgetCfg(0.2, 1024, 0), 188_000)
		if did {
			t.Errorf("overwrote the caller's budget of %s: %s", sent, out)
		}
	}
}

// A budget is itself a thinking signal to llama.cpp, so writing one onto a
// request that is NOT thinking would turn reasoning on for a caller who never
// asked — a behaviour change wearing the costume of a limit.
func TestNoBudgetOnANonThinkingRequest(t *testing.T) {
	cfg := budgetCfg(0.2, 1024, 0)
	cfg.Default = "instruct"
	body := []byte(`{"model":"m","messages":[]}`)
	if _, did := applyReasoningBudget(body, cfg, 188_000); did {
		t.Error("set a budget on an instruct-default request")
	}
	// And when the caller explicitly turns thinking off, even under a thinking
	// default.
	off := []byte(`{"model":"m","chat_template_kwargs":{"enable_thinking":false},"messages":[]}`)
	if _, did := applyReasoningBudget(off, budgetCfg(0.2, 1024, 0), 188_000); did {
		t.Error("set a budget on a request that opted out of thinking")
	}
}

func TestBudgetIsWrittenOnAThinkingRequest(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, did := applyReasoningBudget(body, budgetCfg(0.2, 1024, 0), 188_000)
	if !did {
		t.Fatal("no budget written on a thinking request")
	}
	got, ok := budgetOf(t, out)
	if !ok || got <= 0 {
		t.Fatalf("budget = %v, want a positive number", got)
	}
}

// No context configured means the proxy does not know the window, and guessing
// one would be worse than leaving the backend's own default alone.
func TestNoContextMeansNoBudget(t *testing.T) {
	if got := reasoningBudgetFor(4_000, 0, &config.ReasoningBudget{FractionOfRemaining: 0.2}); got != 0 {
		t.Errorf("got %d, want 0 when the context is unknown", got)
	}
}
