package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestActivityRoundTrip persists a metered record and reads it back, covering
// the P6 token/cost columns.
func TestActivityRoundTrip(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	in := Activity{
		TS: 1, Served: "m", Key: "k", SourceIP: "192.168.1.160", Path: "/v1/chat/completions",
		Status: 200, DwellMS: 42, PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.00105,
		CachedTokens: 7, PromptPerSec: 123.5, PredictedPerSec: 45.25,
	}
	if err := st.InsertActivity(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecentActivity(Window{}, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	in.ID = 1 // RecentActivity now returns the autoincrement id (P10b)
	if got[0] != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got[0], in)
	}

	// ActivityByID returns the full row including captured payloads.
	full := Activity{TS: 2, Served: "m", Status: 200, SourceIP: "10.0.0.5",
		ReqBody: `{"model":"m"}`, RespBody: "hi", TTFBMs: 12,
		CachedTokens: 3, PromptPerSec: 200.0, PredictedPerSec: 60.5}
	if err := st.InsertActivity(full); err != nil {
		t.Fatal(err)
	}
	got2, err := st.ActivityByID(2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.ID != 2 || got2.ReqBody != `{"model":"m"}` || got2.RespBody != "hi" || got2.TTFBMs != 12 || got2.SourceIP != "10.0.0.5" {
		t.Errorf("ActivityByID = %+v", got2)
	}
	if got2.CachedTokens != 3 || got2.PromptPerSec != 200.0 || got2.PredictedPerSec != 60.5 {
		t.Errorf("ActivityByID telemetry = cached %d, tp/s %v, tg/s %v", got2.CachedTokens, got2.PromptPerSec, got2.PredictedPerSec)
	}

	// The served filter scopes RecentActivity to one model (per-model console
	// usage tab); empty served returns every model. Two "m" rows + one "other".
	if err := st.InsertActivity(Activity{TS: 3, Served: "other", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.RecentActivity(Window{}, 10, "", "", ""); len(all) != 3 {
		t.Fatalf("unfiltered should see all models, got %d", len(all))
	}
	only, _ := st.RecentActivity(Window{}, 10, "m", "", "")
	if len(only) != 2 {
		t.Fatalf("served=m should return only model m's rows, got %d", len(only))
	}
	for _, r := range only {
		if r.Served != "m" {
			t.Errorf("served filter leaked a %q row", r.Served)
		}
	}
}

// TestRollupByModel aggregates per served model, ordered by cost desc, and
// honors the since cutoff.
func TestRollupByModel(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "cheap", Status: 200, PromptTokens: 10, CompletionTokens: 5, DwellMS: 50, CostUSD: 0.001},
		{TS: 200, Served: "cheap", Status: 200, PromptTokens: 10, CompletionTokens: 5, DwellMS: 50, CostUSD: 0.001},
		{TS: 300, Served: "pricey", Status: 200, PromptTokens: 100, CompletionTokens: 50, DwellMS: 500, CostUSD: 0.5},
		{TS: 50, Served: "old", Status: 200, CostUSD: 9.9}, // before cutoff
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// Cutoff at ts=100 excludes "old"; pricey outranks cheap by cost.
	got, err := st.RollupSlice(Since(100), PerModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(got), got)
	}
	if got[0].Served != "pricey" || got[0].CostUSD != 0.5 {
		t.Errorf("first group = %+v, want pricey/0.5", got[0])
	}
	if got[1].Served != "cheap" || got[1].Requests != 2 || got[1].PromptTokens != 20 || got[1].DwellMS != 100 {
		t.Errorf("cheap aggregation = %+v", got[1])
	}

	// sinceMS=0 includes everything.
	all, err := st.RollupSlice(Window{}, PerModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 groups all-time, got %d", len(all))
	}
}

// TestRollupByModelSpeeds covers the tp/s and tg/s means, and the one thing that
// makes them trustworthy: a request that reported NO rate must not be averaged in
// as a zero. Only llama.cpp backends report timings — every remote provider and
// every embedding model leaves both columns at their 0 default — so counting
// those would drag a healthy 42 tg/s toward 0 in exact proportion to how much
// remote traffic the box carried that hour, and the Overview slowdown card would
// read the box as degrading whenever callers shifted to a paid backend.
func TestRollupByModelSpeeds(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		// Two requests that reported: means are 3000 tp/s and 40 tg/s.
		{TS: 100, Served: "local", Status: 200, PromptPerSec: 2000, PredictedPerSec: 30},
		{TS: 200, Served: "local", Status: 200, PromptPerSec: 4000, PredictedPerSec: 50},
		// A third that reported nothing. It must be skipped, not averaged as 0.
		{TS: 300, Served: "local", Status: 200},
		// A backend that never reports at all reads as 0 = "unreported".
		{TS: 400, Served: "remote", Status: 200},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RollupSlice(Window{}, PerModel)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Rollup{}
	for _, r := range got {
		by[r.Served] = r
	}

	local := by["local"]
	if local.Requests != 3 {
		t.Fatalf("local requests = %d, want 3", local.Requests)
	}
	// 3000, not 2000: the silent request is excluded from the mean but still
	// counted as a request.
	if local.PromptPerSec != 3000 {
		t.Errorf("tp/s = %v, want 3000 (silent request must not average in as 0)", local.PromptPerSec)
	}
	if local.PredictedPerSec != 40 {
		t.Errorf("tg/s = %v, want 40 (silent request must not average in as 0)", local.PredictedPerSec)
	}
	if r := by["remote"]; r.PromptPerSec != 0 || r.PredictedPerSec != 0 {
		t.Errorf("a backend that reports no timings = %+v, want both 0 (unreported)", r)
	}
}

// TestRollupByKey aggregates per caller key, ordered by cost desc.
func TestRollupByKey(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.5, DwellMS: 100, PromptTokens: 5},
		{TS: 200, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.01, DwellMS: 5},
		{TS: 300, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.01, DwellMS: 5},
		{TS: 400, Served: "m", Key: "", Status: 200, CostUSD: 0.02}, // unkeyed
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RollupByKey(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d: %+v", len(got), got)
	}
	if got[0].Key != "aw3" || got[0].CostUSD != 0.5 {
		t.Errorf("top key = %+v, want aw3/0.5", got[0])
	}
	// ragtag aggregates its two rows.
	for _, r := range got {
		if r.Key == "ragtag" && (r.Requests != 2 || r.DwellMS != 10) {
			t.Errorf("ragtag rollup = %+v", r)
		}
		// Last-seen is the NEWEST row, not the first or the sum. A key with a
		// large historical count and nothing recent is a different problem from
		// one that is busy now, and the count alone cannot tell them apart.
		if r.Key == "ragtag" && r.LastSeenMS != 300 {
			t.Errorf("ragtag last seen = %d, want 300 (newest of 200/300)", r.LastSeenMS)
		}
		if r.Key == "aw3" && r.LastSeenMS != 100 {
			t.Errorf("aw3 last seen = %d, want 100", r.LastSeenMS)
		}
	}
}

