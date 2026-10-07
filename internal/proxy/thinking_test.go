package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const thinkEvent = `data: {"choices":[{"index":0,"delta":{"reasoning_content":"x"}}]}` + "\n\n"
const answerEvent = `data: {"choices":[{"index":0,"delta":{"content":"y"}}]}` + "\n\n"
const nullThink = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}` + "\n\n"

// A marker cut in half by the transport must still count once.
func TestThinkingCountedAcrossSplitWrites(t *testing.T) {
	stream := nullThink + strings.Repeat(thinkEvent, 40) + strings.Repeat(answerEvent, 10)
	sc := &statusCapture{ResponseWriter: httptest.NewRecorder(), streaming: true}
	for i := 0; i < len(stream); i += 7 { // 7-byte writes split every marker somewhere
		_, _ = sc.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	n, ok := sc.thinkingTokens(usage{CompletionTokens: 53, PredictedPerSec: 90})
	if !ok || n != 40 {
		t.Errorf("thinking = %d (measured %v), want 40", n, ok)
	}
}

// Without llama.cpp timings the reply is another backend's: unknown, not 0.
func TestThinkingUnknownWithoutLlamaTimings(t *testing.T) {
	sc := &statusCapture{ResponseWriter: httptest.NewRecorder(), streaming: true}
	_, _ = sc.Write([]byte(answerEvent))
	if _, ok := sc.thinkingTokens(usage{CompletionTokens: 5}); ok {
		t.Error("claimed a thinking measurement for a backend that reports no timings")
	}
}

// Thinking off is a MEASURED zero.
func TestNoThinkingIsMeasuredZero(t *testing.T) {
	sc := &statusCapture{ResponseWriter: httptest.NewRecorder(), streaming: true}
	_, _ = sc.Write([]byte(strings.Repeat(answerEvent, 5)))
	if n, ok := sc.thinkingTokens(usage{CompletionTokens: 5, PredictedPerSec: 80}); !ok || n != 0 {
		t.Errorf("thinking = %d (measured %v), want a measured 0", n, ok)
	}
}

// Non-streaming has no per-token events: the reasoning text's share of the
// characters, applied to completion_tokens.
func TestThinkingNonStreamingIsTheCharacterShare(t *testing.T) {
	body := `{"choices":[{"message":{"reasoning_content":"` + strings.Repeat("r", 300) + `","content":"` + strings.Repeat("c", 100) + `"}}]}`
	sc := &statusCapture{ResponseWriter: httptest.NewRecorder()}
	_, _ = sc.Write([]byte(body))
	if n, ok := sc.thinkingTokens(usage{CompletionTokens: 80, PredictedPerSec: 80}); !ok || n != 60 {
		t.Errorf("thinking = %d (measured %v), want 60 (3/4 of 80)", n, ok)
	}
}
