package proxy

import (
	"encoding/json"

	"github.com/iodesystems/corrallm/internal/config"
)

// Per-mode sampler substitution.
//
// A reasoning model has two right samplers and llama-server can only be launched
// with one, while the caller may flip the mode on any single request
// (chat_template_kwargs.enable_thinking overrides --reasoning). The result is a
// model sampled for the mode it is NOT in, which degrades output silently rather
// than erroring — nothing in the response says the sampler was wrong.
//
// Corrallm is the only place that can fix it: it knows which model a name
// actually resolved to (after aliases, lanes and globs) and it holds that
// model's card values. Doing it in each client means each client reimplements
// it, and each gets it subtly different.

// requestWantsThinking reads the request's own opinion about thinking, and
// ok=false when it expresses none.
//
// The four spellings are not redundant — they come from different dialects that
// all reach this endpoint. chat_template_kwargs is llama.cpp's passthrough to
// the Jinja template; reasoning_effort is OpenAI's; thinking is Anthropic's; and
// reasoning_budget_tokens is llama.cpp's budget sampler, where 0 means "end
// reasoning immediately" and is therefore a request NOT to think.
func requestWantsThinking(req map[string]any) (think bool, ok bool) {
	if kw, isMap := req["chat_template_kwargs"].(map[string]any); isMap {
		if v, present := kw["enable_thinking"]; present {
			if b, isBool := v.(bool); isBool {
				return b, true
			}
		}
	}
	if v, present := req["reasoning_effort"]; present {
		if s, isStr := v.(string); isStr {
			// "none" is OpenAI's off switch; every other effort level is a
			// request to think, harder or less hard.
			return s != "none", true
		}
	}
	if th, isMap := req["thinking"].(map[string]any); isMap {
		if t, isStr := th["type"].(string); isStr {
			return t == "enabled", true
		}
	}
	if v, present := req["reasoning_budget_tokens"]; present {
		if n, isNum := v.(float64); isNum {
			// A negative budget disables the budget sampler, not thinking, so it
			// says nothing about the mode. Zero ends reasoning at once, which
			// does.
			if n == 0 {
				return false, true
			}
			if n > 0 {
				return true, true
			}
		}
	}
	return false, false
}

