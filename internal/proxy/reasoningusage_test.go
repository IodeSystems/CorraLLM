package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

type reasoningUsage struct {
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	Cost *float64 `json:"cost"`
}

func reasoningOfBody(t *testing.T, data string) *int {
	t.Helper()
	var r struct {
		Usage reasoningUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		t.Fatalf("not JSON: %v: %s", err, data)
	}
	if r.Usage.Cost == nil {
		t.Errorf("usage.cost lost: %s", data)
	}
	if r.Usage.CompletionTokensDetails == nil {
		return nil
	}
	return r.Usage.CompletionTokensDetails.ReasoningTokens
}

// The caller's usage carries the reasoning count the activity row stores
// (both come from statusCapture.thinkingTokens).
func TestReasoningTokensReturnedNonStreaming(t *testing.T) {
	rec, _ := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"choices":[{"message":{"content":"abcd","reasoning_content":"abcdefghijkl"}}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":16},"timings":{"predicted_per_second":50}}`)
	got := reasoningOfBody(t, rec.Body.String())
	// 16 × 12/16 of the characters: statusCapture.thinkingTokens, the row's count.
	if got == nil || *got != 12 {
		t.Errorf("reasoning_tokens = %v, want 12: %s", got, rec.Body.String())
	}
}

func TestReasoningTokensReturnedStreaming(t *testing.T) {
	think := `data: {"choices":[{"delta":{"reasoning_content":"x"}}]}` + "\n\n"
	answer := `data: {"choices":[{"delta":{"content":"y"}}]}` + "\n\n"
	rec, _ := costCase(t, nil, `{"model":"mock","stream":true}`, "text/event-stream", 200,
		think, think, think, answer,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4},"timings":{"predicted_per_second":50}}`+"\n\n",
		"data: [DONE]\n\n")
	var got *int
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(data, `"usage"`) {
			got = reasoningOfBody(t, data)
		}
	}
	// One reasoning event per token: the row's count.
	if got == nil || *got != 3 {
		t.Errorf("reasoning_tokens = %v, want 3: %s", got, rec.Body.String())
	}
}

// Not measured (no llama.cpp timings): nothing added, rather than a 0 that
// would claim a measurement.
func TestReasoningTokensAbsentWhenUnmeasured(t *testing.T) {
	rec, _ := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"choices":[{"message":{"content":"hi","reasoning_content":"hmm"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	if got := reasoningOfBody(t, rec.Body.String()); got != nil {
		t.Errorf("added reasoning_tokens %d without a measurement", *got)
	}
}

// An upstream's own count stands.
func TestReasoningTokensKeepsProviderCount(t *testing.T) {
	rec, _ := costCase(t, nil, `{"model":"mock"}`, "application/json", 200,
		`{"choices":[{"message":{"content":"a","reasoning_content":"bbbb"}}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}},"timings":{"predicted_per_second":50}}`)
	if got := reasoningOfBody(t, rec.Body.String()); got == nil || *got != 2 {
		t.Errorf("reasoning_tokens = %v, want the provider's 2: %s", got, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "reasoning_tokens"); n != 1 {
		t.Errorf("reasoning_tokens appears %d times: %s", n, rec.Body.String())
	}
}

// An existing completion_tokens_details object gets the field inside it.
func TestWithCostNestsReasoningTokens(t *testing.T) {
	c := &costWriter{price: func(usage) (float64, string) { return 0.5, costFromPrices },
		reasoning: func(usage) (int, bool) { return 7, true }}
	for in, want := range map[string]string{
		`{"usage":{"completion_tokens_details":{}}}`:                 `{"usage":{"completion_tokens_details":{"reasoning_tokens":7},"cost":0.5,"cost_source":"price-table"}}`,
		`{"usage":{"completion_tokens_details":{"audio_tokens":0}}}`: `{"usage":{"completion_tokens_details":{"audio_tokens":0,"reasoning_tokens":7},"cost":0.5,"cost_source":"price-table"}}`,
		`{"usage":{}}`: `{"usage":{"cost":0.5,"cost_source":"price-table","completion_tokens_details":{"reasoning_tokens":7}}}`,
	} {
		b := []byte(in)
		u, _ := usageIn(b)
		if got := string(c.withCost(b, u)); got != want {
			t.Errorf("withCost(%s)\n got %s\nwant %s", in, got, want)
		}
	}
}
