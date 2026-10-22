package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpstreamSpeedsFromFireworksHeaders(t *testing.T) {
	// Non-streamed Fireworks reply, measured 2026-10-20: ttft 0.399 s, total 0.950 s.
	h := http.Header{}
	h.Set("Fireworks-Server-Time-To-First-Token", "0.400")
	h.Set("Fireworks-Server-Processing-Time", "0.900")
	u := usage{PromptTokens: 1000, CachedTokens: 200, CompletionTokens: 100}
	got, src := upstreamSpeeds(u, h, false, time.Time{}, time.Time{}, time.Time{})
	if src != speedProvider {
		t.Fatalf("source = %q, want provider", src)
	}
	if got.PromptPerSec != 2000 { // 800 uncached / 0.4 s
		t.Errorf("prefill = %v, want 2000", got.PromptPerSec)
	}
	if got.PredictedPerSec != 200 { // 100 / 0.5 s
		t.Errorf("decode = %v, want 200", got.PredictedPerSec)
	}
}

func TestUpstreamSpeedsFromProxyClockWhenStreaming(t *testing.T) {
	start := time.Unix(100, 0)
	first := start.Add(500 * time.Millisecond)
	last := first.Add(2 * time.Second)
	u := usage{PromptTokens: 1000, CompletionTokens: 101}
	got, src := upstreamSpeeds(u, http.Header{}, true, start, first, last)
	if src != speedProxy {
		t.Fatalf("source = %q, want proxy", src)
	}
	if got.PromptPerSec != 2000 || got.PredictedPerSec != 50 {
		t.Errorf("speeds = %v / %v, want 2000 / 50", got.PromptPerSec, got.PredictedPerSec)
	}
}

func TestUpstreamSpeedsUnknownForNonStreamedWithoutHeaders(t *testing.T) {
	start := time.Unix(100, 0)
	u := usage{PromptTokens: 1000, CompletionTokens: 100}
	got, src := upstreamSpeeds(u, http.Header{}, false, start, start.Add(time.Second), start.Add(time.Second))
	if src != "" || got.PromptPerSec != 0 || got.PredictedPerSec != 0 {
		t.Errorf("got %v/%v source %q, want nothing measured", got.PromptPerSec, got.PredictedPerSec, src)
	}
}

func TestKeptHeadersAllowlist(t *testing.T) {
	h := http.Header{}
	h.Set("Fireworks-Prompt-Tokens", "37")
	h.Set("X-Ratelimit-Remaining-Requests", "59")
	h.Set("Set-Cookie", "session=secret")
	h.Set("Content-Type", "application/json")
	want := "fireworks-prompt-tokens: 37\nx-ratelimit-remaining-requests: 59"
	if got := keptHeaders(h); got != want {
		t.Errorf("keptHeaders = %q, want %q", got, want)
	}
}

// TestUpstreamSpeedsEmbeddingIsAllPrefill: an embedding generates nothing, so
// its time to first byte is its prompt processing.
func TestUpstreamSpeedsEmbeddingIsAllPrefill(t *testing.T) {
	start := time.Unix(100, 0)
	u := usage{PromptTokens: 2000}
	got, src := upstreamSpeeds(u, http.Header{}, false, start, start.Add(500*time.Millisecond), start.Add(time.Second))
	if src != speedProxy || got.PromptPerSec != 4000 {
		t.Errorf("got %v tok/s source %q, want 4000 from %q", got.PromptPerSec, src, speedProxy)
	}
}

// TestUsageFromTailOfOverCapEmbedding: llama.cpp puts "usage" after "data", so
// a big batch's usage lies past the capture cap.
func TestUsageFromTailOfOverCapEmbedding(t *testing.T) {
	sc := &statusCapture{ResponseWriter: httptest.NewRecorder(), bufLimit: 64}
	body := `{"data":[{"embedding":[` + strings.Repeat("0.123456,", 2000) + `0.1],"index":0}],"model":"m","object":"list","usage":{"prompt_tokens":812,"total_tokens":812}}`
	_, _ = sc.Write([]byte(body[:100]))
	_, _ = sc.Write([]byte(body[100:]))
	if u := extractUsage(sc.buf, false); u.PromptTokens != 0 {
		t.Fatalf("head parse found %d tokens; the test needs usage past the cap", u.PromptTokens)
	}
	if u := usageFromTail(sc.tail); u.PromptTokens != 812 {
		t.Errorf("usage from tail = %+v, want prompt_tokens 812", u)
	}
}
