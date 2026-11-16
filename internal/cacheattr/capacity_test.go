package cacheattr

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePropsTotalSlots(t *testing.T) {
	n, err := parseProps(strings.NewReader(string(mustRead(t, "props.json"))))
	if err != nil || n != 3 {
		t.Fatalf("total_slots = %d, %v; want 3", n, err)
	}
	if n, err := parseProps(strings.NewReader(`{"model_path":"x"}`)); err != nil || n != -1 {
		t.Errorf("no total_slots = %d, %v; want -1", n, err)
	}
}

// Swift's /metrics (patches p3/p4/p5) carries the cache gauges and the
// eviction-age histogram; stock llama-server carries none of them.
func TestParseMetricsFixtures(t *testing.T) {
	s, ok := parseMetrics(strings.NewReader(string(mustRead(t, "metrics_patched.txt"))))
	if !ok || s.entries != 12 || s.bytes != 27_400_500_000 || s.arenaFree != 3_636_461_568 ||
		s.youngEvicted != 48 || s.evictedCount != 228 || s.evictions <= 0 {
		t.Fatalf("patched = %+v %v", s, ok)
	}
	s, ok = parseMetrics(strings.NewReader(string(mustRead(t, "metrics_stock.txt"))))
	if ok || s.entries != -1 || s.arenaFree != -1 || s.evictedCount != -1 {
		t.Fatalf("stock = %+v %v", s, ok)
	}
}

// Swift's geometry, from its startup log (proc's logbuf test parses the
// same lines): 5984 MiB / 180224 cells target + 704 MiB / 180224 draft;
// 2244.38 MiB of recurrent state over 3 cells × (1 + 4 rs_seq); checkpoint
// 149.646 MiB; per-slot cap 86016; cache limit 27648 MiB.
var swiftGeo = Geometry{
	NCtxSlot:        86016,
	KVCellBytes:     (5984.0 + 704.0) * (1 << 20) / 180224,
	RSSeqBytes:      2244.38 * (1 << 20) / 15,
	CheckpointBytes: 149.646 * (1 << 20),
	CacheRAMBytes:   27648 << 20,
}

const swiftCmd = "llama-server --parallel 3 --kv-unified -c 180224 --cache-ram 27648 --ctx-checkpoints 8"

// worst = 149.65 + 86016 × 38 KiB (3192 MiB) + 8 × 149.65 ≈ 4538.8 MiB;
// 27648 / 4538.8 = 6.09 → 3 + 6 = 9.
func TestWorstEntrySwift(t *testing.T) {
	w, counted := worstEntry(swiftCmd, swiftGeo)
	if mib := float64(w) / (1 << 20); !counted || mib < 4538 || mib > 4540 {
		t.Fatalf("worst entry = %.1f MiB counted=%v, want ≈4538.8", mib, counted)
	}
	c := instanceCapacity(3, nil, swiftCmd, swiftGeo)
	if c.CacheSlots != 9 || c.BudgetBytes != 27648<<20 || c.CacheSlotsTypical != 0 {
		t.Fatalf("capacity = %+v, want cache slots 9 and no typical without samples", c)
	}
	// The composition predicts a logged save: 28674 tokens with no
	// checkpoints → 1215.013 MiB ("total state size").
	got := (swiftGeo.RSSeqBytes + 28674*swiftGeo.KVCellBytes) / (1 << 20)
	if math.Abs(got-1215.013) > 2 { // 1.3 MiB of per-sequence headers
		t.Errorf("predicted save %.3f MiB, logged 1215.013", got)
	}
}

func TestWorstEntryCheckpoints(t *testing.T) {
	g := swiftGeo
	g.CheckpointBytes = 0 // none logged yet: the recurrent state stands in
	if w1, _ := worstEntry(swiftCmd, g); w1 <= 0 {
		t.Fatal("no stand-in")
	}
	// llama.cpp's default 32 when the flag is absent.
	w8, _ := worstEntry(swiftCmd, swiftGeo)
	w32, _ := worstEntry("llama-server", swiftGeo)
	if d := float64(w32-w8) / swiftGeo.CheckpointBytes; math.Abs(d-24) > 0.01 {
		t.Errorf("default checkpoints added %.2f, want 24", d)
	}
	// Dense model, no checkpoint seen: left out, and said so.
	dense := Geometry{NCtxSlot: 1000, KVCellBytes: 1024}
	if w, counted := worstEntry("x", dense); w != 1000*1024 || counted {
		t.Errorf("dense = %d %v", w, counted)
	}
	if _, counted := worstEntry("x -ctxcp 0", dense); !counted {
		t.Error("--ctx-checkpoints 0: nothing to count, so counted")
	}
	if w, _ := worstEntry("x", Geometry{KVCellBytes: 1}); w != 0 {
		t.Error("no context cap: want unknown")
	}
}

