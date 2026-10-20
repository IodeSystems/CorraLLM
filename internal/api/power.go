package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/iodesystems/corrallm/internal/power"
	"github.com/iodesystems/corrallm/internal/store"
)

// PoolPower is one GPU's measured energy over the window.
type PoolPower struct {
	Server  string  `json:"server" doc:"Box the card is in."`
	Pool    string  `json:"pool" doc:"Device pool the card backs."`
	Kwh     float64 `json:"kwh" doc:"Measured energy of the card over the window, kWh."`
	BusyKwh float64 `json:"busyKwh" doc:"Of kwh, the part drawn while a request was being served on it — split across the models served, by time."`
	IdleKwh float64 `json:"idleKwh" doc:"Of kwh, the part drawn with nothing being served: the card's idle draw, charged to no model."`
	AvgW    float64 `json:"avgW" doc:"Mean draw over the measured part of the window, watts."`
}

// BoxPower is one box's measured wall draw over the window.
type BoxPower struct {
	Server  string  `json:"server" doc:"The box."`
	Kwh     float64 `json:"kwh" doc:"Measured wall energy over the window, kWh (the UPS, with only this box on it)."`
	GPUKwh  float64 `json:"gpuKwh" doc:"Of kwh, what its measured GPUs drew."`
	RestKwh float64 `json:"restKwh" doc:"kwh − gpuKwh: CPU, memory, fans, disks and power-supply loss. Charged to no model."`
	AvgW    float64 `json:"avgW" doc:"Mean wall draw over the measured part of the window, watts."`
}

// PowerView is measured energy for the window, from config power.
type PowerView struct {
	Source   string      `json:"source" doc:"The Prometheus server the watts were read from."`
	Measured bool        `json:"measured" doc:"True when these figures replaced the per-token estimate for the models on measured cards. False with error set when they could not."`
	Error    string      `json:"error" doc:"Why nothing was measured: Prometheus unreachable, too little data in the window, or an unbounded window. Empty when measured."`
	Coverage float64     `json:"coverage" doc:"Share of the window's sample steps that had data, 0..1, over every series read."`
	Pools    []PoolPower `json:"pools" doc:"Per GPU, measured energy split into busy and idle."`
	Boxes    []BoxPower  `json:"boxes" doc:"Per box, wall energy split into GPUs and the rest."`
	Kwh      float64     `json:"kwh" doc:"Total measured energy: each box's wall figure, or its GPUs' where it has no box query."`
	USD      float64     `json:"usd" doc:"kwh × costPerKwh."`
}

// minCoverage is the share of sample steps that must have data before the
// measurement replaces the estimate. Missing steps would read as 0 W, and a
// window past Prometheus' retention would quietly undercount.
const minCoverage = 0.9

// powerStep is the sample step for a window: at least 30 s, at most ~1,500
// steps, so a 30-day window is one query of 1,500 points rather than 86,400.
func powerStep(span time.Duration) time.Duration {
	step := max(30*time.Second, span/1500)
	return step.Round(time.Second)
}

