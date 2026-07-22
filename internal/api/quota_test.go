package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/quota"
)

// The API layer's own logic is formatting a ledger entry: bucketView + available.
func TestQuotaFormatting(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	// Budget left, reset in the future → available, resetsIn populated.
	e := quota.Entry{
		Backend:  "groq-a",
		Requests: quota.Bucket{Limit: 1000, Remaining: 999, ResetsAt: now.Add(86 * time.Second)},
		Tokens:   quota.Bucket{Limit: 12000, Remaining: 11938, ResetsAt: now.Add(310 * time.Millisecond)},
	}
	if !available(e, now) {
		t.Error("entry with budget should be available")
	}
	if v := bucketView(e.Requests, 0, now); v.Remaining != 999 || v.ResetsIn == "" {
		t.Errorf("bucketView dropped data: %+v", v)
	}

	// Exhausted request bucket, not yet reset → unavailable.
	e.Requests.Remaining = 0
	if available(e, now) {
		t.Error("exhausted bucket should make the entry unavailable")
	}
	// After reset → available again.
	if !available(e, now.Add(90*time.Second)) {
		t.Error("should recover after the reset time")
	}

	// Cooling from a 429 → unavailable regardless of buckets.
	e2 := quota.Entry{Backend: "x", CoolingUntil: now.Add(30 * time.Second)}
	if available(e2, now) {
		t.Error("a cooling entry should be unavailable")
	}
	if available(e2, now.Add(31*time.Second)) == false {
		t.Error("cooling should clear")
	}

	// A self-cap surfaces cap + effRemaining and drives availability.
	capped := bucketView(quota.Bucket{Limit: 1000, Remaining: 250}, 800, now)
	if capped.Cap == nil || *capped.Cap != 800 || capped.EffRemaining == nil || *capped.EffRemaining != 50 {
		t.Errorf("capped bucket view wrong: %+v", capped)
	}

	// A reset already in the past must not render a resetsIn.
	if v := bucketView(quota.Bucket{Limit: 10, Remaining: 3, ResetsAt: now.Add(-time.Second)}, 0, now); v.ResetsIn != "" {
		t.Errorf("past reset should not populate resetsIn: %q", v.ResetsIn)
	}
}

func TestQuotaLedger_NoProxyIsEmptyNotNil(t *testing.T) {
	out, err := (&Handlers{}).QuotaLedger(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Backends == nil {
		t.Error("backends must serialize as [] not null")
	}
}
