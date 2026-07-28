package proxy

import "testing"

// The distinction that matters: "stop" means the model chose to end, "length"
// means it hit a cap and did NOT finish. Both are HTTP 200, so status alone
// cannot tell them apart — which is why a runaway generation looked like a
// successful slow one.
func TestExtractFinishReason_NonStreaming(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{}}`)
	if got := extractFinishReason(body, false); got != "stop" {
		t.Errorf("got %q, want stop", got)
	}
	cut := []byte(`{"choices":[{"index":0,"message":{"content":"..."},"finish_reason":"length"}]}`)
	if got := extractFinishReason(cut, false); got != "length" {
		t.Errorf("got %q, want length", got)
	}
}

// A stream carries it on the last event that has one — the final delta before
// [DONE]. Earlier deltas carry null, which must not be mistaken for an answer.
func TestExtractFinishReason_Streaming(t *testing.T) {
	sse := []byte(`data: {"choices":[{"delta":{"content":"a"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"b"},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: [DONE]
`)
	if got := extractFinishReason(sse, true); got != "length" {
		t.Errorf("got %q, want length from the final event", got)
	}
}

// Absent, unparseable, or truncated past the capture cap must yield empty
// rather than a guess — an invented "stop" would hide the very case this is for.
func TestExtractFinishReason_MissingIsEmpty(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":       nil,
		"no choices":  []byte(`{"usage":{"prompt_tokens":1}}`),
		"truncated":   []byte(`{"choices":[{"message":{"content":"aaaa`),
		"null reason": []byte(`{"choices":[{"finish_reason":null}]}`),
	} {
		if got := extractFinishReason(in, false); got != "" {
			t.Errorf("%s: got %q, want empty", name, got)
		}
	}
	if got := extractFinishReason([]byte("data: [DONE]\n"), true); got != "" {
		t.Errorf("stream with no reason: got %q, want empty", got)
	}
}
