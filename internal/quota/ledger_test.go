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

// Falloff: an idle counter window leaks its usage back out over the period, so a
// backend at its per-minute limit becomes available again partway through the
// minute (a fractional drain), not only at a hard reset boundary.
func TestCounterMode_Falloff(t *testing.T) {
	l := New()
	t0 := time.Unix(1_800_000_000, 0)
	fixed(l, t0)
	l.SetLimits("or", 10, 0) // 10/min, no daily
	for i := 0; i < 10; i++ {
		l.ObserveResponse("or", 200, http.Header{})
	}
	if l.Available("or") {
		t.Fatal("at the 10/min limit → unavailable")
	}
	// Half the window later, ~5 units have leaked out → available again.
	l.now = func() time.Time { return t0.Add(30 * time.Second) }
	if !l.Available("or") {
		t.Error("after 30s the level should have leaked below the limit")
	}
	if u := l.Snapshot()[0].Windows[0].Used; u != 5 {
		t.Errorf("used after 30s decay = %d, want 5", u)
	}
}

// Durability: a counter-mode backend's usage survives a restart. The ledger
// persists (level, at) to a CounterStore and, when a fresh ledger attaches the
// same store before SetLimits, the window resumes its decayed level instead of
// resetting to zero and over-sending against the provider's real daily cap.
func TestCounterMode_DurableAcrossRestart(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	st := &memCounterStore{}

	// First process: attach store, declare the daily budget, spend 800.
	l1 := New()
	fixed(l1, t0)
	l1.UseStore(st)
	l1.SetLimits("or", 0, 1000)
	for i := 0; i < 800; i++ {
		l1.ObserveResponse("or", 200, http.Header{})
	}
	if u := l1.Snapshot()[0].Windows[0].Used; u != 800 {
		t.Fatalf("first process used = %d, want 800", u)
	}

	// Restart one minute later: a NEW ledger attaches the SAME store, then
	// declares the same limit. The persisted 800 must carry over (minus the tiny
	// decay over 60s: 1000/day ≈ 0.7/min), NOT reset to zero.
	l2 := New()
	l2.now = func() time.Time { return t0.Add(time.Minute) }
	l2.UseStore(st)
	l2.SetLimits("or", 0, 1000)
	if u := l2.Snapshot()[0].Windows[0].Used; u < 798 || u > 800 {
		t.Errorf("after restart used = %d, want ~800 (survived), not 0", u)
	}
}

// memCounterStore is an in-memory CounterStore for the durability test.
type memCounterStore struct {
	rows map[string]PersistedCounter
}

func (m *memCounterStore) SaveQuotaCounter(backend, label string, used float64, atMS int64) error {
	if m.rows == nil {
		m.rows = map[string]PersistedCounter{}
	}
	m.rows[backend+"\x00"+label] = PersistedCounter{Backend: backend, Label: label, Used: used, At: time.UnixMilli(atMS)}
	return nil
}

func (m *memCounterStore) LoadQuotaCounters() ([]PersistedCounter, error) {
	out := make([]PersistedCounter, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
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
