package api

import (
	"context"
	"github.com/iodesystems/corrallm/internal/config"
	"sort"
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
	// Reported is what the provider says about its own limits, as sent.
	Reported []ReportedLimitView `json:"reported,omitempty" doc:"The provider's latest x-ratelimit-* headers, as sent (e.g. Fireworks' per-model token-per-minute limits). Shown, not enforced."`
}

// ReportedLimitView is one provider rate-limit header.
type ReportedLimitView struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// QuotaWindowView is a counter-mode backend's locally-counted request window.
// It is a FALLOFF counter (leaky bucket): Used decays back toward zero at
// limit/window, so ResetsIn is when the current level fully drains, not a hard
// reset boundary.
type QuotaWindowView struct {
	Label    string  `json:"label" doc:"\"1m\" (per-minute) or \"1d\" (per-day)."`
	Limit    float64 `json:"limit"`
	Used     float64 `json:"used" doc:"Fill level. A falloff window leaks back toward zero over its period; a calendar window (calendar-day / calendar-month) empties at the UTC boundary."`
	Blocked  bool    `json:"blocked,omitempty" doc:"The level has reached the limit — the backend is skipped until it drains below."`
	ResetsIn string  `json:"resetsIn,omitempty" doc:"Time until the current level fully drains to zero."`
	Hard     bool    `json:"hard,omitempty" doc:"A full window refuses (429). False: soft — reported, and only steers routing."`
	// SPEND RATE (P56).
	RatePerHour      float64 `json:"ratePerHour" doc:"This window's dimension (usd or req) charged to the scope over the last hour, per hour. Over rateSpanSec when that is shorter (the process started under an hour ago)."`
	RateSpanSec      int     `json:"rateSpanSec" doc:"Seconds ratePerHour is measured over, up to 3600. 0 means too soon after start to call a rate."`
	ExhaustsInSec    int     `json:"exhaustsInSec" doc:"Seconds until the level reaches the limit at ratePerHour. 0 when it does not: the rate is 0, a falloff window leaks faster than it fills, or a calendar window empties first."`
	ProjectedAtReset float64 `json:"projectedAtReset" doc:"Calendar windows: the level reached by the period boundary at ratePerHour. 0 on a falloff window."`
}

// RefusingBackendView is one backend that is refusing outright, and for how long.
//
// A separate list rather than a field on QuotaEntryView, because the ledger only
// carries backends it has OBSERVED a response from. A rung that has refused every
// request since before the last restart may not be in the ledger at all, and the
// whole point of this list is that such a rung stops being invisible.
type RefusingBackendView struct {
	Backend string `json:"backend"`
	Status  int    `json:"status" doc:"The most recent refusal's HTTP status: 401 (key), 402 (unpaid), 403 (not permitted)."`
	Count   int64  `json:"count" doc:"Refusals since it started. A low count with an old start means nothing is even trying it any more."`
	SinceMS int64  `json:"sinceMs" doc:"Unix millis of the FIRST refusal in this run — how long it has been broken, not when it last failed."`
	LastMS  int64  `json:"lastMs" doc:"Unix millis of the most recent refusal."`
}

// refusingViews renders the refusal ledger, minus whatever the configuration
// has disabled. One implementation, because both readers of this list — the
// budgets page and Overview, which is where Now builds its fault list — have to
// agree about what counts as a fault.
//
// DROPPED HERE, NOT AT THE SOURCE, and that is the decision worth defending.
// backend_refusal is a record of what happened: this backend answered 403, at
// this time, this many times. Deleting the row on disable would falsify that
// record, and it would destroy exactly the evidence the decision rests on — the
// operator disables a backend BECAUSE of the refusal, so the refusal is the
// justification, not the noise. It is also the wrong layer: the store has no
// config and no business asking one whether a fact is worth keeping.
//
// The API is where the question is actually being asked, and the question is
// not "did this refuse" but "is there something here that needs attention".
// Nothing is calling a disabled backend, so nothing about it can change, and a
// fault the reader cannot act on is not a fault (OB-6). The row survives, so
// enabling the model brings back the honest answer: this is still the thing
// that was broken, and for how long.
func (h *Handlers) refusingViews() ([]RefusingBackendView, error) {
	out := []RefusingBackendView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.RefusingBackends()
	if err != nil {
		return nil, err
	}
	cfg := h.config()
	var live map[string]config.Model
	if cfg != nil {
		live = cfg.AllModels() // disabled models are already left out of it
	}
	for _, r := range rows {
		// The ledger keys on the served model name (proxy.noteRefusal records
		// the candidate's name), so the config lookup is direct.
		//
		// A backend that is DISABLED or no longer in the config at all is
		// skipped for the same reason: nothing calls it, so its refusal can
		// never clear (only a success clears one) and the reader cannot act on
		// it. A removed backend's row used to sit on Setup indefinitely —
		// groq-gpt-oss-120b, 19 days after the free lane that held it was
		// deleted. The row survives either way, so bringing the model back
		// brings back the honest answer.
		if _, ok := live[r.Backend]; cfg != nil && !ok {
			continue
		}
		out = append(out, RefusingBackendView{
			Backend: r.Backend, Status: r.Status, Count: r.Count,
			SinceMS: r.SinceMS, LastMS: r.LastMS,
		})
	}
	return out, nil
}

// QuotaOutput lists every tracked backend's budget.
type QuotaOutput struct {
	Body struct {
		Backends []QuotaEntryView `json:"backends"`
		// Refusing is what is answering 401/402/403/429/503 right now, longest-broken
		// first. Empty is the normal case and means nothing is refusing.
		Refusing []RefusingBackendView `json:"refusing"`
	}
}

func reportedViews(m map[string]string) []ReportedLimitView {
	if len(m) == 0 {
		return nil
	}
	out := make([]ReportedLimitView, 0, len(m))
	for k, v := range m {
		out = append(out, ReportedLimitView{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// QuotaLedger reports the free-tier budget ledger.
func (h *Handlers) QuotaLedger(_ context.Context, _ *struct{}) (*QuotaOutput, error) {
	out := &QuotaOutput{}
	out.Body.Backends = []QuotaEntryView{}
	refusing, err := h.refusingViews()
	if err != nil {
		return nil, err
	}
	out.Body.Refusing = refusing
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
			Reported:  reportedViews(e.Reported),
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
			wv := QuotaWindowView{Label: w.Label, Limit: w.Limit, Used: w.Used, Hard: w.Hard,
				RatePerHour: w.RatePerHour, RateSpanSec: int(w.RateSpan.Seconds()),
				ProjectedAtReset: w.ProjectedAtReset}
			if w.ExhaustsAt.After(now) {
				wv.ExhaustsInSec = int(w.ExhaustsAt.Sub(now).Seconds())
			}
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
