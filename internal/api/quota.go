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
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	ResetsIn  string `json:"resetsIn,omitempty" doc:"Human duration until this window refills, e.g. \"1m26s\"; empty when unknown."`
}

// QuotaEntryView is one backend's live budget.
type QuotaEntryView struct {
	Backend      string          `json:"backend"`
	Requests     QuotaBucketView `json:"requests"`
	Tokens       QuotaBucketView `json:"tokens"`
	Available    bool            `json:"available" doc:"False when exhausted or cooling from a 429 — the selector skips it."`
	CoolingInSec int             `json:"coolingInSec,omitempty" doc:"Seconds left in a 429 cooldown; 0 when not cooling."`
	Seen         int64           `json:"seen"`
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
			Requests:  bucketView(e.Requests, now),
			Tokens:    bucketView(e.Tokens, now),
			Available: available(e, now),
			Seen:      e.Seen,
		}
		if e.CoolingUntil.After(now) {
			v.CoolingInSec = int(time.Until(e.CoolingUntil).Seconds())
		}
		out.Body.Backends = append(out.Body.Backends, v)
	}
	return out, nil
}

func bucketView(b quota.Bucket, now time.Time) QuotaBucketView {
	v := QuotaBucketView{Limit: b.Limit, Remaining: b.Remaining}
	if b.ResetsAt.After(now) {
		v.ResetsIn = time.Until(b.ResetsAt).Round(time.Second).String()
	}
	return v
}

// available mirrors quota.Ledger.Available for a snapshotted entry (the ledger's
// own method needs the live map; this reads a copy the API already holds).
func available(e quota.Entry, now time.Time) bool {
	if now.Before(e.CoolingUntil) {
		return false
	}
	for _, b := range []quota.Bucket{e.Requests, e.Tokens} {
		if b.Limit > 0 && b.Remaining <= 0 && now.Before(b.ResetsAt) {
			return false
		}
	}
	return true
}
