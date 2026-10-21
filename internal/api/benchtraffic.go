package api

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// BenchTrafficView is the proxy's record of one model's bench run: what went
// over the wire, as the backend billed and timed it.
type BenchTrafficView struct {
	Turns    int `json:"turns" doc:"Model calls answered 200."`
	Requests int `json:"requests" doc:"Every request, refused ones included."`
	Refused  int `json:"refused" doc:"Requests not answered 200 (429s, errors)."`

	ContextTokens   int64   `json:"contextTokens" doc:"Total prompt tokens sent, cache hits included: the summed context length of every turn."`
	CachedTokens    int64   `json:"cachedTokens"`
	CachePct        float64 `json:"cachePct" doc:"Cached ÷ context tokens, 0..1."`
	ProcessedTokens int64   `json:"processedTokens" doc:"Prompt tokens actually evaluated (context minus cache hits)."`
	GeneratedTokens int64   `json:"generatedTokens"`
	ThinkingTokens  int64   `json:"thinkingTokens" doc:"Reasoning tokens, summed over the turns where they were measured."`
	ThinkingTurns   int     `json:"thinkingTurns" doc:"Turns with a thinking count; fewer than turns means the sum is partial."`
	ThinkingPct     float64 `json:"thinkingPct" doc:"Thinking ÷ generated tokens over the measured turns, 0..1."`
	PeakContext     int64   `json:"peakContext" doc:"Largest prompt + completion of a single turn."`
	QueuedMS        int64   `json:"queuedMs"`

	TPS  float64 `json:"tps" doc:"Prompt processing: uncached prompt tokens per second of prefill."`
	TFPS float64 `json:"tfps" doc:"Fused prompt rate: all prompt tokens, cache hits included, per second of prefill."`
	TGS  float64 `json:"tgs" doc:"Generation tokens per second."`

	ThinkingLevel string `json:"thinkingLevel" doc:"What the requests asked for (reasoning_effort, thinking, enable_thinking), else the launch flags, else 'model default'."`
}

// benchRunStart reads a run id (llm-bench's local-time YYYYMMDD-HHMMSS).
func benchRunStart(runID string) (time.Time, bool) {
	t, err := time.ParseInLocation("20060102-150405", runID, time.Local)
	return t, err == nil
}

// benchTraffic rolls up the proxy's rows for one bench result. The window is
// the run id (its start) to the result's publish time (its end).
func (h *Handlers) benchTraffic(ctx context.Context, r store.BenchResult) *BenchTrafficView {
	start, ok := benchRunStart(r.RunID)
	if !ok || h.Store == nil {
		return nil
	}
	from, to := start.UnixMilli(), r.At*1000+5000
	ru, err := h.Store.BenchRollupFor(ctx, r.Model, from, to)
	if err != nil || ru.Requests == 0 {
		return nil
	}
	v := &BenchTrafficView{
		Turns: ru.Turns, Requests: ru.Requests, Refused: ru.Refused,
		ContextTokens: ru.Prompt, CachedTokens: ru.Cached,
		ProcessedTokens: max(ru.Prompt-ru.Cached, 0), GeneratedTokens: ru.Generated,
		ThinkingTokens: ru.Thinking, ThinkingTurns: ru.ThinkingRows,
		PeakContext: ru.PeakCtx, QueuedMS: ru.QueuedMS,
	}
	if ru.Prompt > 0 {
		v.CachePct = float64(ru.Cached) / float64(ru.Prompt)
	}
	if ru.Generated > 0 && ru.ThinkingRows > 0 {
		v.ThinkingPct = float64(ru.Thinking) / float64(ru.Generated)
	}
	if ru.PrefillS > 0 {
		v.TPS = float64(ru.PrefillNew) / ru.PrefillS
		v.TFPS = float64(ru.PrefillAll) / ru.PrefillS
	}
	if ru.GenS > 0 {
		v.TGS = float64(ru.GenTokens) / ru.GenS
	}
	body, _ := h.Store.BenchRequestSample(ctx, r.Model, from, to)
	cmd := ""
	if cfg := h.config(); cfg != nil {
		cmd = cfg.Models[r.Model].Cmd
	}
	v.ThinkingLevel = thinkingLevel(body, cmd)
	return v
}

var (
	reasoningBudgetFlag = regexp.MustCompile(`--reasoning-budget[= ]+(-?\d+)`)
	enableThinkingOff   = regexp.MustCompile(`"?enable_thinking"?\s*:\s*false`)
)

// thinkingLevel names the thinking a run asked for: the request's own
// parameters first, then the backend's launch flags.
func thinkingLevel(reqBody, cmd string) string {
	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		EnableThinking     *bool          `json:"enable_thinking"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	}
	if reqBody != "" && json.Unmarshal([]byte(reqBody), &req) == nil {
		switch {
		case req.ReasoningEffort != "":
			return "effort " + req.ReasoningEffort
		case req.Reasoning != nil && req.Reasoning.Effort != "":
			return "effort " + req.Reasoning.Effort
		case req.Thinking != nil && req.Thinking.Type == "disabled":
			return "off"
		case req.Thinking != nil && req.Thinking.BudgetTokens > 0:
			return fmt.Sprintf("budget %d tokens", req.Thinking.BudgetTokens)
		case req.EnableThinking != nil:
			return onOff(*req.EnableThinking)
		}
		if b, ok := req.ChatTemplateKwargs["enable_thinking"].(bool); ok {
			return onOff(b)
		}
	}
	if m := reasoningBudgetFlag.FindStringSubmatch(cmd); m != nil {
		switch m[1] {
		case "0":
			return "off (--reasoning-budget 0)"
		case "-1":
			return "unlimited (--reasoning-budget -1)"
		default:
			return "budget " + m[1] + " tokens (--reasoning-budget)"
		}
	}
	if strings.Contains(cmd, "enable_thinking") && enableThinkingOff.MatchString(cmd) {
		return "off (enable_thinking false)"
	}
	return "model default"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
