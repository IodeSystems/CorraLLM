package store

import (
	"context"
	"testing"
)

func payloadStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The whole point: the payload goes, the ROW and every number on it stay.
//
// Dropping the row to reclaim the payload would take the analytics with it —
// rollups, series, the caller roster. This takes only the part that has stopped
// being read.
func TestPrunePayloadsKeepsTheRowAndItsMetrics(t *testing.T) {
	st := payloadStore(t)
	old := Activity{
		TS: 1000, Served: "Qwen3.8-27B", Key: "dun", Path: "/v1/chat/completions",
		Status: 200, DwellMS: 4200, PromptTokens: 91000, CompletionTokens: 512,
		CostUSD: 0.0134, QueuedMS: 300, CachedTokens: 88000, TTFBMs: 900,
		ReqBody: `{"model":"Qwen3.8-27B","messages":[…240KiB…]}`, RespBody: "hello",
	}
	if err := st.InsertActivity(old); err != nil {
		t.Fatal(err)
	}

	n, err := st.PrunePayloads(2000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cleared %d rows, want 1", n)
	}

	got, err := st.ActivityByID(1)
	if err != nil {
		t.Fatalf("the row must still exist: %v", err)
	}
	if got.ReqBody != "" || got.RespBody != "" {
		t.Errorf("payloads survived: req=%q resp=%q", got.ReqBody, got.RespBody)
	}
	// Every metric is read for as long as the row is retained.
	if got.Status != 200 || got.DwellMS != 4200 || got.PromptTokens != 91000 ||
		got.CompletionTokens != 512 || got.CostUSD != 0.0134 || got.QueuedMS != 300 ||
		got.CachedTokens != 88000 || got.TTFBMs != 900 {
		t.Errorf("a metric was lost with the payload: %+v", got)
	}
	if got.Served != "Qwen3.8-27B" || got.Key != "dun" {
		t.Errorf("identity lost: served=%q key=%q", got.Served, got.Key)
	}
}

// Recent rows keep their payloads — replay is the one thing they are for, and it
// is only ever wanted on something recent.
func TestPrunePayloadsLeavesRecentRowsAlone(t *testing.T) {
	st := payloadStore(t)
	if err := st.InsertActivity(Activity{
		TS: 5000, Served: "m", Status: 200, ReqBody: `{"keep":true}`, RespBody: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PrunePayloads(2000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cleared %d rows, want 0 — this row is newer than the cutoff", n)
	}
	got, err := st.ActivityByID(1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReqBody != `{"keep":true}` || got.RespBody != "ok" {
		t.Errorf("a recent payload was cleared: req=%q resp=%q", got.ReqBody, got.RespBody)
	}
}

// A second pass over already-cleared rows must report zero.
//
// Not cosmetic. Without the IS-NOT-NULL guard the UPDATE matches on timestamp
// alone, so every pass rewrites every old row it already cleared — turning a
// one-off cleanup into a permanent write load on a five-minute timer, on the
// largest table in the database.
func TestPrunePayloadsIsIdempotent(t *testing.T) {
	st := payloadStore(t)
	for i := range 3 {
		if err := st.InsertActivity(Activity{
			TS: int64(100 + i), Served: "m", Status: 200, ReqBody: "body", RespBody: "resp",
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.PrunePayloads(1000)
	if err != nil {
		t.Fatal(err)
	}
	if first != 3 {
		t.Fatalf("first pass cleared %d, want 3", first)
	}
	second, err := st.PrunePayloads(1000)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("second pass cleared %d, want 0 — it is rewriting rows it already cleared", second)
	}
}

// A row that never carried a payload (capture disabled, or an audio request
// summarized to a size) must not be counted as work done.
func TestPrunePayloadsSkipsRowsThatNeverHadOne(t *testing.T) {
	st := payloadStore(t)
	if err := st.InsertActivity(Activity{TS: 100, Served: "m", Status: 200}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PrunePayloads(1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cleared %d rows, want 0", n)
	}
}

// Clearing one half is still work. A row that captured a request but no response
// (a 499, a streamed reply whose capture was disabled) must be reached.
func TestPrunePayloadsClearsARowWithOnlyARequestBody(t *testing.T) {
	st := payloadStore(t)
	if err := st.InsertActivity(Activity{
		TS: 100, Served: "m", Status: 499, ReqBody: "just the request",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PrunePayloads(1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cleared %d rows, want 1", n)
	}
	got, err := st.ActivityByID(1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReqBody != "" {
		t.Errorf("req_body survived: %q", got.ReqBody)
	}
}

// A backlog larger than one chunk must drain completely.
//
// The loop returns when a pass clears fewer rows than the chunk size, so an
// off-by-one there would strand the tail — and the tail is exactly the oldest,
// largest backlog this feature exists to clear.
func TestPrunePayloadsDrainsPastOneChunk(t *testing.T) {
	st := payloadStore(t)
	const n = payloadPruneChunk + 37
	for i := range n {
		if err := st.InsertActivity(Activity{
			TS: int64(i), Served: "m", Status: 200, ReqBody: "body", RespBody: "resp",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cleared, err := st.PrunePayloads(int64(n) + 1)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != n {
		t.Fatalf("cleared %d of %d — the tail past the first chunk was stranded", cleared, n)
	}
	// And nothing is left behind.
	again, err := st.PrunePayloads(int64(n) + 1)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second pass cleared %d, want 0", again)
	}
}
