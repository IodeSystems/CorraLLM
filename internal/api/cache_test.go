package api

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// TestCacheHitReporting: a zero cache count must stay distinguishable from a
// backend that never reports one. Collapsing the two would show every embedding
// model and remote provider as a 0%-hit-rate cache problem they do not have.
func TestCacheHitReporting(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	rows := []store.Activity{
		// caching: 1000 prompt tokens, 750 from cache, over two requests — one hit,
		// one genuine miss (a cold first call).
		{TS: now - 1000, Served: "caching", Key: "a", Status: 200,
			PromptTokens: 500, CachedTokens: 750, PromptPerSec: 1000},
		{TS: now - 900, Served: "caching", Key: "a", Status: 200,
			PromptTokens: 500, CachedTokens: 0, PromptPerSec: 1000},
		// embedder: real traffic, never reports a cache at all.
		{TS: now - 800, Served: "embedder", Key: "b", Status: 200, PromptTokens: 4000},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handlers{Store: st, Cfg: &config.Config{}}
	out, err := h.UsageRollup(context.Background(), &UsageRollupInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RollupRow{}
	for _, r := range out.Body.Rows {
		got[r.Served] = r
	}

	c := got["caching"]
	if c.CachedTokens != 750 {
		t.Errorf("caching cachedTokens = %d, want 750", c.CachedTokens)
	}
	if math.Abs(c.CacheHitRate-0.75) > 0.001 {
		t.Errorf("caching hit rate = %v, want 0.75", c.CacheHitRate)
	}
	// One of the two requests reported a hit; the other missed for real.
	if c.CacheReports != 1 {
		t.Errorf("caching cacheReports = %d, want 1", c.CacheReports)
	}
	// 750 cached tokens at 1000 tok/s ≈ 0.75s of prompt processing avoided.
	if math.Abs(c.CachedSecondsSaved-0.75) > 0.01 {
		t.Errorf("caching secondsSaved = %v, want ~0.75", c.CachedSecondsSaved)
	}

	// The embedder has real traffic and a 0 hit rate arithmetically — but zero
	// reports, which is what marks it UNKNOWN rather than measured-zero.
	e := got["embedder"]
	if e.PromptTokens != 4000 {
		t.Fatalf("embedder promptTokens = %d, want 4000", e.PromptTokens)
	}
	if e.CacheReports != 0 {
		t.Errorf("embedder cacheReports = %d, want 0 (never reports a cache)", e.CacheReports)
	}

	// The grand total's rate comes from the totals, not an average of the rows:
	// 750 cached of 5000 prompted = 15%, NOT the (75% + 0%)/2 = 37.5% a per-row
	// mean would produce.
	tot := out.Body.Total
	if tot.PromptTokens != 5000 || tot.CachedTokens != 750 {
		t.Fatalf("total = %d cached / %d prompt, want 750 / 5000", tot.CachedTokens, tot.PromptTokens)
	}
	if math.Abs(tot.CacheHitRate-0.15) > 0.001 {
		t.Errorf("total hit rate = %v, want 0.15 (weighted by tokens, not averaged per model)",
			tot.CacheHitRate)
	}

	// Per-key carries the same distinction.
	byKey, err := h.UsageByKey(context.Background(), &UsageByKeyInput{})
	if err != nil {
		t.Fatal(err)
	}
	k := map[string]KeyUsageRow{}
	for _, r := range byKey.Body.Rows {
		k[r.Key] = r
	}
	if math.Abs(k["a"].CacheHitRate-0.75) > 0.001 || k["a"].CacheReports != 1 {
		t.Errorf("key a = rate %v / reports %d, want 0.75 / 1",
			k["a"].CacheHitRate, k["a"].CacheReports)
	}
	if k["b"].CacheReports != 0 {
		t.Errorf("key b reports = %d, want 0", k["b"].CacheReports)
	}
}
