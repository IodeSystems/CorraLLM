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

// A self-cap makes budget run out before the provider's own limit does.
func TestSelfCap(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.SetCap("groq-a", 800, 0) // cap requests at 800 of Groq's 1000

	// Provider says 250 left (used 750). Under our 800 cap, effective = 250-(1000-800)=50 → still available.
	l.ObserveResponse("groq-a", 200, hdr(
		"X-Ratelimit-Limit-Requests", "1000", "X-Ratelimit-Remaining-Requests", "250",
		"X-Ratelimit-Reset-Requests", "60s"))
	if !l.Available("groq-a") {
		t.Error("50 of our cap left → should still be available")
	}
	// Provider says 200 left (used 800 = our whole cap). Effective = 200-200 = 0 → exhausted, though Groq has 200 to spare.
	l.ObserveResponse("groq-a", 200, hdr(
		"X-Ratelimit-Limit-Requests", "1000", "X-Ratelimit-Remaining-Requests", "200",
		"X-Ratelimit-Reset-Requests", "60s"))
	if l.Available("groq-a") {
		t.Error("self-cap reached → should be unavailable even with provider headroom")
	}
}

func TestEffRemaining(t *testing.T) {
	// No cap → provider value untouched.
	if got := EffRemaining(Bucket{Limit: 1000, Remaining: 300}, 0); got != 300 {
		t.Errorf("no cap: got %d want 300", got)
	}
	// Cap 800 of 1000, 300 provider-remaining → 300-(1000-800)=100.
	if got := EffRemaining(Bucket{Limit: 1000, Remaining: 300}, 800); got != 100 {
		t.Errorf("capped: got %d want 100", got)
	}
	// Cap >= limit → no effect.
	if got := EffRemaining(Bucket{Limit: 1000, Remaining: 300}, 1000); got != 300 {
		t.Errorf("cap>=limit: got %d want 300", got)
	}
}

// Counter-mode: no headers, budget tracked by counting our requests. OpenRouter.
func TestCounterMode(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.SetLimits("or", 20, 1000) // 20/min, 1000/day

	// Registered but uncalled → visible with declared limits, available.
	snap := l.Snapshot()
	if len(snap) != 1 || len(snap[0].Windows) != 2 {
		t.Fatalf("counter-mode backend should show 2 windows pre-call: %+v", snap)
	}
	if !l.Available("or") {
		t.Error("uncalled counter-mode backend should be available")
	}

	// Fire 20 requests (no headers) → per-minute window exhausted.
	for i := 0; i < 20; i++ {
		l.ObserveResponse("or", 200, http.Header{})
	}
	if l.Available("or") {
		t.Error("20/min reached → should be unavailable")
	}
	w := l.Snapshot()[0].Windows
	var minute Window
	for _, x := range w {
		if x.Label == "1m" {
			minute = x
		}
	}
	if minute.Used != 20 {
		t.Errorf("per-minute used = %d, want 20", minute.Used)
	}

	// After the minute rolls, the per-minute window resets → available again
	// (still well under the 1000/day).
	l.now = func() time.Time { return t0.Add(61 * time.Second) }
	if !l.Available("or") {
		t.Error("per-minute window should reset after 60s")
	}
	// One more call rolls the 1m window to used=1.
	l.ObserveResponse("or", 200, http.Header{})
	for _, x := range l.Snapshot()[0].Windows {
		if x.Label == "1m" && x.Used != 1 {
			t.Errorf("per-minute should have rolled to 1, got %d", x.Used)
		}
	}
}

// A 429 counts against the counter too (providers count failed requests).
func TestCounterMode_429Counts(t *testing.T) {
	l := New()
	fixed(l, time.Unix(1_800_000_000, 0))
	l.SetLimits("or", 2, 0)
	l.ObserveResponse("or", 200, http.Header{})
	l.ObserveResponse("or", 429, http.Header{}) // failed, but still counts
	if l.Available("or") {
		t.Error("2 requests (one a 429) reached the 2/min limit → unavailable")
	}
}

// MarkDown takes a backend out of rotation with exponential backoff.
func TestMarkDown_ExponentialBackoff(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)

	// First failure = 5m base.
	if d := l.MarkDown("c"); d != 5*time.Minute {
		t.Errorf("1st backoff = %v, want 5m", d)
	}
	if l.Available("c") {
		t.Error("a marked-down backend must be unavailable")
	}
	// Doubling: 10m, 20m, 40m.
	for i, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute} {
		if d := l.MarkDown("c"); d != want {
			t.Errorf("backoff #%d = %v, want %v", i+2, d, want)
		}
	}
	// Enough consecutive failures reach the 24h cap and stay there.
	for i := 0; i < 20; i++ {
		l.MarkDown("c")
	}
	if d := l.MarkDown("c"); d != 24*time.Hour {
		t.Errorf("backoff should cap at 24h, got %v", d)
	}

	// A success resets the streak → next failure is back to the 5m base.
	l.ObserveResponse("c", 200, hdr(
		"X-Ratelimit-Limit-Requests", "10", "X-Ratelimit-Remaining-Requests", "9"))
	if d := l.MarkDown("c"); d != 5*time.Minute {
		t.Errorf("after a success the backoff should reset to 5m, got %v", d)
	}
}

// A stale backend (churned out of its provider's free roster) is unavailable
// until the mark is cleared.
func TestSetStale(t *testing.T) {
	l := New()
	fixed(l, time.Unix(1_800_000_000, 0))
	if !l.Available("or") {
		t.Fatal("precondition: available")
	}
	l.SetStale("or", true)
	if l.Available("or") {
		t.Error("a stale backend must be unavailable")
	}
	if s := l.Snapshot(); len(s) != 1 || !s[0].Stale {
		t.Errorf("snapshot should show stale=true: %+v", s)
	}
	l.SetStale("or", false)
	if !l.Available("or") {
		t.Error("clearing stale should restore availability")
	}
}