// The key filter is what makes the roster actionable: "this key did 9,971
// requests" is only useful if you can then ask what they were.
func TestRecentActivityKeyFilter(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 100, Served: "m", Key: "aw3", Status: 200},
		{TS: 200, Served: "m", Key: "ragtag", Status: 200},
		{TS: 300, Served: "other", Key: "ragtag", Status: 200},
		{TS: 400, Served: "other", Key: "", Status: 200},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RecentActivity(Window{}, 10, "", "ragtag", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("key=ragtag should return 2 rows, got %d", len(got))
	}
	for _, r := range got {
		if r.Key != "ragtag" {
			t.Errorf("key filter leaked a %q row", r.Key)
		}
	}

	// Both filters compose — the alternative was a branch per combination, and
	// the pair that nobody wired is exactly the one an operator reaches for
	// ("what did this caller do ON THIS MODEL").
	both, _ := st.RecentActivity(Window{}, 10, "other", "ragtag", "")
	if len(both) != 1 || both[0].TS != 300 {
		t.Fatalf("served+key should intersect, got %+v", both)
	}

	// An empty key means "no filter", not "unkeyed traffic only" — the roster
	// never asks for the latter, and treating "" as a value would silently
	// return one row where the caller expected every row.
	if all, _ := st.RecentActivity(Window{}, 10, "", "", ""); len(all) != 4 {
		t.Fatalf("empty key must not filter, got %d rows", len(all))
	}
}

