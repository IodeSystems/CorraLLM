package quota

import (
	"net/http"
	"testing"
	"time"
)

// A fixed clock so ResetsAt/CoolingUntil are assertable.
func fixed(l *Ledger, t0 time.Time) { l.now = func() time.Time { return t0 } }

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// The exact headers the live Groq smoke test returned, including the Go-duration
// reset format — the detail the research got wrong.
func TestObserveResponse_GroqHeaders(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.ObserveResponse("groq-a", 200, hdr(
		"X-Ratelimit-Limit-Requests", "1000",
		"X-Ratelimit-Remaining-Requests", "999",
		"X-Ratelimit-Reset-Requests", "1m26.4s",
		"X-Ratelimit-Limit-Tokens", "12000",
		"X-Ratelimit-Remaining-Tokens", "11938",
		"X-Ratelimit-Reset-Tokens", "310ms",
	))
	e := l.Snapshot()
	if len(e) != 1 {
		t.Fatalf("want 1 entry, got %d", len(e))
	}
	g := e[0]
	if g.Requests.Limit != 1000 || g.Requests.Remaining != 999 {
		t.Errorf("requests bucket wrong: %+v", g.Requests)
	}
	if g.Tokens.Remaining != 11938 {
		t.Errorf("tokens remaining wrong: %+v", g.Tokens)
	}
	// The reset must parse as a DURATION, not be dropped: 1m26.4s = 86.4s.
	if want := t0.Add(86400 * time.Millisecond); !g.Requests.ResetsAt.Equal(want) {
		t.Errorf("requests reset = %v, want %v (duration parse)", g.Requests.ResetsAt, want)
	}
	if want := t0.Add(310 * time.Millisecond); !g.Tokens.ResetsAt.Equal(want) {
		t.Errorf("tokens reset = %v, want %v", g.Tokens.ResetsAt, want)
	}
	if g.Seen != 1 {
		t.Errorf("Seen = %d, want 1", g.Seen)
	}
}

// A local backend (no rate-limit headers, 200) must not create an entry.
func TestObserveResponse_NoHeadersIsNoop(t *testing.T) {
	l := New()
	l.ObserveResponse("Qwen3-6-27B-MPT", 200, hdr("Content-Type", "application/json"))
	if len(l.Snapshot()) != 0 {
		t.Error("a local backend with no rate-limit headers must not be tracked")
	}
}

// Available reflects budget + cooling.
func TestAvailable(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)

	// Unknown backend → optimistically available.
	if !l.Available("never-seen") {
		t.Error("unknown backend should be available")
	}

	// Has budget → available.
	l.ObserveResponse("groq-a", 200, hdr(
		"X-Ratelimit-Limit-Requests", "1000", "X-Ratelimit-Remaining-Requests", "5",
		"X-Ratelimit-Reset-Requests", "60s"))
	if !l.Available("groq-a") {
		t.Error("backend with remaining budget should be available")
	}

	// Exhausted (remaining 0, not yet reset) → unavailable.
	l.ObserveResponse("groq-a", 200, hdr(
		"X-Ratelimit-Limit-Requests", "1000", "X-Ratelimit-Remaining-Requests", "0",
		"X-Ratelimit-Reset-Requests", "60s"))
	if l.Available("groq-a") {
		t.Error("exhausted backend should be unavailable until reset")
	}
	// After the reset window, available again.
	l.now = func() time.Time { return t0.Add(61 * time.Second) }
	if !l.Available("groq-a") {
		t.Error("backend should recover after reset")
	}
}

// A 429 sets cooling from Retry-After and clears once it passes.
func TestObserve429_Cooling(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.ObserveResponse("groq-a", http.StatusTooManyRequests, hdr("Retry-After", "30"))
	if l.Available("groq-a") {
		t.Error("a 429 with Retry-After should put the backend in cooling")
	}
	if got := l.Snapshot()[0].CoolingUntil; !got.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("coolingUntil = %v, want +30s", got)
	}
	l.now = func() time.Time { return t0.Add(31 * time.Second) }
	if !l.Available("groq-a") {
		t.Error("cooling should clear after Retry-After elapses")
	}
}

// Retry-After as a duration string (some providers) parses too.
func TestObserve429_RetryAfterDuration(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.ObserveResponse("x", http.StatusTooManyRequests, hdr("Retry-After", "1m30s"))
	if got := l.Snapshot()[0].CoolingUntil; !got.Equal(t0.Add(90 * time.Second)) {
		t.Errorf("coolingUntil = %v, want +90s", got)
	}
}

// A 429 with no Retry-After falls back to the exhausted bucket's reset.
func TestObserve429_FallsBackToReset(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.ObserveResponse("x", http.StatusTooManyRequests, hdr(
		"X-Ratelimit-Limit-Requests", "1000", "X-Ratelimit-Remaining-Requests", "0",
		"X-Ratelimit-Reset-Requests", "45s"))
	if got := l.Snapshot()[0].CoolingUntil; !got.Equal(t0.Add(45 * time.Second)) {
		t.Errorf("coolingUntil = %v, want the +45s reset", got)
	}
}
