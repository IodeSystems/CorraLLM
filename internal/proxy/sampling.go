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
func applySamplingProfile(body []byte, cfg *config.SamplingConfig) ([]byte, bool) {
	if cfg == nil {
		return body, false
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		// Not an object we understand. Forwarding it untouched is right: this
		// rewrite is an improvement, never a precondition for serving.
		return body, false
	}

	think, stated := requestWantsThinking(req)
	if !stated {
		think = cfg.DefaultThinking()
	}
	prof := cfg.ProfileFor(think)
	if prof.Empty() {
		return body, false
	}

	changed := false
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