// TestRollupSeriesQueueMetrics aggregates queue wait + 429 rejections per bucket.
func TestRollupSeriesQueueMetrics(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows := []Activity{
		{TS: 1000, Served: "m", Key: "k", Status: 200, QueuedMS: 0},
		{TS: 1500, Served: "m", Key: "k", Status: 200, QueuedMS: 500},  // queued then served
		{TS: 1800, Served: "m", Key: "k", Status: 429, QueuedMS: 1000}, // queued then rejected
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	// One wide bucket covering all three.
	got, err := st.RollupSeries(Window{}, 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (bucket,key) row, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Requests != 3 || r.Rejected != 1 || r.QueuedMS != 1500 {
		t.Errorf("got requests=%d rejected=%d queuedMs=%d, want 3/1/1500", r.Requests, r.Rejected, r.QueuedMS)
	}
}

// TestLaneSamples: samples aggregate to mean/peak per (bucket, group), and
// pruning drops old rows.
func TestLaneSamples(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Two samples in the same bucket for "interactive": waiting 2 then 6.
	if err := st.InsertLaneSamples(1000, []LaneSample{{Group: "interactive", Active: 1, Waiting: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertLaneSamples(2000, []LaneSample{{Group: "interactive", Active: 1, Waiting: 6}}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LaneDepthSeries(Window{}, 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (bucket,group), got %d", len(got))
	}
	r := got[0]
	if r.Group != "interactive" || r.AvgWaiting != 4 || r.MaxWaiting != 6 {
		t.Errorf("got %+v, want interactive avgWaiting=4 maxWaiting=6", r)
	}

	// Prune everything before ts 5000 → empty.
	if err := st.PruneLaneSamples(5000); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LaneDepthSeries(Window{}, 3_600_000); len(got) != 0 {
		t.Errorf("after prune want 0 rows, got %d", len(got))
	}
}

// TestPruneActivity deletes rows older than the cutoff, keeping recent ones.
func TestPruneActivity(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, ts := range []int64{100, 200, 5000, 6000} {
		if err := st.InsertActivity(Activity{TS: ts, Served: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.PruneActivity(1000) // drop ts < 1000 (the 100 and 200 rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	got, _ := st.RecentActivity(Window{}, 10, "", "", "")
	if len(got) != 2 {
		t.Errorf("remaining %d rows, want 2", len(got))
	}
}

// TestMigrationsIdempotent: Open applies the upgrade migrations and is safe to
// call repeatedly against the same database (duplicate-column errors swallowed).
func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	// A shared in-memory DB across connections via the file: DSN.
	dsn := "file:store_migrate_test?mode=memory&cache=shared"
	a, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = a.Close() }()
	if _, err := Open(ctx, dsn); err != nil {
		t.Fatalf("second open (migrations must be idempotent): %v", err)
	}
}

// The Keys page aggregates every row per key. On a table whose rows carry
// captured payloads that is gigabytes of heap fetches unless the index carries
// the summed columns — and with a single connection it blocks writers for as
// long as it runs. Assert the plan is COVERING, which is the property that
// makes it cheap; a future column added to the rollup without being added to
// the index silently reintroduces the 2-second page.
func TestKeyRollupIsIndexOnly(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.db.Query(`EXPLAIN QUERY PLAN
		SELECT key, COUNT(*), MAX(ts), SUM(prompt_tokens), SUM(completion_tokens),
		       SUM(dwell_ms), SUM(cost_usd), SUM(cached_tokens)
		FROM activity WHERE ts >= 0 GROUP BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "COVERING INDEX idx_activity_key_rollup") {
		t.Errorf("the per-key rollup is not index-only; it will fetch every row from a table of captured payloads:\n%s", plan.String())
	}
}

// The superseded index must not survive an upgrade: idx_activity_key_rollup
// begins with the same (key, ts), so keeping both charges every insert twice
// for one access path.
func TestSupersededKeyIndexIsDroppedOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// Put the old index back, as an upgraded database has it.
	if _, err := st.db.Exec(`CREATE INDEX IF NOT EXISTS idx_activity_key_ts ON activity(key, ts)`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	var n int
	if err := st2.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_activity_key_ts'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("idx_activity_key_ts survived the upgrade; every insert now maintains two indexes for one access path")
	}
}

// TestRecentActivityPageOffsetsThroughPages pages through a known set of rows
// a few at a time and checks both the ordering within each page and hasMore's
// transition from true to false, including the boundary where the offset
// lands past the last row.
func TestRecentActivityPageOffsetsThroughPages(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const total = 7
	for i := 1; i <= total; i++ {
		if err := st.InsertActivity(Activity{TS: int64(i), Served: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}

	const pageSize = 3
	var seen []int64
	offset := 0
	for pages := 0; ; pages++ {
		if pages > total { // guard against an infinite loop if hasMore never clears
			t.Fatal("paged past the row count without hasMore going false")
		}
		page, hasMore, err := st.RecentActivityPage(Window{}, pageSize, offset, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			seen = append(seen, a.TS)
		}
		wantMore := offset+len(page) < total
		if hasMore != wantMore {
			t.Fatalf("offset %d: hasMore = %v, want %v (returned %d rows)", offset, hasMore, wantMore, len(page))
		}
		if !hasMore {
			break
		}
		if len(page) != pageSize {
			t.Fatalf("offset %d: got %d rows on a non-final page, want %d", offset, len(page), pageSize)
		}
		offset += len(page)
	}
	want := []int64{7, 6, 5, 4, 3, 2, 1} // newest (highest ts) first, across every page
	if len(seen) != len(want) {
		t.Fatalf("saw %d rows across all pages, want %d: %v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("row %d across pages: got ts %d, want %d (full sequence %v)", i, seen[i], want[i], seen)
		}
	}

	// Offset landing past the end: no rows, and definitely no more of them.
	past, hasMore, err := st.RecentActivityPage(Window{}, pageSize, total+5, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(past) != 0 || hasMore {
		t.Errorf("offset past the end: got %d rows, hasMore %v, want 0 rows and hasMore false", len(past), hasMore)
	}

	// Offset landing exactly on the last row boundary: one row, no more after it.
	last, hasMore, err := st.RecentActivityPage(Window{}, pageSize, total-1, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].TS != 1 || hasMore {
		t.Errorf("offset at the last row: got %d rows (ts %v), hasMore %v, want 1 row (ts 1), hasMore false", len(last), last, hasMore)
	}
}

// TestGPUCompute summarises samples per bucket and pool: first, last, low,
// high, mean and the trend in points per minute (P56).
func TestGPUCompute(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// Bucket [0, 600 000): 10, 30, 50 one minute apart; then one in the next.
	for i, u := range []int{10, 30, 50} {
		if err := st.InsertGPUSamples(int64(i)*60_000, []GPUSample{{Server: "box", Pool: "gpu0", UtilPct: u}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertGPUSamples(600_000, []GPUSample{{Server: "box", Pool: "gpu0", UtilPct: 90}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.GPUCompute(Window{}, 600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.N != 3 || r.Open != 10 || r.Close != 50 || r.Low != 10 || r.High != 50 || r.Mean != 30 || r.SlopePerMin != 20 {
		t.Errorf("bucket 0 = %+v, want n3 o10 c50 l10 h50 mean30 slope20", r)
	}
	if rows[1].N != 1 || rows[1].SlopePerMin != 0 {
		t.Errorf("bucket 1 = %+v, want one sample and no slope", rows[1])
	}
}
