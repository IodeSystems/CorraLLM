package api

import (
	"context"
	"time"

	"github.com/iodesystems/corrallm/internal/quota"
)

// Free-tier quota ledger (P16) — read-only observability over the per-backend
// budgets corrallm learns from remote providers' X-Ratelimit-* headers.

// QuotaBucketView is one rate-limit window for the API.
type QuotaBucketView struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining" doc:"The provider's own remaining count."`
	// Cap and EffRemaining appear only when a self-throttle is configured
	// (pointers so a real 0 — capped and exhausted — is distinct from "no cap").
	// EffRemaining is the budget WE allow (remaining minus the provider headroom
	// we deliberately leave unspent), and is what availability is judged on.
	Cap          *int   `json:"cap,omitempty"`
	EffRemaining *int   `json:"effRemaining,omitempty"`
	ResetsIn     string `json:"resetsIn,omitempty" doc:"Human duration until this window refills, e.g. \"1m26s\"; empty when unknown."`
	// Stale is true once the reset time has passed: the window has rolled on the
	// provider's side but we only learn the new count on the next call, so
	// `remaining` is a last-known value, not live. Availability still self-corrects.
	Stale bool `json:"stale,omitempty"`
}

// QuotaEntryView is one backend's live budget.
type QuotaEntryView struct {
	Backend      string          `json:"backend"`
	Requests     QuotaBucketView `json:"requests"`
	Tokens       QuotaBucketView `json:"tokens"`
	Available    bool            `json:"available" doc:"False when exhausted or cooling from a 429 — the selector skips it."`
	CoolingInSec int             `json:"coolingInSec,omitempty" doc:"Seconds left in a 429 cooldown; 0 when not cooling."`
	Seen         int64           `json:"seen"`
	// ObservedAgoSec is how long ago the counts were learned. The ledger only
	// updates on a request, so between calls the numbers are a snapshot this old,
	// not live — surfaced so a stale count is not read as current.
	ObservedAgoSec int `json:"observedAgoSec"`
	// Windows is populated for counter-mode backends (OpenRouter): locally-counted
	// request budgets, since the provider sends no rate-limit headers.
	Windows []QuotaWindowView `json:"windows,omitempty"`
	// Stale is true when the backend's model has churned out of its provider's
	// free roster (P16e) — unavailable until a refresh finds it free again.
	Stale bool `json:"stale,omitempty"`
}

// QuotaWindowView is a counter-mode backend's locally-counted request window.
type QuotaWindowView struct {
	Label    string `json:"label" doc:"\"1m\" (per-minute) or \"1d\" (per-day)."`
	Limit    int    `json:"limit"`
	Used     int    `json:"used"`
	ResetsIn string `json:"resetsIn,omitempty"`
}

// QuotaOutput lists every tracked backend's budget.
type QuotaOutput struct {
	Body struct {
		Backends []QuotaEntryView `json:"backends"`
	}
}

// QuotaLedger reports the free-tier budget ledger.
func (h *Handlers) QuotaLedger(_ context.Context, _ *struct{}) (*QuotaOutput, error) {
	out := &QuotaOutput{}
	out.Body.Backends = []QuotaEntryView{}
	if h.Proxy == nil {
		return out, nil
	}
	now := time.Now()
	for _, e := range h.Proxy.QuotaSnapshot() {
		v := QuotaEntryView{
			Backend:   e.Backend,
			Requests:  bucketView(e.Requests, e.CapRequests, now),
			Tokens:    bucketView(e.Tokens, e.CapTokens, now),
			Available: available(e, now),
			Seen:      e.Seen,
			Stale:     e.Stale,
		}
		if !e.LastSeen.IsZero() {
			v.ObservedAgoSec = int(now.Sub(e.LastSeen).Seconds())
		}
		if e.CoolingUntil.After(now) {
			v.CoolingInSec = int(time.Until(e.CoolingUntil).Seconds())
		}
		for _, w := range e.Windows {
			wv := QuotaWindowView{Label: w.Label, Limit: w.Limit, Used: w.Used}
			if w.ResetsAt.After(now) {
				wv.ResetsIn = time.Until(w.ResetsAt).Round(time.Second).String()
			}
			v.Windows = append(v.Windows, wv)
		}
		out.Body.Backends = append(out.Body.Backends, v)
	}
	return out, nil
}

func bucketView(b quota.Bucket, cap int, now time.Time) QuotaBucketView {
	v := QuotaBucketView{Limit: b.Limit, Remaining: b.Remaining}
	if cap > 0 && cap < b.Limit {
		eff := quota.EffRemaining(b, cap)
		v.Cap, v.EffRemaining = &cap, &eff
	}
	if !b.ResetsAt.IsZero() {
		if b.ResetsAt.After(now) {
			v.ResetsIn = time.Until(b.ResetsAt).Round(time.Second).String()
		} else {
			// Window has rolled since we last observed it — the count is stale.
			v.Stale = true
		}
	}
	return v
}

// available mirrors quota.Ledger.Available for a snapshotted entry (the ledger's
// own method needs the live map; this reads a copy the API already holds),
// honoring the self-cap.
func available(e quota.Entry, now time.Time) bool {
	if e.Stale {
		return false
	}
	if now.Before(e.CoolingUntil) {
		return false
	}
	windows := []struct {
		b   quota.Bucket
		cap int
	}{{e.Requests, e.CapRequests}, {e.Tokens, e.CapTokens}}
	for _, w := range windows {
		if w.b.Limit > 0 && quota.EffRemaining(w.b, w.cap) <= 0 && now.Before(w.b.ResetsAt) {
			return false
		}
	}
	// Counter-mode windows: exhausted if a still-active window is at its limit.
	for _, w := range e.Windows {
		if w.Limit > 0 && w.Used >= w.Limit && now.Before(w.ResetsAt) {
			return false
		}
	}
	return true
}
