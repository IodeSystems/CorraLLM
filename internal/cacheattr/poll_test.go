package cacheattr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A backend with 0005's counters is read; one without (/metrics 404, or no
// prompt-cache lines) reads as "not exposed", -1, and is not asked again.
func TestPollerReadsCountersOrFallsBack(t *testing.T) {
	var evictions atomic.Int64
	with := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "# HELP x\nllamacpp:prompt_cache_entries 12\nllamacpp:prompt_cache_bytes{slot=\"x\"} 3.5e+09\nllamacpp:prompt_cache_evictions_total %d\n", evictions.Load())
	}))
	defer with.Close()
	var asked404 atomic.Int64
	without := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked404.Add(1)
		http.NotFound(w, nil)
	}))
	defer without.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("llamacpp:prompt_tokens_total 5\n"))
	}))
	defer plain.Close()

	p := NewPoller(func() []Target {
		return []Target{{"swift", "a", with.URL}, {"swift", "b", without.URL}, {"other", "c", plain.URL}}
	}, 0)
	ctx := context.Background()
	p.PollOnce(ctx)
	evictions.Store(5)
	p.PollOnce(ctx)
	p.PollOnce(ctx)

	ring := p.samples["a"]
	if len(ring) != 3 || ring[0].entries != 12 || ring[0].bytes != 3_500_000_000 || ring[2].evictions != 5 {
		t.Fatalf("samples = %+v", ring)
	}
	for i := range ring { // three reads inside one millisecond: spread them
		ring[i].ts = int64(1000 * (i + 1))
	}
	if got := p.EvictionsBetween("a", ring[0].ts, ring[2].ts); got != 5 {
		t.Errorf("evictions between = %d, want 5", got)
	}
	if got := p.EvictionsBetween("a", ring[0].ts-1, ring[2].ts); got != -1 {
		t.Errorf("no sample before the window: %d, want -1", got)
	}
	if got := p.EvictionsBetween("b", 0, 1<<62); got != -1 {
		t.Errorf("404 backend: %d, want -1", got)
	}
	if asked404.Load() != 1 {
		t.Errorf("a 404 backend was asked %d times, want once", asked404.Load())
	}
	if !p.absent["c"] {
		t.Error("a /metrics without prompt-cache lines should read as not exposed")
	}
	entries, bytes, _, ok := p.Latest("swift")
	if !ok || entries != 12 || bytes != 3_500_000_000 {
		t.Errorf("latest = %d %d %v", entries, bytes, ok)
	}
	if _, _, _, ok := p.Latest("other"); ok {
		t.Error("an instance with no counters reported some")
	}
}

func TestParseMetricsIgnoresOtherLines(t *testing.T) {
	s, ok := parseMetrics(strings.NewReader("# TYPE a counter\nfoo 1\nllamacpp:prompt_cache_evictions 7\n"))
	if !ok || s.evictions != 7 || s.entries != -1 {
		t.Errorf("%+v %v", s, ok)
	}
}
