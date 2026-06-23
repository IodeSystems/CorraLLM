package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// TestRecentActivity maps store rows to API records newest-first and honors the
// limit (defaulting when unset).
func TestRecentActivity(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for i := 1; i <= 3; i++ {
		if err := st.InsertActivity(store.Activity{
			TS: int64(i), Served: "m", Backend: "m#0", Key: "k",
			Path: "/v1/chat/completions", Status: 200,
			DwellMS: int64(i * 10), PromptTokens: i, CompletionTokens: i, CostUSD: float64(i) * 0.001,
		}); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handlers{Store: st}

	// Default limit when unset.
	out, err := h.RecentActivity(context.Background(), &RecentActivityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Records) != 3 {
		t.Fatalf("want 3 records, got %d", len(out.Body.Records))
	}
	// Newest first: TS 3, 2, 1.
	if out.Body.Records[0].TS != 3 || out.Body.Records[2].TS != 1 {
		t.Errorf("not newest-first: %d..%d", out.Body.Records[0].TS, out.Body.Records[2].TS)
	}
	// Metering fields carried through.
	if out.Body.Records[0].CostUSD != 0.003 || out.Body.Records[0].DwellMS != 30 {
		t.Errorf("metering not carried: %+v", out.Body.Records[0])
	}

	// Explicit limit bounds the result.
	out, err = h.RecentActivity(context.Background(), &RecentActivityInput{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Records) != 2 {
		t.Fatalf("want 2 records, got %d", len(out.Body.Records))
	}
}

// TestUsageRollup checks the grand total and that the window filters old rows.
func TestUsageRollup(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now()
	// Recent rows (within a 1h window) + one 2h-old row (outside it).
	if err := st.InsertActivity(store.Activity{TS: now.UnixMilli(), Served: "a", Status: 200, CostUSD: 0.10, PromptTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertActivity(store.Activity{TS: now.UnixMilli(), Served: "b", Status: 200, CostUSD: 0.20, PromptTokens: 7}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertActivity(store.Activity{TS: now.Add(-2 * time.Hour).UnixMilli(), Served: "old", Status: 200, CostUSD: 9.9}); err != nil {
		t.Fatal(err)
	}

	h := &Handlers{Store: st}

	// All-time: 3 models, total cost includes the old row.
	all, err := h.UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Body.Rows) != 3 {
		t.Fatalf("all-time want 3 rows, got %d", len(all.Body.Rows))
	}
	if all.Body.Total.CostUSD != 10.2 {
		t.Errorf("all-time total = %v, want 10.2", all.Body.Total.CostUSD)
	}

	// 1h window: drops the old row; total = 0.30.
	win, err := h.UsageRollup(context.Background(), &UsageRollupInput{WindowHours: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Body.Rows) != 2 {
		t.Fatalf("windowed want 2 rows, got %d", len(win.Body.Rows))
	}
	if win.Body.Total.CostUSD < 0.2999 || win.Body.Total.CostUSD > 0.3001 {
		t.Errorf("windowed total = %v, want ~0.30", win.Body.Total.CostUSD)
	}
	// Costliest first.
	if win.Body.Rows[0].Served != "b" {
		t.Errorf("first row = %s, want b", win.Body.Rows[0].Served)
	}
}

// TestUsageByKey aggregates per key and derives energy from cost/costPerKwh.
func TestUsageByKey(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now()
	_ = st.InsertActivity(store.Activity{TS: now.UnixMilli(), Served: "m", Key: "aw3", Status: 200, CostUSD: 0.14})
	_ = st.InsertActivity(store.Activity{TS: now.UnixMilli(), Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.07})

	h := &Handlers{Store: st, Cfg: &config.Config{CostPerKwh: 0.14}}
	out, err := h.UsageByKey(context.Background(), &UsageByKeyInput{WindowHours: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Rows) != 2 {
		t.Fatalf("want 2 keys, got %d", len(out.Body.Rows))
	}
	top := out.Body.Rows[0]
	if top.Key != "aw3" || top.CostUSD != 0.14 {
		t.Errorf("top = %+v, want aw3/0.14", top)
	}
	// energy = cost / costPerKwh = 0.14 / 0.14 = 1 kWh.
	if top.EnergyKwh < 0.999 || top.EnergyKwh > 1.001 {
		t.Errorf("energyKwh = %v, want ~1", top.EnergyKwh)
	}
}

// TestOverview renders config into model/lane defs + capacity, flagging
// spawnable backends and summarizing stage policy.
func TestOverview(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"box": {Pools: map[string]string{"gpu0": "24GB"}, Reserve: map[string]string{"gpu0": "1GB"}},
		},
		Models: map[string]config.Model{
			"m": {Backends: []config.Backend{
				{Cmd: "llama-server ...", Server: "box", Type: "local", Quality: 100, MaxTokens: 512},
				{Type: "claude", Quality: 90}, // pure-proxy (no cmd)
			}},
		},
		PriorityGroups: map[string]config.PriorityGroup{
			"batch": {Weight: 1, Interruptible: true, OnSaturated: map[string]config.Stage{"local": {Queue: true}}},
		},
		Keys: map[string]string{"ragtag": "batch"},
	}
	h := &Handlers{Cfg: cfg}
	out, err := h.Overview(context.Background(), &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Body.Servers) != 1 || out.Body.Servers[0].Pools[0].TotalBytes != 24*1000*1000*1000 {
		t.Fatalf("servers = %+v", out.Body.Servers)
	}
	if len(out.Body.Models) != 1 {
		t.Fatalf("models = %+v", out.Body.Models)
	}
	md := out.Body.Models[0]
	if !md.Spawnable || len(md.Backends) != 2 {
		t.Fatalf("model = %+v", md)
	}
	if !md.Backends[0].Spawnable || md.Backends[0].Cmd == "" || md.Backends[0].MaxTokens != 512 {
		t.Errorf("backend0 = %+v", md.Backends[0])
	}
	if md.Backends[1].Spawnable {
		t.Errorf("backend1 (pure-proxy) should not be spawnable: %+v", md.Backends[1])
	}
	if len(out.Body.Groups) != 1 || out.Body.Groups[0].Stages[0].Policy != "queue" {
		t.Errorf("groups = %+v", out.Body.Groups)
	}
	if len(out.Body.Keys) != 1 || out.Body.Keys[0].Group != "batch" {
		t.Errorf("keys = %+v", out.Body.Keys)
	}
}

// TestModelActionHandlers covers the load/unload wrappers' result mapping
// without spawning: unknown model fails gracefully; unloading an absent model
// is a no-op success.
func TestModelActionHandlers(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Backends: []config.Backend{{Type: "local"}}}}}
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}

	ld, err := h.LoadModel(context.Background(), actionInput("nope"))
	if err != nil {
		t.Fatal(err)
	}
	if ld.Body.OK || !strings.Contains(ld.Body.Message, "unknown") {
		t.Errorf("load unknown = %+v", ld.Body)
	}

	ul, err := h.UnloadModel(context.Background(), actionInput("m"))
	if err != nil {
		t.Fatal(err)
	}
	if !ul.Body.OK || ul.Body.Evicted != 0 {
		t.Errorf("unload absent = %+v", ul.Body)
	}
}