// applySamplingProfile substitutes the model's per-mode sampler into a chat
// request body, returning the new body and whether anything changed.
//
// THE CALLER ALWAYS WINS. Only fields absent from the request are filled in.
// Overriding a field the caller sent would make `temperature: 0` unpinnable,
// and reproducible measurement depends on exactly that — llm-bench's
// --pin-sampling sends temperature 0 and a seed precisely so a probe is not a
// coin flip, and a proxy that overwrote it would silently restore the coin flip.
func applySamplingProfile(body []byte, cfg *config.SamplingConfig, caller config.KeyPolicy) ([]byte, bool) {
	if cfg == nil {
		return body, false
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		// Not an object we understand. Forwarding it untouched is right: this
		// rewrite is an improvement, never a precondition for serving.
		return body, false
	}

	changed := false
	think, stated := requestWantsThinking(req)
	if !stated {
		if pref, set := caller.ThinkingPreference(); set {
			// THE BACKEND HAS TO BE TOLD, not just the sampler chosen.
			//
			// llama-server was launched with one mode, and picking the other
			// profile here would sample for a mode the model is not in — the
			// exact silent degradation this file exists to prevent, arrived at
			// from the opposite direction. Writing the request's own switch
			// makes the two agree whichever way the process was started, and
			// leaves the body self-describing for applyReasoningBudget, which
			// reads the same field rather than re-deriving the decision.
			think = pref
			kw, _ := req["chat_template_kwargs"].(map[string]any)
			if kw == nil {
				kw = map[string]any{}
			}
			kw["enable_thinking"] = think
			req["chat_template_kwargs"] = kw
			changed = true
		} else {
			think = cfg.DefaultThinking()
		}
	}
	prof := cfg.ProfileFor(think)
	if prof.Empty() && !changed {
		return body, false
	}

	set := func(key string, val any) {
		if val == nil {
			return
		}
		if _, present := req[key]; present {
			return // the caller has an opinion; it stands
		}
		switch v := val.(type) {
		case *float64:
			if v == nil {
				return
			}
			req[key] = *v
		case *int:
			if v == nil {
				return
			}
			req[key] = *v
		default:
			return
		}
		changed = true
	}
	set("temperature", prof.Temperature)
	set("top_p", prof.TopP)
	set("top_k", prof.TopK)
	set("min_p", prof.MinP)
	set("presence_penalty", prof.PresencePenalty)
	set("frequency_penalty", prof.FrequencyPenalty)
	set("repeat_penalty", prof.RepeatPenalty)

	if !changed {
		return body, false
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// bytesPerTokenEstimate is how the proxy guesses a prompt's token count without
// tokenising it.
//
// corrallm has not tokenised anything at this point and will not: the tokenizer
// lives in the backend, so an exact count costs a round trip to the very process
// this request is queued for — latency added to every request to size a cap.
// Four bytes per token is the same approximation slotcache already uses.
//
// It errs the SAFE way for this purpose. English is near 4, code and JSON are
// nearer 3, so a byte count divided by 4 UNDER-estimates tokens on exactly the
// traffic that dominates here — which over-estimates what is left, which makes
// the budget too generous rather than too tight. A generous cap costs some
// latency; a tight one truncates a thought and looks like a stupid model.
const bytesPerTokenEstimate = 4

// reasoningBudgetFor sizes a thinking budget from what the prompt has left over.
//
// Returns 0 when no budget should be set, which is not the same as a budget of
// zero: llama.cpp reads 0 as "end reasoning immediately".
func reasoningBudgetFor(bodyBytes, contextTokens int, cfg *config.ReasoningBudget) int {
	if cfg == nil || cfg.FractionOfRemaining <= 0 || contextTokens <= 0 {
		return 0
	}
	promptEst := bodyBytes / bytesPerTokenEstimate
	remaining := contextTokens - promptEst
	if remaining <= 0 {
		// The prompt is already at or past the window. Nothing to divide, and
		// the context limit will refuse this request on its own terms with a
		// better message than a zero budget would produce.
		return 0
	}
	budget := int(float64(remaining) * cfg.FractionOfRemaining)
	if cfg.Max > 0 && budget > cfg.Max {
		budget = cfg.Max
	}
	if budget < cfg.Min {
		// Deliberately unrestricted rather than clamped UP to Min: raising it to
		// the floor would hand a nearly-full window a budget it cannot afford,
		// which is the opposite of what this is for.
		return 0
	}
	return budget
}

// applyReasoningBudget fills in reasoning_budget_tokens when the request is
// going to think and did not say how long for.
//
// Called after applySamplingProfile and given the same treatment: THE CALLER
// ALWAYS WINS. A caller that sent its own budget — including 0, meaning "do not
// think" — keeps it.
//
// Only ever set when thinking is the resolved mode. The value is itself a
// thinking signal to llama.cpp (a positive budget implies reasoning), so writing
// one onto an instruct request would turn thinking ON for a caller who never
// asked, which is a behaviour change wearing the costume of a limit.
func applyReasoningBudget(body []byte, cfg *config.SamplingConfig, contextTokens int) ([]byte, bool) {
	if cfg == nil || cfg.ReasoningBudget == nil {
		return body, false
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, false
	}
	if _, present := req["reasoning_budget_tokens"]; present {
		return body, false
	}
	think, stated := requestWantsThinking(req)
	if !stated {
		think = cfg.DefaultThinking()
	}
	if !think {
		return body, false
	}
	budget := reasoningBudgetFor(len(body), contextTokens, cfg.ReasoningBudget)
	if budget <= 0 {
		return body, false
	}
	req["reasoning_budget_tokens"] = budget
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}
