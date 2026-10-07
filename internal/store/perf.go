package store

import (
	"math"
	"sort"
)

// Dist is a distribution summary over requests: how many, the mean, and both
// tails. Both tails because which one matters depends on the reading: for a
// speed the slow tail (P10) is where a slowdown shows, for a cache hit rate the
// high tail (P90) is how good it gets.
type Dist struct {
	N   int64
	Avg float64
	P10 float64
	P90 float64
	P99 float64
}

// PerfStat is per-request speed and cache behaviour for one served model (or,
// with Served empty, all of them together).
//
// PER REQUEST, not per token: the mean decode speed here is the speed a caller
// typically saw, which is the question the dashboard asks. Rates are the
// backend's own report (llama.cpp timings); a request that reported none
// contributes nothing rather than a zero.
type PerfStat struct {
	Served string
	// Decode (generation) speed, tokens/sec.
	TG Dist
	// Prefill (prompt processing) speed, tokens/sec.
	TP Dist
	// Cache: cached_tokens / prompt_tokens per request, 0..1. Only over models
	// that reported ANY cache hit in the window — a backend with no prompt cache
	// (or that does not report one) would otherwise read as a measured 0%.
	Cache Dist
}

// PerfStats reads per-request speed and cache-hit distributions over a window,
// per served model plus a total across them (Served "").
//
// These columns sit after the captured payload on a migrated database, so the
// read walks each row's overflow pages while the payload is still kept (7
// days). Measured on box1 2026-10-02, warm: 1 day 0.24 s, 7 days 1.35 s, 30
// days 1.31 s — the same cost RollupByModel already pays for its averages.
func (s *Store) PerfStats(w Window) (rows []PerfStat, total PerfStat, err error) {
	wWhere, wArgs := w.ts()
	q, err := s.db.Query(
		`SELECT served, prompt_tokens, cached_tokens, prompt_per_sec, predicted_per_sec
		 FROM activity WHERE `+wWhere+` AND status = 200`, wArgs...)
	if err != nil {
		return nil, PerfStat{}, err
	}
	defer func() { _ = q.Close() }()

	type acc struct{ tg, tp, cache []float64 }
	by := map[string]*acc{}
	reportsCache := map[string]bool{}
	type hit struct {
		served string
		rate   float64
	}
	var hits []hit
	for q.Next() {
		var served string
		var prompt, cached int64
		var tp, tg float64
		if err := q.Scan(&served, &prompt, &cached, &tp, &tg); err != nil {
			return nil, PerfStat{}, err
		}
		a := by[served]
		if a == nil {
			a = &acc{}
			by[served] = a
		}
		if tg > 0 {
			a.tg = append(a.tg, tg)
		}
		if tp > 0 {
			a.tp = append(a.tp, tp)
		}
		if prompt > 0 {
			hits = append(hits, hit{served, float64(cached) / float64(prompt)})
		}
		if cached > 0 {
			reportsCache[served] = true
		}
	}
	if err := q.Err(); err != nil {
		return nil, PerfStat{}, err
	}
	for _, h := range hits {
		if reportsCache[h.served] {
			by[h.served].cache = append(by[h.served].cache, h.rate)
		}
	}

	var all acc
	for served, a := range by {
		if len(a.tg)+len(a.tp)+len(a.cache) == 0 {
			continue // no request here reported anything this reads
		}
		all.tg = append(all.tg, a.tg...)
		all.tp = append(all.tp, a.tp...)
		all.cache = append(all.cache, a.cache...)
		rows = append(rows, PerfStat{Served: served, TG: dist(a.tg), TP: dist(a.tp), Cache: dist(a.cache)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Served < rows[j].Served })
	return rows, PerfStat{TG: dist(all.tg), TP: dist(all.tp), Cache: dist(all.cache)}, nil
}

// dist summarises v (sorting it in place): mean and nearest-rank P10/P90.
func dist(v []float64) Dist {
	if len(v) == 0 {
		return Dist{}
	}
	sort.Float64s(v)
	var sum float64
	for _, x := range v {
		sum += x
	}
	rank := func(p float64) float64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
	return Dist{N: int64(len(v)), Avg: sum / float64(len(v)), P10: rank(0.1), P90: rank(0.9), P99: rank(0.99)}
}
