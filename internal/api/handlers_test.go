package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
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
