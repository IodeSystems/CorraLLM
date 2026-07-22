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
	"math"
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
	// Stale is true when the backend's model has churned out of its provider's
	// free roster (P16e): it is unavailable until a refresh finds it free again.
	Stale bool `json:"stale,omitempty"`
}

// Window is a locally-counted request budget for a counter-mode backend.
type Window struct {
	Label    string    `json:"label"` // "1m" | "1d"
	Limit    int       `json:"limit"`
	Used     int       `json:"used"`             // decayed fill level, rounded for display
	Blocked  bool      `json:"blocked,omitempty"` // level has reached the limit
	ResetsAt time.Time `json:"resetsAt,omitempty"` // when the level fully drains to zero
}

// cap is a per-backend self-throttle below the provider's own limit. 0 = none.
type cap struct{ requests, tokens int }

// counterWindow is one locally-counted request budget for a counter-mode backend,
// modeled as a FALLOFF COUNTER (leaky bucket): each request adds one unit and the
// level leaks back out at limit/dur, so an idle window drains to empty over one
// period instead of snapping to zero at a reset cliff. This has two payoffs over
// a fixed-reset window: no thundering burst at a reset boundary, and the whole
// state is just (used, at) — a scalar and a timestamp — so it persists to two
// columns and a restart resumes by decaying the level for the elapsed downtime.
type counterWindow struct {
	label string
	limit int
	dur   time.Duration
	used  float64   // fill level as of `at`
	at    time.Time // when `used` was last computed
}

// levelAt returns the decayed fill at now without mutating: the bucket leaks
// `limit` units per `dur`, clamped at zero. A zero `at` (never used) is the
// stored level as-is.
func (w *counterWindow) levelAt(now time.Time) float64 {
	if w.at.IsZero() {
		return w.used
	}
	elapsed := now.Sub(w.at)
	if elapsed <= 0 {
		return w.used
	}
	lv := w.used - float64(w.limit)*(float64(elapsed)/float64(w.dur))
	if lv < 0 {
		return 0
	}
	return lv
}

// add decays to now, then adds one unit of usage.
func (w *counterWindow) add(now time.Time) {
	w.used = w.levelAt(now)
	w.at = now
	w.used++
}

// drainAt is when the current level would fully leak back to zero (the display
// "resets" time); zero when already empty.
func (w *counterWindow) drainAt(now time.Time) time.Time {
	lv := w.levelAt(now)
	if lv <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(lv / float64(w.limit) * float64(w.dur)))
}

// counterState is a counter-mode backend's windows (per-minute, per-day).
type counterState struct{ windows []*counterWindow }

// PersistedCounter is one falloff window's durable state (see CounterStore).
type PersistedCounter struct {
	Backend string
	Label   string
	Used    float64
	At      time.Time
}

// CounterStore persists falloff-counter state so a counter-mode backend's usage
// survives a restart. Header-tracked backends need none (they relearn from the
// next response), but a locally-counted daily budget would otherwise reset to
// zero on every restart and over-send against the provider's real cap.
type CounterStore interface {
	LoadQuotaCounters() ([]PersistedCounter, error)
	SaveQuotaCounter(backend, label string, used float64, atUnixMS int64) error
}

// loadedCounter is a persisted level awaiting a matching window at construction.
type loadedCounter struct {
	used float64
	at   time.Time
}

func counterKey(backend, label string) string { return backend + "\x00" + label }

// Ledger holds live per-backend budgets. Safe for concurrent use.
type Ledger struct {
	mu        sync.RWMutex
	now       func() time.Time
	entries   map[string]*Entry
	caps      map[string]cap
	counters  map[string]*counterState
	hardFails map[string]int  // consecutive hard failures per backend, for backoff
	stale     map[string]bool // backend's model has churned out of its provider's free roster (P16e)
	store     CounterStore    // durable falloff-counter state (nil = memory-only)
	loaded    map[string]loadedCounter
}

// New builds an empty ledger.
func New() *Ledger {
	return &Ledger{
		now: time.Now, entries: map[string]*Entry{},
		caps: map[string]cap{}, counters: map[string]*counterState{},
		hardFails: map[string]int{}, stale: map[string]bool{},
	}
}

// UseStore attaches durable storage for the falloff counters and loads any
// persisted levels. Call it BEFORE SetLimits so each window seeds from its saved
// state and a restart resumes the day's usage instead of resetting to zero. A nil
// store (tests) leaves counters memory-only; a load error starts cold rather than
// failing construction — persistence is a durability optimization, not a
// correctness dependency.
func (l *Ledger) UseStore(s CounterStore) {
	if s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = s
	rows, err := s.LoadQuotaCounters()
	if err != nil {
		return
	}
	l.loaded = make(map[string]loadedCounter, len(rows))
	for _, r := range rows {
		l.loaded[counterKey(r.Backend, r.Label)] = loadedCounter{used: r.Used, at: r.At}
	}
}

