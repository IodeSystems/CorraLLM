package proxy

import (
	"net/http"
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