func actionInput(model string) *ModelActionInput {
	in := &ModelActionInput{}
	in.Body.Model = model
	return in
}

// TestUsageSeries builds a dense, ascending bucket axis and aligns each key's
// points to it, with energy derived from cost.
func TestUsageSeries(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.14})
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.14})
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.07})

	h := &Handlers{Store: st, Cfg: &config.Config{CostPerKwh: 0.14}}
	out, err := h.UsageSeries(context.Background(), &UsageSeriesInput{WindowHours: 2, BucketMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Body.Buckets) == 0 {
		t.Fatal("no buckets")
	}
	for i := 1; i < len(out.Body.Buckets); i++ {
		if out.Body.Buckets[i] <= out.Body.Buckets[i-1] {
			t.Fatalf("buckets not strictly ascending at %d", i)
		}
	}
	if len(out.Body.Keys) != 2 || out.Body.Keys[0].Key != "aw3" {
		t.Fatalf("keys = %+v, want aw3 first", out.Body.Keys)
	}
	// Each key's points align to the axis; sum recovers the totals.
	for _, ks := range out.Body.Keys {
		if len(ks.Points) != len(out.Body.Buckets) {
			t.Fatalf("%s points len %d != buckets %d", ks.Key, len(ks.Points), len(out.Body.Buckets))
		}
		var cost, energy float64
		var reqs int64
		for _, p := range ks.Points {
			cost += p.CostUSD
			energy += p.EnergyKwh
			reqs += p.Requests
		}
		switch ks.Key {
		case "aw3":
			if cost < 0.279 || cost > 0.281 || reqs != 2 {
				t.Errorf("aw3 totals cost=%v reqs=%d", cost, reqs)
			}
			if energy < 1.999 || energy > 2.001 { // 0.28 / 0.14
				t.Errorf("aw3 energy=%v, want ~2 kWh", energy)
			}
		case "ragtag":
			if cost < 0.069 || cost > 0.071 {
				t.Errorf("ragtag cost=%v", cost)
			}
		}
	}
}