// SetStale marks (or clears) a backend as churned out of its provider's free
// roster (P16e). A stale backend is unavailable to the selector until a later
// roster refresh finds its model free again. Distinct from cooling: staleness is
// a config/roster fact, not a rate limit, and clears the moment the model
// reappears rather than after a timer.
func (l *Ledger) SetStale(backend string, stale bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if stale {
		l.stale[backend] = true
		if l.entries[backend] == nil {
			l.entries[backend] = &Entry{Backend: backend}
		}
	} else {
		delete(l.stale, backend)
	}
}

// Hard-failure backoff bounds: the cooldown after a 401/402/403 doubles each
// consecutive failure from base, capped at max, so a persistently-broken backend
// (e.g. 402 billing) quiesces toward once a day instead of being hammered every
// few minutes. A success resets it (see ObserveResponse).
const (
	hardFailBase  = 5 * time.Minute
	hardFailMax   = 24 * time.Hour
	hardFailShift = 13 // cap the shift so base<<shift never overflows int64
)

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
		cs.windows = append(cs.windows, l.seedWindow(backend, "1m", rpm, time.Minute))
	}
	if rpd > 0 {
		cs.windows = append(cs.windows, l.seedWindow(backend, "1d", rpd, 24*time.Hour))
	}
	l.counters[backend] = cs
	// Create the entry now so a counter-mode backend shows its declared budget in
	// the ledger before its first call (header-mode backends are discovered on
	// first response; counter-mode is declared, so surface it up front).
	if l.entries[backend] == nil {
		l.entries[backend] = &Entry{Backend: backend}
	}
}

// seedWindow builds a fresh falloff window, restoring its level from persisted
// state (UseStore) when present so the count survives a restart.
func (l *Ledger) seedWindow(backend, label string, limit int, dur time.Duration) *counterWindow {
	w := &counterWindow{label: label, limit: limit, dur: dur}
	if lc, ok := l.loaded[counterKey(backend, label)]; ok {
		w.used, w.at = lc.used, lc.at
	}
	return w
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
	// A success clears the hard-failure backoff streak: a backend that starts
	// serving again returns to fast retries next time it fails.
	if status >= 200 && status < 300 {
		delete(l.hardFails, backend)
	}
	_, isCounter := l.counters[backend]
	hasHeaders := h.Get("X-Ratelimit-Limit-Requests") != "" || h.Get("X-Ratelimit-Limit-Tokens") != ""
	if !is429 && !hasHeaders && !isCounter {
		l.mu.Unlock()
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
	var saves []PersistedCounter
	if cs := l.counters[backend]; cs != nil {
		for _, w := range cs.windows {
			w.add(now)
			if l.store != nil {
				saves = append(saves, PersistedCounter{Backend: backend, Label: w.label, Used: w.used, At: w.at})
			}
		}
	}
	if is429 {
		e.CoolingUntil = coolUntil(h, e, now)
	}
	store := l.store
	l.mu.Unlock()
	// Persist the updated levels OUTSIDE the lock — SQLite I/O must not block
	// other ledger readers, and the in-memory ledger is already authoritative;
	// the write is best-effort durability, so a failure is swallowed.
	for _, s := range saves {
		_ = store.SaveQuotaCounter(s.Backend, s.Label, s.Used, s.At.UnixMilli())
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

// MarkDown records a HARD failure (401/402/403 — auth or billing, which a retry
// won't fix) and cools the backend with EXPONENTIAL BACKOFF: 5m, 10m, 20m, …
// doubling per consecutive failure, capped at 24h. A persistently-broken backend
// thus backs off toward once a day instead of being retried every few minutes; a
// later success (in ObserveResponse) resets the streak so a recovered backend
// returns to fast retries. Returns the cooldown applied (for logging).
func (l *Ledger) MarkDown(backend string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.hardFails[backend]++
	shift := l.hardFails[backend] - 1
	if shift > hardFailShift {
		shift = hardFailShift
	}
	dur := hardFailBase << shift
	if dur > hardFailMax {
		dur = hardFailMax
	}
	e := l.entries[backend]
	if e == nil {
		e = &Entry{Backend: backend}
		l.entries[backend] = e
	}
	e.CoolingUntil = now.Add(dur)
	e.LastSeen = now
	return dur
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
	if l.stale[backend] {
		return false
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
	// Counter-mode windows: exhausted while the decayed fill is at the limit.
	if cs := l.counters[backend]; cs != nil {
		for _, cw := range cs.windows {
			if cw.levelAt(now) >= float64(cw.limit) {
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
		v.Stale = l.stale[e.Backend]
		if cs := l.counters[e.Backend]; cs != nil {
			v.Windows = make([]Window, 0, len(cs.windows))
			for _, cw := range cs.windows {
				lv := cw.levelAt(now)
				w := Window{
					Label:   cw.label,
					Limit:   cw.limit,
					Used:    int(math.Round(lv)),
					Blocked: lv >= float64(cw.limit),
				}
				if d := cw.drainAt(now); !d.IsZero() {
					w.ResetsAt = d
				}
				v.Windows = append(v.Windows, w)
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}