// measurePower reads measured watts for the window and returns the view and,
// per served model on a measured card, its kWh. byModel is nil when nothing was
// measured; the caller then keeps the estimate.
//
// A card's energy in each step splits by what it was doing: the share of the
// step some request was being served goes to the models served in it, in
// proportion to each one's time; the rest is idle. Overlapping requests share
// one draw — the card does not draw twice.
func (h *Handlers) measurePower(ctx context.Context, w store.Window) (*PowerView, map[string]float64) {
	cfg := h.config()
	if cfg == nil || cfg.Power == nil {
		return nil, nil
	}
	view := &PowerView{Source: cfg.Power.Prometheus}
	now := time.Now().UnixMilli()
	from, to := w.FromMS, w.ToMS
	if to <= 0 || to > now {
		to = now
	}
	if from <= 0 || to <= from {
		view.Error = "the window has no start, so there is no span to read watts over"
		return view, nil
	}
	stepD := powerStep(time.Duration(to-from) * time.Millisecond)
	step := stepD.Milliseconds()
	n := int((to - from + step - 1) / step)
	at := func(ts int64) int {
		i := int((ts - from + step/2) / step)
		if i < 0 || i >= n {
			return -1
		}
		return i
	}
	kwh := func(watts float64) float64 { return watts * float64(step) / 3.6e9 }
	read := func(q string) ([]power.Series, error) {
		return power.QueryRange(ctx, cfg.Power.Prometheus, q,
			time.UnixMilli(from), time.UnixMilli(from+int64(n-1)*step), stepD)
	}

	spans, err := h.Store.ServiceSpans(w)
	if err != nil {
		view.Error = err.Error()
		return view, nil
	}
	byPool := spansByPool(cfg, spans)
	byModel := map[string]float64{}
	covered, expected := 0, 0

	names := make([]string, 0, len(cfg.Power.Servers))
	for name := range cfg.Power.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sp := cfg.Power.Servers[name]
		srv := cfg.Servers[name]
		gpuKwh := 0.0
		if sp.GPU != "" {
			series, err := read(sp.GPU)
			if err != nil {
				view.Error = fmt.Sprintf("gpu query for %s: %v", name, err)
				return view, nil
			}
			for _, s := range series {
				pool := ""
				for p, sel := range srv.Devices {
					if power.SameCard(sel, s.Labels["uuid"]) {
						pool = p
					}
				}
				if pool == "" {
					continue // a card no pool budgets
				}
				pp := PoolPower{Server: name, Pool: pool}
				mine := byPool[poolKey{name, pool}]
				union, _ := store.BusyFractions(mine, from, step, n, now)
				perModel := map[string][]float64{}
				models := map[string][]store.ServiceSpan{}
				for _, x := range mine {
					models[x.Served] = append(models[x.Served], x)
				}
				for m, xs := range models {
					perModel[m], _ = store.BusyFractions(xs, from, step, n, now)
				}
				have := 0
				for _, smp := range s.Samples {
					i := at(smp.TS)
					if i < 0 {
						continue
					}
					have++
					e := kwh(smp.V)
					pp.Kwh += e
					busy := e * union[i]
					sum := 0.0
					for _, f := range perModel {
						sum += f[i]
					}
					if sum <= 0 {
						busy = 0
					}
					pp.BusyKwh += busy
					pp.IdleKwh += e - busy
					for m, f := range perModel {
						if sum > 0 {
							byModel[m] += busy * f[i] / sum
						}
					}
				}
				for m := range models {
					byModel[m] += 0 // a model served here is measured even at 0 kWh
				}
				if have > 0 {
					pp.AvgW = pp.Kwh * 3.6e9 / float64(int64(have)*step)
				}
				covered += have
				expected += n
				gpuKwh += pp.Kwh
				view.Pools = append(view.Pools, pp)
			}
		}
		total := gpuKwh
		if sp.Box != "" {
			series, err := read(sp.Box)
			if err != nil {
				view.Error = fmt.Sprintf("box query for %s: %v", name, err)
				return view, nil
			}
			if len(series) != 1 {
				view.Error = fmt.Sprintf("box query for %s returned %d series, want 1", name, len(series))
				return view, nil
			}
			bp := BoxPower{Server: name, GPUKwh: gpuKwh}
			have := 0
			for _, smp := range series[0].Samples {
				if at(smp.TS) < 0 {
					continue
				}
				have++
				bp.Kwh += kwh(smp.V)
			}
			bp.RestKwh = max(0, bp.Kwh-gpuKwh)
			if have > 0 {
				bp.AvgW = bp.Kwh * 3.6e9 / float64(int64(have)*step)
			}
			covered += have
			expected += n
			total = bp.Kwh
			view.Boxes = append(view.Boxes, bp)
		}
		view.Kwh += total
	}
	if expected > 0 {
		view.Coverage = float64(covered) / float64(expected)
	}
	if view.Coverage < minCoverage {
		view.Error = fmt.Sprintf("Prometheus has data for %.0f%% of the window; below %.0f%% the estimate is kept", view.Coverage*100, minCoverage*100)
		return view, nil
	}
	view.Measured = true
	return view, byModel
}