// TestUsageSeriesByGroup resolves keys to groups and buckets per group.
func TestUsageSeriesByGroup(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "aw3", Status: 200, CostUSD: 0.1})
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.2})
	_ = st.InsertActivity(store.Activity{TS: now, Served: "m", Key: "ragtag", Status: 200, CostUSD: 0.2})

	cfg := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10}, "batch": {Weight: 1},
		},
		Keys: map[string]string{"aw3": "interactive", "ragtag": "batch"},
	}
	h := &Handlers{Store: st, Cfg: cfg}
	out, err := h.UsageSeriesByGroup(context.Background(), &UsageSeriesInput{WindowHours: 2, BucketMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}

	byGroup := map[string][]GroupSeriesPoint{}
	for _, gs := range out.Body.Groups {
		byGroup[gs.Group] = gs.Points
		if len(gs.Points) != len(out.Body.Buckets) {
			t.Fatalf("%s points misaligned", gs.Group)
		}
	}
	sumReq := func(g string) int64 {
		var s int64
		for _, p := range byGroup[g] {
			s += p.Requests
		}
		return s
	}
	if sumReq("interactive") != 1 || sumReq("batch") != 2 {
		t.Errorf("group request totals: interactive=%d batch=%d, want 1/2", sumReq("interactive"), sumReq("batch"))
	}
	// batch busier → sorted first.
	if out.Body.Groups[0].Group != "batch" {
		t.Errorf("groups not busiest-first: %+v", out.Body.Groups)
	}
}

// TestLanes joins group policy with live admission load: an admitted request
// shows up under its group, and configured groups carry their weight/currency.
func TestLanes(t *testing.T) {
	cfg := &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10, Interruptible: false, ShareCurrency: "dwell"},
			"batch":       {Weight: 1, Interruptible: true},
		},
	}
	sc := sched.NewWithConfig(cfg)
	h := &Handlers{Cfg: cfg, Sched: sc}

	rel, _, err := sc.Admit(context.Background(), "m#0", "local", 2, "interactive", 10, false, config.Stage{Reject: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	out, err := h.Lanes(context.Background(), &LanesInput{})
	if err != nil {
		t.Fatal(err)
	}

	groups := map[string]GroupView{}
	for _, g := range out.Body.Groups {
		groups[g.Name] = g
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(groups), out.Body.Groups)
	}
	gi := groups["interactive"]
	if gi.Weight != 10 || gi.ShareCurrency != "dwell" || gi.Active != 1 || gi.Interruptible {
		t.Errorf("interactive = %+v", gi)
	}
	gb := groups["batch"]
	if gb.Weight != 1 || gb.ShareCurrency != "requests" || gb.Active != 0 || !gb.Interruptible {
		t.Errorf("batch = %+v", gb)
	}

	if len(out.Body.Backends) != 1 || out.Body.Backends[0].Backend != "m#0" || out.Body.Backends[0].Active != 1 {
		t.Errorf("backends = %+v", out.Body.Backends)
	}
}
