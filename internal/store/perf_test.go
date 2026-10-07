package store

import (
	"context"
	"testing"
)

// TestPerfStats covers per-model and total distributions, that unreported
// rates contribute nothing, and that a model which never reported a cache hit
// is left out of the cache distribution rather than read as 0%.
func TestPerfStats(t *testing.T) {
	st, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ins := func(ts int64, served string, status, prompt, cached int, tp, tg float64) {
		t.Helper()
		if err := st.InsertActivity(Activity{TS: ts, Served: served, Path: "/v1/chat/completions", Status: status,
			PromptTokens: prompt, CachedTokens: cached, PromptPerSec: tp, PredictedPerSec: tg}); err != nil {
			t.Fatal(err)
		}
	}
	// "m": 10 requests, tg 10..100, tp 100..1000, cache 0.1..1.0.
	for i := 1; i <= 10; i++ {
		ins(int64(i), "m", 200, 1000, 100*i, float64(100*i), float64(10*i))
	}
	ins(11, "m", 429, 0, 0, 0, 0)       // turned away: ignored
	ins(12, "m", 200, 500, 0, 0, 0)     // no timings reported: counts for cache only
	ins(13, "embed", 200, 50, 0, 0, 0)  // never reports a cache, no timings: absent
	ins(14, "n", 200, 1000, 0, 200, 50) // a second model, no cache hits ever

	rows, total, err := st.PerfStats(Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Served != "m" || rows[1].Served != "n" {
		t.Fatalf("rows = %+v", rows)
	}
	m := rows[0]
	if m.TG.N != 10 || m.TG.Avg != 55 || m.TG.P10 != 10 || m.TG.P90 != 90 {
		t.Errorf("m tg = %+v", m.TG)
	}
	if m.TP.N != 10 || m.TP.P10 != 100 || m.TP.P90 != 900 {
		t.Errorf("m tp = %+v", m.TP)
	}
	// 11 cache samples: 0.1..1.0 plus the 0 from the untimed request.
	if m.Cache.N != 11 || m.Cache.P90 != 0.9 {
		t.Errorf("m cache = %+v", m.Cache)
	}
	if n := rows[1]; n.Cache.N != 0 || n.TG.N != 1 {
		t.Errorf("n = %+v (a model with no cache hits must not read as 0%%)", n)
	}
	if total.TG.N != 11 || total.Cache.N != 11 {
		t.Errorf("total = %+v", total)
	}
}

// P99 is the nearest rank: over 1..200 it is the 198th value, not the max.
func TestDistP99IsNearestRank(t *testing.T) {
	v := make([]float64, 200)
	for i := range v {
		v[len(v)-1-i] = float64(i + 1) // unsorted on purpose
	}
	d := dist(v)
	if d.P90 != 180 || d.P99 != 198 {
		t.Errorf("P90 %v P99 %v, want 180 and 198", d.P90, d.P99)
	}
	if one := dist([]float64{7}); one.P99 != 7 {
		t.Errorf("single value P99 = %v", one.P99)
	}
}
