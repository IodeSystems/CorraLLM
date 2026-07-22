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
}

// Ledger holds live per-backend budgets. Safe for concurrent use.
type Ledger struct {
	mu      sync.RWMutex
	now     func() time.Time
	entries map[string]*Entry
}

// New builds an empty ledger.
func New() *Ledger {
	return &Ledger{now: time.Now, entries: map[string]*Entry{}}
}

// ObserveResponse folds a proxied response's rate-limit headers into the
// backend's entry. It is a no-op when the response carries no X-Ratelimit-*
// headers and is not a 429 — a local llama.cpp reply — so it is safe to call for
// every proxied response.
func (l *Ledger) ObserveResponse(backend string, status int, h http.Header) {
	is429 := status == http.StatusTooManyRequests
	if !is429 && h.Get("X-Ratelimit-Limit-Requests") == "" && h.Get("X-Ratelimit-Limit-Tokens") == "" {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[backend]
	if e == nil {
		e = &Entry{Backend: backend}
		l.entries[backend] = e
	}
	e.LastSeen = now
	e.Seen++
	updateBucket(&e.Requests, h, "Requests", now)
	updateBucket(&e.Tokens, h, "Tokens", now)
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
	for _, b := range []Bucket{e.Requests, e.Tokens} {
		if b.Limit > 0 && b.Remaining <= 0 && now.Before(b.ResetsAt) {
			return false
		}
	}
	return true
}

// Snapshot returns a copy of every tracked entry, backend-sorted, for display.
func (l *Ledger) Snapshot() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}
