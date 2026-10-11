package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Two models on one pool serving at once count once toward busy; a model with
// no pool (a pure proxy) is left out; TTFT splits into queue and active.
func TestGPUUtilization(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UnixMilli()
	for _, a := range []store.Activity{
		// big: served now-60s..now-30s (30 s), ttfb 12 s of which 10 s queued.
		{TS: now - 30_000, Served: "big", Path: "/v1/chat/completions", Status: 200, DwellMS: 40_000, QueuedMS: 10_000, TTFBMs: 12_000},
		// small, same pool, overlapping now-50s..now-40s: adds nothing to busy.
		{TS: now - 40_000, Served: "small", Path: "/v1/chat/completions", Status: 200, DwellMS: 10_000, TTFBMs: 1_000},
		{TS: now - 20_000, Served: "remote", Path: "/v1/chat/completions", Status: 200, DwellMS: 5_000, TTFBMs: 500},
	} {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Models: map[string]config.Model{
		"big":    {Server: "box1", Pool: "gpu0", Cmd: "x"},
		"small":  {Server: "box1", Pool: "gpu0", Cmd: "x"},
		"remote": {},
	}}
	h := &Handlers{Store: st, Cfg: cfg}
	out, err := h.GPUUtilization(context.Background(), &GPUUtilizationInput{WindowHours: 1, BucketMinutes: 60})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Pools) != 1 {
		t.Fatalf("pools %+v, want only box1/gpu0", out.Body.Pools)
	}
	p := out.Body.Pools[0]
	if p.Server != "box1" || p.Pool != "gpu0" || len(p.Models) != 2 {
		t.Errorf("pool %+v", p)
	}
	var busy float64
	for _, b := range p.Busy {
		busy += b
	}
	if busy <= 0 {
		t.Errorf("no busy share: %v", p.Busy)
	}
	if p.TTFTCount != 2 || p.QueueMSAvg != 5_000 || p.ActiveMSAvg != 1_500 {
		t.Errorf("ttft count %d queue %v active %v, want 2 / 5000 / 1500", p.TTFTCount, p.QueueMSAvg, p.ActiveMSAvg)
	}
}
