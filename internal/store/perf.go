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
	Group
	// Decode (generation) speed, tokens/sec.
	TG Dist
	// Prefill (prompt processing) speed, tokens/sec.
	TP Dist
	// Cache: cached_tokens / prompt_tokens per request, 0..1. Only over models
	// that reported ANY cache hit in the window — a backend with no prompt cache
	// (or that does not report one) would otherwise read as a measured 0%.
	Cache Dist
	// Prefill as THROUGHPUT, token-weighted. The per-request mean (TP) is
	// dominated by cache hits that read a few hundred new tokens behind a fixed
	// per-request overhead; with 95% of prompts cached it reads ~780 tok/s while
	// the card does ~2,000 on any real chunk (box1, 2026-10-09).
	Prefill PrefillStat
}

// PrefillBucketBounds are the upper bounds (exclusive) of the new-token
// buckets; the last bucket is open-ended.
var PrefillBucketBounds = []int64{500, 2000, 8000}

// PrefillStat sums prompt processing over requests that reported a prefill
// rate. Seconds is each request's new tokens / its reported rate.
type PrefillStat struct {
	N            int64
	PromptTokens int64 // all prompt tokens, cached included
	NewTokens    int64 // tokens actually processed
	Seconds      float64
	// Buckets by new tokens per request: <500, 500–2k, 2k–8k, 8k+.
	Buckets [4]PrefillBucket
}

// PrefillBucket is the same sums over the requests in one new-token range.
type PrefillBucket struct {
	N         int64
	NewTokens int64
	Seconds   float64
}

// Fused is prompt tokens, cached included, per second of prefill: the rate a
// caller's prompt is effectively read at.
func (p PrefillStat) Fused() float64 { return ratio(float64(p.PromptTokens), p.Seconds) }

// Raw is new tokens per second of prefill: what the GPU actually processed.
func (p PrefillStat) Raw() float64 { return ratio(float64(p.NewTokens), p.Seconds) }

// Raw is the bucket's new tokens per second.
func (b PrefillBucket) Raw() float64 { return ratio(float64(b.NewTokens), b.Seconds) }

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}

func (p *PrefillStat) add(prompt, cached int64, tp float64) {
	newTok := prompt - cached
	if tp <= 0 || newTok <= 0 {
		return
	}
	sec := float64(newTok) / tp
	p.N++
	p.PromptTokens += prompt
	p.NewTokens += newTok
	p.Seconds += sec
	i := len(PrefillBucketBounds)
	for j, hi := range PrefillBucketBounds {
		if newTok < hi {
			i = j
			break
		}
	}
	b := &p.Buckets[i]
	b.N++
	b.NewTokens += newTok
	b.Seconds += sec
}

func (p *PrefillStat) merge(o PrefillStat) {
	p.N += o.N
	p.PromptTokens += o.PromptTokens
	p.NewTokens += o.NewTokens
	p.Seconds += o.Seconds
	for i := range p.Buckets {
		p.Buckets[i].N += o.Buckets[i].N
		p.Buckets[i].NewTokens += o.Buckets[i].NewTokens
		p.Buckets[i].Seconds += o.Buckets[i].Seconds
	}
}

// PerfStats reads per-request speed and cache-hit distributions over a window,
// narrowed and split by sl, plus a total across the rows (Group zero).
//
// These columns sit after the captured payload on a migrated database, so the
// read walks each row's overflow pages while the payload is still kept (7
// days). Measured on box1 2026-10-02, warm: 1 day 0.24 s, 7 days 1.35 s, 30
// days 1.31 s — the same cost RollupByModel already pays for its averages.
func (s *Store) PerfStats(w Window, sl Slice) (rows []PerfStat, total PerfStat, err error) {
	wWhere, wArgs := w.ts()
	sWhere, sArgs := sl.where("key", "served")
	q, err := s.db.Query(
		`SELECT key, served, prompt_tokens, cached_tokens, prompt_per_sec, predicted_per_sec
		 FROM activity WHERE `+wWhere+sWhere+` AND status = 200`, append(wArgs, sArgs...)...)
	if err != nil {
		return nil, PerfStat{}, err
	}
	defer func() { _ = q.Close() }()

	type acc struct {
		tg, tp, cache []float64
		prefill       PrefillStat
	}
	by := map[Group]*acc{}
	// Whether a MODEL reports a cache is a property of its backend, whatever
	// the split, so this stays keyed by served name.
	reportsCache := map[string]bool{}
	type hit struct {
		group  Group
		served string
		rate   float64
	}
	var hits []hit
	for q.Next() {
		var key, served string
		var prompt, cached int64
		var tp, tg float64
		if err := q.Scan(&key, &served, &prompt, &cached, &tp, &tg); err != nil {
			return nil, PerfStat{}, err
		}
		g := sl.group(key, served)
		a := by[g]
		if a == nil {
			a = &acc{}
			by[g] = a
		}
		if tg > 0 {
			a.tg = append(a.tg, tg)
		}
		if tp > 0 {
			a.tp = append(a.tp, tp)
		}
		a.prefill.add(prompt, cached, tp)
		if prompt > 0 {
			hits = append(hits, hit{g, served, float64(cached) / float64(prompt)})
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
			by[h.group].cache = append(by[h.group].cache, h.rate)
		}
	}

	var all acc
	for g, a := range by {
		if len(a.tg)+len(a.tp)+len(a.cache) == 0 {
			continue // no request here reported anything this reads
		}
		all.tg = append(all.tg, a.tg...)
		all.tp = append(all.tp, a.tp...)
		all.cache = append(all.cache, a.cache...)
		all.prefill.merge(a.prefill)
		rows = append(rows, PerfStat{Group: g, TG: dist(a.tg), TP: dist(a.tp), Cache: dist(a.cache), Prefill: a.prefill})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group.less(rows[j].Group) })
	return rows, PerfStat{TG: dist(all.tg), TP: dist(all.tp), Cache: dist(all.cache), Prefill: all.prefill}, nil
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
