// Package quota is corrallm's free-tier budget ledger (P16). It tracks each
// remote backend's remaining rate-limit budget, learned from the X-Ratelimit-*
// headers a provider returns on every response, so a selector can route around
// an exhausted backend BEFORE it 429s rather than discovering exhaustion by
// eating the error.
//
// A backend is one model definition = one key (see plan/p16-free-aggregator.md
// §4): two keys for the same provider are two independent budgets, so the ledger
// keys on the served name, never on the provider.
package quota

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Bucket is one rate-limit window (requests or tokens): the ceiling, what's
// left, and when the provider says it refills. ResetsAt is zero when unknown.
type Bucket struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetsAt  time.Time `json:"resetsAt,omitempty"`
}

// Entry is one backend's live budget across both windows.
type Entry struct {
	Backend  string `json:"backend"`
	Requests Bucket `json:"requests"`
	Tokens   Bucket `json:"tokens"`
	// CoolingUntil is set on a 429: the selector skips the backend until then.
	CoolingUntil time.Time `json:"coolingUntil,omitempty"`
	LastSeen     time.Time `json:"lastSeen"`
	Seen         int64     `json:"seen"`
	// CapRequests/CapTokens echo the configured self-cap (0 = none) so a consumer
	// can render the effective budget via EffRemaining(bucket, cap).
	CapRequests int `json:"capRequests,omitempty"`
	CapTokens   int `json:"capTokens,omitempty"`
	// Windows is populated for COUNTER-MODE backends (no rate-limit headers):
	// locally-counted request budgets per window (per-minute, per-day).
	Windows []Window `json:"windows,omitempty"`
}

// Window is a locally-counted request budget for a counter-mode backend.
type Window struct {
	Label    string    `json:"label"` // "1m" | "1d"
	Limit    int       `json:"limit"`
	Used     int       `json:"used"`
	ResetsAt time.Time `json:"resetsAt,omitempty"`
}

// cap is a per-backend self-throttle below the provider's own limit. 0 = none.
type cap struct{ requests, tokens int }

// counterWindow is one locally-counted request window for a counter-mode
// backend: a limit, a rolling count, and the window's start.
type counterWindow struct {
	label string
	limit int
	dur   time.Duration
	used  int
	start time.Time
}

// roll resets the window if its duration has elapsed since start. A zero start
// (never used) is treated as elapsed so the first tick begins a fresh window.
func (w *counterWindow) roll(now time.Time) {
	if w.start.IsZero() || now.Sub(w.start) >= w.dur {
		w.start, w.used = now, 0
	}
}

// counterState is a counter-mode backend's windows (per-minute, per-day).
type counterState struct{ windows []*counterWindow }

// Ledger holds live per-backend budgets. Safe for concurrent use.
type Ledger struct {
	mu       sync.RWMutex
	now      func() time.Time
	entries  map[string]*Entry
	caps     map[string]cap
	counters map[string]*counterState
}

// New builds an empty ledger.
func New() *Ledger {
	return &Ledger{
		now: time.Now, entries: map[string]*Entry{},
		caps: map[string]cap{}, counters: map[string]*counterState{},
	}
}

// SetLimits registers a COUNTER-MODE backend: one whose provider sends no
// rate-limit headers, so budget is tracked by counting our requests against
// these limits. Either window may be 0 to skip it. Called at construction from
// the backend's freeTier.limits config.
func (l *Ledger) SetLimits(backend string, rpm, rpd int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if rpm <= 0 && rpd <= 0 {
		delete(l.counters, backend)
		return
	}
	cs := &counterState{}
	if rpm > 0 {
		cs.windows = append(cs.windows, &counterWindow{label: "1m", limit: rpm, dur: time.Minute})
	}
	if rpd > 0 {
		cs.windows = append(cs.windows, &counterWindow{label: "1d", limit: rpd, dur: 24 * time.Hour})
	}
	l.counters[backend] = cs
	// Create the entry now so a counter-mode backend shows its declared budget in
	// the ledger before its first call (header-mode backends are discovered on
	// first response; counter-mode is declared, so surface it up front).
	if l.entries[backend] == nil {
		l.entries[backend] = &Entry{Backend: backend}
	}
}

// SetCap self-throttles a backend below the provider's limit: budget is treated
// as exhausted once usage reaches the cap, leaving the provider's own headroom
// unspent. 0 for a window means no cap on it. Called at construction from the
// backend's freeTier.cap config.
func (l *Ledger) SetCap(backend string, requests, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if requests <= 0 && tokens <= 0 {
		delete(l.caps, backend)
		return
	}
	l.caps[backend] = cap{requests: requests, tokens: tokens}
}

// EffRemaining is a bucket's remaining budget after a self-cap: the provider
// says `remaining` of `Limit` are left, but if we only allow `capN` of that
// Limit, we have `remaining - (Limit - capN)` of OUR budget left. No cap (0) or
// a cap at/above the provider's limit leaves the provider value untouched.
func EffRemaining(b Bucket, capN int) int {
	if capN <= 0 || capN >= b.Limit || b.Limit <= 0 {
		return b.Remaining
	}
	return b.Remaining - (b.Limit - capN)
}

