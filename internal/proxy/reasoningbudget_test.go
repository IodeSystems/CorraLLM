package proxy

import (
	"encoding/json"
	"strings"
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
	out, did, _ := applyReasoningBudget(body, budgetCfg(0, 1024, 0), 188_000)
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
		out, did, _ := applyReasoningBudget(body, budgetCfg(0.2, 1024, 0), 188_000)
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
	if _, did, _ := applyReasoningBudget(body, cfg, 188_000); did {
		t.Error("set a budget on an instruct-default request")
	}
	// And when the caller explicitly turns thinking off, even under a thinking
	// default.
	off := []byte(`{"model":"m","chat_template_kwargs":{"enable_thinking":false},"messages":[]}`)
	if _, did, _ := applyReasoningBudget(off, budgetCfg(0.2, 1024, 0), 188_000); did {
		t.Error("set a budget on a request that opted out of thinking")
	}
}

func TestBudgetIsWrittenOnAThinkingRequest(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, did, _ := applyReasoningBudget(body, budgetCfg(0.2, 1024, 0), 188_000)
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

// ANSWER ROOM: the injected budget leaves at least the reserve under
// max_tokens, and only corrallm's value moves.
func TestInjectedBudgetLeavesAnswerReserve(t *testing.T) {
	cfg := budgetCfg(0.2, 2048, 8192) // 188k context: 8192 before the clamp
	body := []byte(`{"model":"m","max_tokens":6000,"messages":[]}`)
	out, did, clamp := applyReasoningBudget(body, cfg, 188_000)
	got, ok := budgetOf(t, out)
	if !did || !ok || got != 6000-config.DefaultAnswerReserve {
		t.Fatalf("budget = %v (did %v), want %d: %s", got, did, 6000-config.DefaultAnswerReserve, out)
	}
	if clamp == nil || clamp.From != 8192 || clamp.To != 3952 || clamp.MaxTokens != 6000 {
		t.Errorf("clamp = %+v", clamp)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["max_tokens"].(float64) != 6000 {
		t.Errorf("max_tokens changed: %s", out)
	}
	// Room enough: no clamp.
	out, _, clamp = applyReasoningBudget([]byte(`{"model":"m","max_tokens":20000,"messages":[]}`), cfg, 188_000)
	if got, _ := budgetOf(t, out); got != 8192 || clamp != nil {
		t.Errorf("budget = %v clamp %+v, want 8192 unclamped", got, clamp)
	}
	// No max_tokens: nothing to keep room under.
	out, _, clamp = applyReasoningBudget([]byte(`{"model":"m","messages":[]}`), cfg, 188_000)
	if got, _ := budgetOf(t, out); got != 8192 || clamp != nil {
		t.Errorf("budget = %v clamp %+v without max_tokens", got, clamp)
	}
	// max_tokens at or under the reserve: no budget at all, as before.
	if out, did, _ := applyReasoningBudget([]byte(`{"model":"m","max_tokens":2048,"messages":[]}`), cfg, 188_000); did {
		t.Errorf("injected a budget that leaves no room: %s", out)
	}
	// A configured reserve, and a negative one turning it off.
	cfg.ReasoningBudget.AnswerReserve = 1000
	out, _, _ = applyReasoningBudget(body, cfg, 188_000)
	if got, _ := budgetOf(t, out); got != 5000 {
		t.Errorf("budget = %v, want 5000 with answerReserve 1000", got)
	}
	cfg.ReasoningBudget.AnswerReserve = -1
	out, _, clamp = applyReasoningBudget(body, cfg, 188_000)
	if got, _ := budgetOf(t, out); got != 6000 || clamp == nil {
		t.Errorf("budget = %v, want 6000 (max_tokens) with the reserve off", got)
	}
}

// A caller's own budget that leaves too little is reported, never changed.
func TestCallerBudgetShortIsReportedNotChanged(t *testing.T) {
	cfg := budgetCfg(0.2, 2048, 8192)
	cases := map[string]struct {
		left  int
		short bool
	}{
		`{"reasoning_budget_tokens":3000,"max_tokens":4000}`:            {1000, true},
		`{"reasoning_budget_tokens":3000,"max_completion_tokens":4000}`: {1000, true},
		`{"reasoning_budget_tokens":1000,"max_tokens":4000}`:            {0, false},
		`{"reasoning_budget_tokens":0,"max_tokens":100}`:                {0, false}, // 0 = do not think
		`{"reasoning_budget_tokens":3000}`:                              {0, false},
		`{"max_tokens":100}`:                                            {0, false},
	}
	for in, want := range cases {
		left, short := callerAnswerRoom([]byte(in), cfg)
		if left != want.left || short != want.short {
			t.Errorf("%s: got (%d, %v), want %+v", in, left, short, want)
		}
	}
	// Default reserve with no budget config at all.
	if _, short := callerAnswerRoom([]byte(`{"reasoning_budget_tokens":3000,"max_tokens":4000}`), &config.SamplingConfig{}); !short {
		t.Error("no default reserve without a reasoningBudget config")
	}

	p := &Proxy{}
	body := []byte(`{"model":"m","reasoning_budget_tokens":3000,"max_tokens":4000,"messages":[]}`)
	out, notes := p.withSampling(body, config.Model{Sampling: cfg}, "m", config.Caller{Key: "k"})
	if b, _ := budgetOf(t, out); b != 3000 || !strings.Contains(string(out), `"max_tokens":4000`) {
		t.Errorf("caller's request changed: %s", out)
	}
	if strings.Join(notes, ";") != "reasoning-budget-short answer_room=1000" {
		t.Errorf("notes = %q", notes)
	}
}

func TestWithSamplingNotesTheClamp(t *testing.T) {
	p := &Proxy{}
	body := []byte(`{"model":"m","max_tokens":6000,"messages":[]}`)
	_, notes := p.withSampling(body, config.Model{Sampling: budgetCfg(0.2, 2048, 8192), ContextPerRequest: 188_000}, "m", config.Caller{Key: "k"})
	if strings.Join(notes, ";") != "reasoning-budget-clamped from=8192 to=3952" {
		t.Errorf("notes = %q", notes)
	}
}

func TestBudgetLogIsRateLimitedPerKeyAndModel(t *testing.T) {
	p := &Proxy{}
	if !p.budgetLogDue("short", "k", "m") || p.budgetLogDue("short", "k", "m") {
		t.Error("second line within the window was not suppressed")
	}
	if !p.budgetLogDue("short", "k2", "m") || !p.budgetLogDue("short", "k", "m2") || !p.budgetLogDue("clamp", "k", "m") {
		t.Error("a different key, model or kind was suppressed")
	}
}