func TestCacheBudgetPicksTightest(t *testing.T) {
	const g = 1 << 30
	last := &sample{bytes: 20 * g, arenaFree: 4 * g}
	if b := cacheBudget(last, "", Geometry{}); b != 24*g {
		t.Errorf("metrics only = %d", b)
	}
	if b := cacheBudget(last, "-cram 8192", Geometry{}); b != 8*g {
		t.Errorf("flag tighter = %d", b)
	}
	if b := cacheBudget(nil, "", Geometry{CacheRAMBytes: 27648 << 20}); b != 27648<<20 {
		t.Errorf("log only = %d", b)
	}
	if b := cacheBudget(nil, "--cache-ram 0", Geometry{}); b != 0 {
		t.Errorf("cache off = %d, want 0", b)
	}
	if b := cacheBudget(nil, "--cache-ram -1", Geometry{}); b != -1 {
		t.Errorf("no limit, nothing else = %d, want unknown", b)
	}
}

func TestTypicalMathAndSmoothing(t *testing.T) {
	const g = 1 << 30
	smp := func(entries, bytes, free int64) sample {
		return sample{entries: entries, bytes: bytes, arenaFree: free, youngEvicted: -1, evictedCount: -1}
	}
	// Mean of per-sample means: 2 and 4 GiB → 3; the 2-entry sample is too
	// few. Budget from the newest: 12 + 0 → 4 entries + 2 slots.
	ring := []sample{smp(4, 8*g, 0), smp(2, 1*g, 0), smp(3, 12*g, 0)}
	c := instanceCapacity(2, ring, "", Geometry{})
	if c.CacheSlotsTypical != 6 || c.MeanEntryBytes != 3*g || c.BudgetBytes != 12*g || c.CacheSlots != 0 {
		t.Fatalf("capacity = %+v", c)
	}
	// One outlier among ten moves the mean by a tenth.
	ring = nil
	for range 9 {
		ring = append(ring, smp(4, 8*g, 4*g))
	}
	ring = append(ring, smp(3, 36*g, 0)) // 12 GiB/entry
	if c := instanceCapacity(1, ring, "", Geometry{}); c.MeanEntryBytes != 3*g || c.CacheSlotsTypical != 13 {
		t.Errorf("smoothed = %+v", c)
	}
	if c := instanceCapacity(1, []sample{smp(2, 4*g, 8*g)}, "", Geometry{}); c.CacheSlotsTypical != 0 {
		t.Errorf("insufficient data = %+v, want omitted", c)
	}
}

func TestChurnOf(t *testing.T) {
	ring := []sample{
		{youngEvicted: -1, evictedCount: -1},
		{youngEvicted: 10, evictedCount: 100},
		{youngEvicted: 40, evictedCount: 140},
	}
	if y, n, ok := churnOf(ring); !ok || y != 30 || n != 40 {
		t.Errorf("churn = %d/%d %v", y, n, ok)
	}
	if _, _, ok := churnOf([]sample{{youngEvicted: -1, evictedCount: -1}}); ok {
		t.Error("no histogram reported churn")
	}
	if _, _, ok := churnOf([]sample{{youngEvicted: 5, evictedCount: 50}, {youngEvicted: 0, evictedCount: 1}}); ok {
		t.Error("a counter reset reported churn")
	}
}

// End to end over HTTP: Swift's fixtures give slots, both cache counts and
// churn; a stock backend (no geometry, no cache metrics) gives slots only; a
// backend with no /props gives nothing.
func TestPollerCapacity(t *testing.T) {
	props, patched, stock := mustRead(t, "props.json"), mustRead(t, "metrics_patched.txt"), mustRead(t, "metrics_stock.txt")
	var propsAsked atomic.Int64
	serve := func(metrics []byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/props":
				propsAsked.Add(1)
				_, _ = w.Write(props)
			case "/metrics":
				_, _ = w.Write(metrics)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a, b := serve(patched), serve(stock)
	defer a.Close()
	defer b.Close()
	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	p := NewPoller(func() []Target {
		return []Target{
			{Served: "swift", Instance: "a", BaseURL: a.URL, Cmd: swiftCmd, Geometry: swiftGeo},
			{Served: "stock", Instance: "b", BaseURL: b.URL},
			{Served: "vllm", Instance: "c", BaseURL: none.URL},
		}
	}, 0)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())

	c, ok := p.Capacity("swift")
	// Guaranteed 9 (see TestWorstEntrySwift). Typical: mean 27.4005e9/12 =
	// 2283375000 over budget min(27648 MiB, bytes+free 31.04e9) → 3 + 12.
	if !ok || c.Slots != 3 || c.CacheSlots != 9 || !c.CheckpointsCounted || c.CacheSlotsTypical != 15 ||
		c.BudgetBytes != 27648<<20 || c.MeanEntryBytes != 2_283_375_000 {
		t.Fatalf("swift = %+v %v", c, ok)
	}
	if c.Churn != 0 || c.ChurnEvictions != 0 {
		t.Errorf("two equal samples: churn %+v, want 0 of 0", c)
	}
	if c, ok := p.Capacity("stock"); !ok || c.Slots != 3 || c.CacheSlots != 0 || c.CacheSlotsTypical != 0 || c.Churn != -1 {
		t.Errorf("stock = %+v %v", c, ok)
	}
	if _, ok := p.Capacity("vllm"); ok {
		t.Error("a backend without /props reported slots")
	}
	if propsAsked.Load() != 2 {
		t.Errorf("/props asked %d times, want once per instance", propsAsked.Load())
	}
}
