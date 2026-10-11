package api

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// GPUUtilizationInput sets the window and granularity of the GPU utilization series.
type GPUUtilizationInput struct {
	WindowHours   int   `query:"windowHours" default:"24" minimum:"1" maximum:"8760" doc:"Trailing window in hours."`
	BucketMinutes int   `query:"bucketMinutes" default:"15" minimum:"1" maximum:"1440" doc:"Bucket width in minutes."`
	FromMS        int64 `query:"from" doc:"Absolute window start, epoch ms (inclusive). Overrides windowHours."`
	ToMS          int64 `query:"to" doc:"Absolute window end, epoch ms (exclusive). Overrides windowHours."`
}

// PoolUtilization is one GPU pool's busy time and time to first token, per bucket.
type PoolUtilization struct {
	Server  string   `json:"server" doc:"Box the pool is on."`
	Pool    string   `json:"pool" doc:"Device pool (the GPU) the models' weights are placed on."`
	Models  []string `json:"models" doc:"Served models that ran on this pool in the window."`
	BusyPct float64  `json:"busyPct" doc:"Share of the window (elapsed part) at least one request was being served on this pool, 0..1."`
	// Per bucket, aligned to GPUUtilizationOutput.Buckets.
	Busy     []float64 `json:"busy" doc:"Per bucket: share of the bucket at least one request was being served, 0..1."`
	Requests []int64   `json:"requests" doc:"Per bucket: served requests that ENDED in the bucket."`
	TTFTN    []int64   `json:"ttftN" doc:"Per bucket: streamed requests whose first byte fell in the bucket — what the ttft* means are over."`
	QueueMS  []float64 `json:"queueMs" doc:"Per bucket: mean time in corrallm's queue before admission, ms."`
	LoadMS   []float64 `json:"loadMs" doc:"Per bucket: mean time waiting for a cold model load, ms."`
	ActiveMS []float64 `json:"activeMs" doc:"Per bucket: mean active time to first token (prompt processing, after queue and load), ms."`
	// Window totals over the same streamed requests.
	TTFTCount   int64   `json:"ttftCount" doc:"Streamed requests with a first-token figure in the window."`
	QueueMSAvg  float64 `json:"queueMsAvg" doc:"Window mean queue time, ms."`
	ActiveMSAvg float64 `json:"activeMsAvg" doc:"Window mean active time to first token, ms."`
	TTFTMSP90   int64   `json:"ttftMsP90" doc:"Window 90th percentile of total time to first token (queue + load + active), ms."`
	ActiveMSP90 int64   `json:"activeMsP90" doc:"Window 90th percentile of active time to first token, ms."`
	TTFTMSP99   int64   `json:"ttftMsP99" doc:"Window 99th percentile of total time to first token, ms."`
	ActiveMSP99 int64   `json:"activeMsP99" doc:"Window 99th percentile of active time to first token, ms."`
}