// ObserveResponse folds a proxied response's rate-limit headers into the
// backend's entry. It is a no-op when the response carries no X-Ratelimit-*
// headers and is not a 429 — a local llama.cpp reply — so it is safe to call for
// every proxied response.
func (l *Ledger) ObserveResponse(backend string, status int, h http.Header) {
	is429 := status == http.StatusTooManyRequests
	l.mu.Lock()
	defer l.mu.Unlock()
	_, isCounter := l.counters[backend]
	hasHeaders := h.Get("X-Ratelimit-Limit-Requests") != "" || h.Get("X-Ratelimit-Limit-Tokens") != ""
	if !is429 && !hasHeaders && !isCounter {
		return
	}
	now := l.now()
	e := l.entries[backend]
	if e == nil {
		e = &Entry{Backend: backend}
		l.entries[backend] = e
	}
	e.LastSeen = now
	e.Seen++
	updateBucket(&e.Requests, h, "Requests", now)
	updateBucket(&e.Tokens, h, "Tokens", now)
	// Counter-mode: this completed request counts, INCLUDING a 429 — providers
	// count failed requests against the quota too (verified in the research).
	if cs := l.counters[backend]; cs != nil {
		for _, w := range cs.windows {
			w.roll(now)
			w.used++
		}
	}
	if is429 {
		e.CoolingUntil = coolUntil(h, e, now)
	}
}

// updateBucket reads the limit/remaining/reset triple for one window. The reset
// header is a GO-DURATION STRING ("1m26.4s", "310ms") — verified live against
// Groq, and the reason this uses time.ParseDuration, not strconv: a naive
// integer-seconds parse silently drops every reset.
func updateBucket(b *Bucket, h http.Header, kind string, now time.Time) {
	if v := h.Get("X-Ratelimit-Limit-" + kind); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			b.Limit = n
		}
	}
	if v := h.Get("X-Ratelimit-Remaining-" + kind); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			b.Remaining = n
		}
	}
	if v := h.Get("X-Ratelimit-Reset-" + kind); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			b.ResetsAt = now.Add(d)
		}
	}
}

// coolUntil picks how long to skip a backend after a 429: Retry-After if the
// provider sent one (seconds int or duration string), else the soonest reset of
// an exhausted bucket, else a conservative minute.
func coolUntil(h http.Header, e *Entry, now time.Time) time.Time {
	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if d, err := time.ParseDuration(ra); err == nil {
			return now.Add(d)
		}
	}
	var soonest time.Time
	for _, b := range []Bucket{e.Requests, e.Tokens} {
		if b.Remaining <= 0 && !b.ResetsAt.IsZero() {
			if soonest.IsZero() || b.ResetsAt.Before(soonest) {
				soonest = b.ResetsAt
			}
		}
	}
	if !soonest.IsZero() {
		return soonest
	}
	return now.Add(time.Minute)
}

// MarkDown puts a backend in cooldown for dur, used for a HARD failure (402
// payment-required, 403 forbidden, 401 unauthorized) that a retry won't fix
// soon. The selector skips it until it might recover — billing enabled, key
// corrected — instead of hammering a backend that structurally cannot serve.
func (l *Ledger) MarkDown(backend string, dur time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[backend]
	if e == nil {
		e = &Entry{Backend: backend}
		l.entries[backend] = e
	}
	e.CoolingUntil = now.Add(dur)
	e.LastSeen = now
}

// Available reports whether a backend has budget and is not cooling. An unknown
// backend (never observed) is optimistically available — the ledger learns on
// the first response, and refusing a backend we know nothing about would strand
// it forever.
func (l *Ledger) Available(backend string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e := l.entries[backend]
	if e == nil {
		return true
	}
	now := l.now()
	if now.Before(e.CoolingUntil) {
		return false
	}
	c := l.caps[backend]
	windows := []struct {
		b   Bucket
		cap int
	}{{e.Requests, c.requests}, {e.Tokens, c.tokens}}
	for _, w := range windows {
		if w.b.Limit > 0 && EffRemaining(w.b, w.cap) <= 0 && now.Before(w.b.ResetsAt) {
			return false
		}
	}
	// Counter-mode windows: exhausted if a still-active window is at its limit.
	if cs := l.counters[backend]; cs != nil {
		for _, cw := range cs.windows {
			active := !cw.start.IsZero() && now.Sub(cw.start) < cw.dur
			if active && cw.used >= cw.limit {
				return false
			}
		}
	}
	return true
}

// Snapshot returns a copy of every tracked entry, backend-sorted, for display.
func (l *Ledger) Snapshot() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := l.now()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		v := *e
		if c, ok := l.caps[e.Backend]; ok {
			v.CapRequests, v.CapTokens = c.requests, c.tokens
		}
		if cs := l.counters[e.Backend]; cs != nil {
			v.Windows = make([]Window, 0, len(cs.windows))
			for _, cw := range cs.windows {
				w := Window{Label: cw.label, Limit: cw.limit, Used: cw.used}
				// A window whose duration has elapsed has effectively reset; show
				// it as 0 used rather than a stale count from a spent window.
				if cw.start.IsZero() || now.Sub(cw.start) >= cw.dur {
					w.Used = 0
				} else {
					w.ResetsAt = cw.start.Add(cw.dur)
				}
				v.Windows = append(v.Windows, w)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}