// GPUUtilizationOutput is a shared time axis plus one series per GPU pool.
type GPUUtilizationOutput struct {
	Body struct {
		BucketMinutes int               `json:"bucketMinutes" doc:"Effective bucket width (may be coarsened)."`
		Buckets       []int64           `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Pools         []PoolUtilization `json:"pools" doc:"One entry per GPU pool that served anything in the window, busiest first."`
	}
}

// GPUUtilization reports, per GPU pool, how much of the time something was
// being served (the union of request service spans, so two models on one card
// at once count once) and how time to first token splits into corrallm's
// queue, cold load, and active prompt processing.
//
// A request is attributed to the pool of the placement that served it (or the
// model's only placement for rows older than the placement column). Models
// corrallm does not run (pure proxies) have no pool and are left out.
func (h *Handlers) GPUUtilization(_ context.Context, in *GPUUtilizationInput) (*GPUUtilizationOutput, error) {
	now := time.Now().UnixMilli()
	w := windowOf(in.FromMS, in.ToMS, seriesMinutes(in.WindowHours))
	buckets, _, bucketMS := seriesAxis(w, in.BucketMinutes, now)
	spans, err := h.Store.ServiceSpans(w)
	if err != nil {
		return nil, err
	}
	cfg := h.config()
	type poolKey struct{ server, pool string }
	byPool := map[poolKey][]store.ServiceSpan{}
	for _, sp := range spans {
		m, ok := cfg.Models[sp.Served]
		if !ok {
			continue
		}
		pl, ok := m.PlacementNamed(sp.Placement)
		if !ok {
			list := m.PlacementList()
			if len(list) == 0 {
				continue
			}
			pl = list[0]
		}
		if pl.Pool == "" {
			continue
		}
		k := poolKey{pl.Server, pl.Pool}
		byPool[k] = append(byPool[k], sp)
	}

	out := &GPUUtilizationOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	if len(buckets) == 0 {
		return out, nil
	}
	start, n := buckets[0], len(buckets)
	from := w.FromMS
	if from <= 0 || from < start {
		from = start
	}
	to := w.ToMS
	if to <= 0 || to > now {
		to = now
	}
	pos := func(t int64) int {
		if t < start {
			return -1
		}
		i := int((t - start) / bucketMS)
		if i >= n {
			return -1
		}
		return i
	}
	for k, sps := range byPool {
		frac, busyMS := store.BusyFractions(sps, start, bucketMS, n, now)
		p := PoolUtilization{
			Server: k.server, Pool: k.pool, Busy: frac,
			Requests: make([]int64, n), TTFTN: make([]int64, n),
			QueueMS: make([]float64, n), LoadMS: make([]float64, n), ActiveMS: make([]float64, n),
		}
		if to > from {
			p.BusyPct = float64(busyMS) / float64(to-from)
		}
		models := map[string]bool{}
		var totals, actives []int64
		var qSum, aSum int64
		for _, sp := range sps {
			models[sp.Served] = true
			if i := pos(sp.EndMS); i >= 0 {
				p.Requests[i]++
			}
			if sp.ActiveTTFTMS < 0 {
				continue
			}
			i := pos(sp.FirstByteMS)
			if i < 0 {
				continue
			}
			p.TTFTN[i]++
			p.QueueMS[i] += float64(sp.QueuedMS)
			p.LoadMS[i] += float64(sp.LoadMS)
			p.ActiveMS[i] += float64(sp.ActiveTTFTMS)
			totals = append(totals, sp.QueuedMS+sp.LoadMS+sp.ActiveTTFTMS)
			actives = append(actives, sp.ActiveTTFTMS)
			qSum += sp.QueuedMS
			aSum += sp.ActiveTTFTMS
		}
		for i := range p.TTFTN {
			if c := float64(p.TTFTN[i]); c > 0 {
				p.QueueMS[i] /= c
				p.LoadMS[i] /= c
				p.ActiveMS[i] /= c
			}
		}
		if c := int64(len(totals)); c > 0 {
			p.TTFTCount = c
			p.QueueMSAvg = float64(qSum) / float64(c)
			p.ActiveMSAvg = float64(aSum) / float64(c)
			p.TTFTMSP90, p.TTFTMSP99 = rankP90P99(totals)
			p.ActiveMSP90, p.ActiveMSP99 = rankP90P99(actives)
		}
		for m := range models {
			p.Models = append(p.Models, m)
		}
		sort.Strings(p.Models)
		out.Body.Pools = append(out.Body.Pools, p)
	}
	sort.Slice(out.Body.Pools, func(i, j int) bool { return out.Body.Pools[i].BusyPct > out.Body.Pools[j].BusyPct })
	return out, nil
}

// rankP90P99 is the nearest-rank 90th and 99th percentiles. Sorts v.
func rankP90P99(v []int64) (p90, p99 int64) {
	slices.Sort(v)
	at := func(q float64) int64 { return v[min(len(v)-1, int(q*float64(len(v))))] }
	return at(0.9), at(0.99)
}
