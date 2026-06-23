// Package api holds corrallm's typed handlers and the gat gateway wiring. Each
// operation is registered once with gat.Register and is thereby reachable over
// REST (huma), GraphQL, and gRPC — the "register once → typed everywhere" loop.
// P0 ships the meta operations (health, version) that exercise the whole
// codegen pipeline; the inference proxy + scheduler operations land in P1+.
package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
)

// Handlers carries the dependencies every operation needs. It grows as phases
// add the scheduler, residency, and cost subsystems.
type Handlers struct {
	Version string
	Cfg     *config.Config
	Store   *store.Store
	Mgr     *proc.Manager    // residency introspection (P8)
	Sched   *sched.Scheduler // live admission load (P8-beyond)
}

// --- health ---

// HealthInput has no parameters.
type HealthInput struct{}

// HealthOutput reports liveness and the build version.
type HealthOutput struct {
	Body struct {
		Status  string `json:"status" doc:"Always \"ok\" when the process is serving."`
		Version string `json:"version" doc:"Build version stamp."`
	}
}

// Health is an unauthenticated liveness probe.
func (h *Handlers) Health(_ context.Context, _ *HealthInput) (*HealthOutput, error) {
	out := &HealthOutput{}
	out.Body.Status = "ok"
	out.Body.Version = h.Version
	return out, nil
}

// --- config summary ---

// ConfigSummaryInput has no parameters.
type ConfigSummaryInput struct{}

// ConfigSummaryOutput reports the loaded config's shape — enough for the P0 UI
// to prove the gat→GraphQL→typed-client loop end to end without leaking secrets.
type ConfigSummaryOutput struct {
	Body struct {
		Servers        []string `json:"servers" doc:"Declared server names."`
		Models         []string `json:"models" doc:"Served model names."`
		PriorityGroups []string `json:"priorityGroups" doc:"Configured priority group names."`
	}
}

// ConfigSummary returns the names declared in the loaded config.
func (h *Handlers) ConfigSummary(_ context.Context, _ *ConfigSummaryInput) (*ConfigSummaryOutput, error) {
	out := &ConfigSummaryOutput{}
	out.Body.Servers = keys(h.Cfg.Servers)
	out.Body.Models = keys(h.Cfg.Models)
	out.Body.PriorityGroups = keys(h.Cfg.PriorityGroups)
	return out, nil
}

// --- recent activity (P8) ---

// RecentActivityInput bounds how many records to return.
type RecentActivityInput struct {
	Limit int `query:"limit" default:"50" minimum:"1" maximum:"500" doc:"Max records, newest first."`
}

// ActivityRecord is one proxied-request row surfaced to the UI. Mirrors
// store.Activity with the P6 metering fields (dwell/tokens/$) exposed.
type ActivityRecord struct {
	TS               int64   `json:"ts" doc:"Unix millis when the request was logged."`
	Served           string  `json:"served" doc:"Served model name."`
	Backend          string  `json:"backend" doc:"Backend that handled it."`
	Key              string  `json:"key" doc:"Caller identity."`
	Path             string  `json:"path" doc:"Request path."`
	Status           int     `json:"status" doc:"HTTP status."`
	DwellMS          int64   `json:"dwellMs" doc:"Time in request, milliseconds."`
	PromptTokens     int     `json:"promptTokens" doc:"Metered prompt tokens."`
	CompletionTokens int     `json:"completionTokens" doc:"Metered completion tokens."`
	CostUSD          float64 `json:"costUsd" doc:"Resolved request cost in USD."`
}

// RecentActivityOutput is the newest-first activity list.
type RecentActivityOutput struct {
	Body struct {
		Records []ActivityRecord `json:"records" doc:"Activity rows, newest first."`
	}
}

// RecentActivity returns the most recent proxied-request records.
func (h *Handlers) RecentActivity(_ context.Context, in *RecentActivityInput) (*RecentActivityOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := h.Store.RecentActivity(limit)
	if err != nil {
		return nil, err
	}
	out := &RecentActivityOutput{}
	out.Body.Records = make([]ActivityRecord, 0, len(rows))
	for _, a := range rows {
		out.Body.Records = append(out.Body.Records, ActivityRecord{
			TS:               a.TS,
			Served:           a.Served,
			Backend:          a.Backend,
			Key:              a.Key,
			Path:             a.Path,
			Status:           a.Status,
			DwellMS:          a.DwellMS,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			CostUSD:          a.CostUSD,
		})
	}
	return out, nil
}

// --- residency (P8) ---

// ResidencyInput has no parameters.
type ResidencyInput struct{}

// PoolView is one memory pool's budget/usage.
type PoolView struct {
	Pool   string `json:"pool" doc:"Pool name (gpu0, system, …)."`
	Budget int64  `json:"budget" doc:"Bytes available to spawned backends (total − reserve)."`
	Used   int64  `json:"used" doc:"Bytes currently reserved by resident backends."`
}

// ServerView is a server's per-pool residency.
type ServerView struct {
	Server string     `json:"server" doc:"Server name."`
	Pools  []PoolView `json:"pools" doc:"Per-pool budget/usage."`
}

// PoolUsageView is a resident backend's reservation against one pool.
type PoolUsageView struct {
	Pool  string `json:"pool" doc:"Pool name."`
	Bytes int64  `json:"bytes" doc:"Reserved bytes."`
}

// ResidentModelView is one loaded/loading backend.
type ResidentModelView struct {
	Name       string          `json:"name" doc:"Backend id (<servedModel>#<index>)."`
	ModelName  string          `json:"modelName" doc:"Served model name."`
	Server     string          `json:"server" doc:"Server, empty for pure-proxy."`
	State      string          `json:"state" doc:"absent|loading|ready|failed|evicting."`
	Refs       int             `json:"refs" doc:"In-flight requests holding it."`
	Persistent bool            `json:"persistent" doc:"Pinned: exempt from eviction."`
	LastUsedMS int64           `json:"lastUsedMs" doc:"Unix millis of last use, 0 if never."`
	NCtx       int             `json:"nCtx" doc:"Context length parsed from the backend (0 if unknown)."`
	NSlots     int             `json:"nSlots" doc:"Slot count parsed from the backend (0 if unknown)."`
	Usage      []PoolUsageView `json:"usage" doc:"Per-pool reservation."`
}

// ResidencyOutput reports server pool budgets and resident backends.
type ResidencyOutput struct {
	Body struct {
		Servers []ServerView        `json:"servers" doc:"Per-server pool budget/usage."`
		Models  []ResidentModelView `json:"models" doc:"Currently resident backends."`
	}
}

// Residency returns the live residency snapshot (pool budgets + what's warm).
func (h *Handlers) Residency(_ context.Context, _ *ResidencyInput) (*ResidencyOutput, error) {
	snap := h.Mgr.Snapshot()
	out := &ResidencyOutput{}
	out.Body.Servers = make([]ServerView, 0, len(snap.Servers))
	for _, s := range snap.Servers {
		sv := ServerView{Server: s.Server, Pools: make([]PoolView, 0, len(s.Pools))}
		for _, p := range s.Pools {
			sv.Pools = append(sv.Pools, PoolView{Pool: p.Pool, Budget: p.Budget, Used: p.Used})
		}
		out.Body.Servers = append(out.Body.Servers, sv)
	}
	out.Body.Models = make([]ResidentModelView, 0, len(snap.Models))
	for _, m := range snap.Models {
		mv := ResidentModelView{
			Name: m.Name, ModelName: m.ModelName, Server: m.Server, State: m.State,
			Refs: m.Refs, Persistent: m.Persistent, LastUsedMS: m.LastUsedMS,
			NCtx: m.NCtx, NSlots: m.NSlots,
			Usage: make([]PoolUsageView, 0, len(m.Usage)),
		}
		for _, u := range m.Usage {
			mv.Usage = append(mv.Usage, PoolUsageView{Pool: u.Pool, Bytes: u.Bytes})
		}
		out.Body.Models = append(out.Body.Models, mv)
	}
	return out, nil
}

// --- usage rollup (P8) ---

// UsageRollupInput bounds the rollup window.
type UsageRollupInput struct {
	WindowHours int `query:"windowHours" default:"24" minimum:"0" maximum:"8760" doc:"Trailing window in hours; 0 = all time."`
}

// RollupRow is aggregated usage for one served model.
type RollupRow struct {
	Served           string  `json:"served" doc:"Served model name."`
	Requests         int64   `json:"requests" doc:"Request count."`
	PromptTokens     int64   `json:"promptTokens" doc:"Total prompt tokens."`
	CompletionTokens int64   `json:"completionTokens" doc:"Total completion tokens."`
	DwellMS          int64   `json:"dwellMs" doc:"Total dwell, milliseconds."`
	CostUSD          float64 `json:"costUsd" doc:"Total cost, USD."`
}

// UsageRollupOutput is per-model usage plus a grand total over the window.
type UsageRollupOutput struct {
	Body struct {
		WindowHours int         `json:"windowHours" doc:"Window applied (0 = all time)."`
		Rows        []RollupRow `json:"rows" doc:"Per-model usage, costliest first."`
		Total       RollupRow   `json:"total" doc:"Grand total across all models (served=\"\")."`
	}
}

// UsageRollup aggregates metered activity by served model over a window.
func (h *Handlers) UsageRollup(_ context.Context, in *UsageRollupInput) (*UsageRollupOutput, error) {
	var sinceMS int64
	if in.WindowHours > 0 {
		sinceMS = time.Now().Add(-time.Duration(in.WindowHours) * time.Hour).UnixMilli()
	}
	rows, err := h.Store.RollupByModel(sinceMS)
	if err != nil {
		return nil, err
	}
	out := &UsageRollupOutput{}
	out.Body.WindowHours = in.WindowHours
	out.Body.Rows = make([]RollupRow, 0, len(rows))
	for _, r := range rows {
		row := RollupRow{
			Served: r.Served, Requests: r.Requests,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			DwellMS: r.DwellMS, CostUSD: r.CostUSD,
		}
		out.Body.Rows = append(out.Body.Rows, row)
		out.Body.Total.Requests += row.Requests
		out.Body.Total.PromptTokens += row.PromptTokens
		out.Body.Total.CompletionTokens += row.CompletionTokens
		out.Body.Total.DwellMS += row.DwellMS
		out.Body.Total.CostUSD += row.CostUSD
	}
	return out, nil
}

// --- per-key usage rollup (P8-beyond) ---

// UsageByKeyInput bounds the rollup window.
type UsageByKeyInput struct {
	WindowHours int `query:"windowHours" default:"24" minimum:"0" maximum:"8760" doc:"Trailing window in hours; 0 = all time."`
}

// KeyUsageRow is aggregated usage for one caller key, including energy derived
// from cost (energy = cost / costPerKwh; meaningful for energy-priced local
// types — exact for an all-local deployment).
type KeyUsageRow struct {
	Key              string  `json:"key" doc:"Caller key (empty = unkeyed)."`
	Requests         int64   `json:"requests" doc:"Request count."`
	PromptTokens     int64   `json:"promptTokens" doc:"Total prompt tokens."`
	CompletionTokens int64   `json:"completionTokens" doc:"Total completion tokens."`
	DwellMS          int64   `json:"dwellMs" doc:"Total time in request, milliseconds."`
	CostUSD          float64 `json:"costUsd" doc:"Total cost, USD."`
	EnergyKwh        float64 `json:"energyKwh" doc:"Energy in kWh (cost / costPerKwh; 0 if rate unset)."`
}

// UsageByKeyOutput is per-key usage over the window, costliest first.
type UsageByKeyOutput struct {
	Body struct {
		WindowHours int           `json:"windowHours" doc:"Window applied (0 = all time)."`
		Rows        []KeyUsageRow `json:"rows" doc:"Per-key usage, costliest first."`
	}
}

// UsageByKey aggregates metered activity by caller key over a window — the data
// behind the per-key cost/requests/energy/time view.
func (h *Handlers) UsageByKey(_ context.Context, in *UsageByKeyInput) (*UsageByKeyOutput, error) {
	var sinceMS int64
	if in.WindowHours > 0 {
		sinceMS = time.Now().Add(-time.Duration(in.WindowHours) * time.Hour).UnixMilli()
	}
	rows, err := h.Store.RollupByKey(sinceMS)
	if err != nil {
		return nil, err
	}
	rate := h.Cfg.CostPerKwh
	out := &UsageByKeyOutput{}
	out.Body.WindowHours = in.WindowHours
	out.Body.Rows = make([]KeyUsageRow, 0, len(rows))
	for _, r := range rows {
		row := KeyUsageRow{
			Key: r.Key, Requests: r.Requests,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			DwellMS: r.DwellMS, CostUSD: r.CostUSD,
		}
		if rate > 0 {
			row.EnergyKwh = r.CostUSD / rate
		}
		out.Body.Rows = append(out.Body.Rows, row)
	}
	return out, nil
}

// --- per-key usage time-series (P8-beyond) ---

// UsageSeriesInput sets the time window and bucket granularity.
type UsageSeriesInput struct {
	WindowHours   int `query:"windowHours" default:"24" minimum:"1" maximum:"8760" doc:"Trailing window in hours."`
	BucketMinutes int `query:"bucketMinutes" default:"60" minimum:"1" maximum:"1440" doc:"Bucket width in minutes."`
}

// SeriesPoint is one bucket's metrics for a key (aligned to UsageSeriesOutput.Buckets).
type SeriesPoint struct {
	Requests  int64   `json:"requests" doc:"Requests in this bucket."`
	CostUSD   float64 `json:"costUsd" doc:"Cost in this bucket, USD."`
	EnergyKwh float64 `json:"energyKwh" doc:"Energy in this bucket, kWh (cost/costPerKwh)."`
	DwellMS   int64   `json:"dwellMs" doc:"Total dwell in this bucket, ms."`
}

// KeySeries is one caller key's dense time series.
type KeySeries struct {
	Key    string        `json:"key" doc:"Caller key (empty = unkeyed)."`
	Points []SeriesPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// UsageSeriesOutput is a shared time axis plus one dense series per key.
type UsageSeriesOutput struct {
	Body struct {
		BucketMinutes int         `json:"bucketMinutes" doc:"Effective bucket width (may be coarsened)."`
		Buckets       []int64     `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Keys          []KeySeries `json:"keys" doc:"Per-key dense series, costliest first."`
	}
}

const maxSeriesBuckets = 600

// UsageSeries returns per-key time series (requests/cost/energy/dwell) over a
// window, bucketed for charting. Buckets are dense (0-filled) so every key's
// Points align to the shared Buckets axis.
func (h *Handlers) UsageSeries(_ context.Context, in *UsageSeriesInput) (*UsageSeriesOutput, error) {
	windowHours := in.WindowHours
	if windowHours <= 0 {
		windowHours = 24
	}
	bucketMin := in.BucketMinutes
	if bucketMin <= 0 {
		bucketMin = 60
	}
	windowMS := int64(windowHours) * 3600_000
	bucketMS := int64(bucketMin) * 60_000
	// Coarsen so the axis never exceeds maxSeriesBuckets points.
	for windowMS/bucketMS > maxSeriesBuckets {
		bucketMS *= 2
	}

	now := time.Now().UnixMilli()
	end := (now / bucketMS) * bucketMS
	start := ((now - windowMS) / bucketMS) * bucketMS

	var buckets []int64
	index := map[int64]int{} // bucket start → position in buckets
	for b := start; b <= end; b += bucketMS {
		index[b] = len(buckets)
		buckets = append(buckets, b)
	}

	rows, err := h.Store.RollupSeries(now-windowMS, bucketMS)
	if err != nil {
		return nil, err
	}

	rate := h.Cfg.CostPerKwh
	// Per key: a dense slice of points + a running total cost for ordering.
	type acc struct {
		points    []SeriesPoint
		totalCost float64
	}
	byKey := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue // outside the dense axis (clock skew); skip
		}
		a := byKey[r.Key]
		if a == nil {
			a = &acc{points: make([]SeriesPoint, len(buckets))}
			byKey[r.Key] = a
		}
		energy := 0.0
		if rate > 0 {
			energy = r.CostUSD / rate
		}
		a.points[pos] = SeriesPoint{
			Requests: r.Requests, CostUSD: r.CostUSD, EnergyKwh: energy, DwellMS: r.DwellMS,
		}
		a.totalCost += r.CostUSD
	}

	out := &UsageSeriesOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Keys = make([]KeySeries, 0, len(byKey))
	for k, a := range byKey {
		out.Body.Keys = append(out.Body.Keys, KeySeries{Key: k, Points: a.points})
	}
	sort.Slice(out.Body.Keys, func(i, j int) bool {
		return byKey[out.Body.Keys[i].Key].totalCost > byKey[out.Body.Keys[j].Key].totalCost
	})
	return out, nil
}

// --- model logs (P8-beyond control plane) ---

// ModelLogsInput names the backend whose logs to fetch.
type ModelLogsInput struct {
	Backend string `query:"backend" doc:"Backend id (<servedModel>#<index>), as in residency."`
	Tail    int    `query:"tail" default:"200" minimum:"1" maximum:"2000" doc:"Max trailing lines."`
}

// ModelLogsOutput is the captured stdout/stderr tail of a spawned backend.
type ModelLogsOutput struct {
	Body struct {
		Backend string   `json:"backend" doc:"Backend id."`
		Lines   []string `json:"lines" doc:"Captured lines, oldest first (empty for pure-proxy/absent)."`
	}
}

// ModelLogs returns a spawned backend's recent stdout/stderr.
func (h *Handlers) ModelLogs(_ context.Context, in *ModelLogsInput) (*ModelLogsOutput, error) {
	lines := h.Mgr.Logs(in.Backend)
	tail := in.Tail
	if tail <= 0 {
		tail = 200
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	out := &ModelLogsOutput{}
	out.Body.Backend = in.Backend
	out.Body.Lines = lines
	if out.Body.Lines == nil {
		out.Body.Lines = []string{}
	}
	return out, nil
}

// --- per-group usage time-series (P8-beyond): spot priority starvation ---

// GroupSeriesPoint is one bucket's metrics for a priority group.
type GroupSeriesPoint struct {
	Requests int64   `json:"requests" doc:"Requests in this bucket."`
	CostUSD  float64 `json:"costUsd" doc:"Cost in this bucket, USD."`
	DwellMS  int64   `json:"dwellMs" doc:"Total dwell in this bucket, ms."`
	Rejected int64   `json:"rejected" doc:"Requests backpressured (429) — queue-pressure signal."`
	QueuedMS int64   `json:"queuedMs" doc:"Total time queued before admit/reject, ms."`
}

// GroupSeries is one priority group's dense time series.
type GroupSeries struct {
	Group  string             `json:"group" doc:"Priority group name."`
	Points []GroupSeriesPoint `json:"points" doc:"One point per bucket, aligned to Buckets."`
}

// UsageSeriesByGroupOutput is a shared time axis plus one dense series per group
// — the data behind the stacked-area priority view (interactive-starvation watch).
type UsageSeriesByGroupOutput struct {
	Body struct {
		BucketMinutes int           `json:"bucketMinutes" doc:"Effective bucket width."`
		Buckets       []int64       `json:"buckets" doc:"Bucket start times, unix millis, ascending."`
		Groups        []GroupSeries `json:"groups" doc:"Per-group dense series, busiest first."`
	}
}

// UsageSeriesByGroup buckets activity by (priority group, time), resolving each
// caller key to its group. Stacked over time it reveals whether a high-priority
// lane (e.g. interactive) is being starved under contention.
func (h *Handlers) UsageSeriesByGroup(_ context.Context, in *UsageSeriesInput) (*UsageSeriesByGroupOutput, error) {
	windowHours := in.WindowHours
	if windowHours <= 0 {
		windowHours = 24
	}
	bucketMin := in.BucketMinutes
	if bucketMin <= 0 {
		bucketMin = 60
	}
	windowMS := int64(windowHours) * 3600_000
	bucketMS := int64(bucketMin) * 60_000
	for windowMS/bucketMS > maxSeriesBuckets {
		bucketMS *= 2
	}

	now := time.Now().UnixMilli()
	end := (now / bucketMS) * bucketMS
	start := ((now - windowMS) / bucketMS) * bucketMS
	var buckets []int64
	index := map[int64]int{}
	for b := start; b <= end; b += bucketMS {
		index[b] = len(buckets)
		buckets = append(buckets, b)
	}

	rows, err := h.Store.RollupSeries(now-windowMS, bucketMS)
	if err != nil {
		return nil, err
	}

	type acc struct {
		points []GroupSeriesPoint
		total  int64
	}
	byGroup := map[string]*acc{}
	for _, r := range rows {
		pos, ok := index[r.BucketTS]
		if !ok {
			continue
		}
		grp, _ := h.Cfg.ResolveGroup(r.Key)
		a := byGroup[grp]
		if a == nil {
			a = &acc{points: make([]GroupSeriesPoint, len(buckets))}
			byGroup[grp] = a
		}
		p := &a.points[pos]
		p.Requests += r.Requests
		p.CostUSD += r.CostUSD
		p.DwellMS += r.DwellMS
		p.Rejected += r.Rejected
		p.QueuedMS += r.QueuedMS
		a.total += r.Requests // COUNT(*) already includes rejected attempts
	}

	out := &UsageSeriesByGroupOutput{}
	out.Body.BucketMinutes = int(bucketMS / 60_000)
	out.Body.Buckets = buckets
	out.Body.Groups = make([]GroupSeries, 0, len(byGroup))
	for g, a := range byGroup {
		out.Body.Groups = append(out.Body.Groups, GroupSeries{Group: g, Points: a.points})
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool {
		return byGroup[out.Body.Groups[i].Group].total > byGroup[out.Body.Groups[j].Group].total
	})
	return out, nil
}

// --- lanes / live admission load (P8-beyond) ---

// LanesInput has no parameters.
type LanesInput struct{}

// GroupView is a priority group's policy plus its aggregated live load.
type GroupView struct {
	Name          string `json:"name" doc:"Priority group name."`
	Weight        int    `json:"weight" doc:"Fairshare weight (effective)."`
	ShareCurrency string `json:"shareCurrency" doc:"requests|dwell|cost."`
	Interruptible bool   `json:"interruptible" doc:"May a higher group preempt it?"`
	Active        int    `json:"active" doc:"In-flight slots across all backends."`
	Waiting       int    `json:"waiting" doc:"Queued requests across all backends."`
}

// GroupLoadView is one group's load on one backend.
type GroupLoadView struct {
	Group   string `json:"group" doc:"Priority group name."`
	Active  int    `json:"active" doc:"In-flight slots."`
	Waiting int    `json:"waiting" doc:"Queued requests."`
}

// BackendLoadView is a backend's live admission load with a group breakdown.
type BackendLoadView struct {
	Backend  string          `json:"backend" doc:"Backend id."`
	Capacity int             `json:"capacity" doc:"Configured slots."`
	Active   int             `json:"active" doc:"Slots in use."`
	Waiting  int             `json:"waiting" doc:"Queued requests."`
	Groups   []GroupLoadView `json:"groups" doc:"Per-group breakdown."`
}

// LanesOutput reports priority groups and per-backend admission load.
type LanesOutput struct {
	Body struct {
		Groups   []GroupView       `json:"groups" doc:"Priority groups with aggregated load."`
		Backends []BackendLoadView `json:"backends" doc:"Per-backend live load."`
	}
}

// Lanes returns priority-group policy joined with live admission load — the
// scheduler's per-backend slots/inflight/waiting, aggregated per group.
func (h *Handlers) Lanes(_ context.Context, _ *LanesInput) (*LanesOutput, error) {
	snap := h.Sched.Snapshot()

	// Aggregate live active/waiting per group across backends.
	type load struct{ active, waiting int }
	agg := map[string]*load{}
	out := &LanesOutput{}
	out.Body.Backends = make([]BackendLoadView, 0, len(snap.Backends))
	for _, b := range snap.Backends {
		bv := BackendLoadView{
			Backend: b.Backend, Capacity: b.Capacity, Active: b.Active, Waiting: b.Waiting,
			Groups: make([]GroupLoadView, 0, len(b.Groups)),
		}
		for _, g := range b.Groups {
			bv.Groups = append(bv.Groups, GroupLoadView{Group: g.Group, Active: g.Active, Waiting: g.Waiting})
			l := agg[g.Group]
			if l == nil {
				l = &load{}
				agg[g.Group] = l
			}
			l.active += g.Active
			l.waiting += g.Waiting
		}
		out.Body.Backends = append(out.Body.Backends, bv)
	}

	// Union of configured groups and any group seen live (e.g. synthesized default).
	names := map[string]struct{}{}
	for name := range h.Cfg.PriorityGroups {
		names[name] = struct{}{}
	}
	for name := range agg {
		names[name] = struct{}{}
	}
	out.Body.Groups = make([]GroupView, 0, len(names))
	for name := range names {
		pg := h.Cfg.PriorityGroups[name] // zero value if unlisted (e.g. default)
		gv := GroupView{
			Name:          name,
			Weight:        pg.EffectiveWeight(),
			ShareCurrency: h.Sched.ShareCurrency(name),
			Interruptible: pg.Interruptible,
		}
		if l := agg[name]; l != nil {
			gv.Active, gv.Waiting = l.active, l.waiting
		}
		out.Body.Groups = append(out.Body.Groups, gv)
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool { return out.Body.Groups[i].Name < out.Body.Groups[j].Name })
	return out, nil
}

// --- overview: model/lane definitions + capacity (P8-beyond) ---

// PoolDef is a server pool's declared total and reserved headroom.
type PoolDef struct {
	Pool         string `json:"pool" doc:"Pool name."`
	TotalBytes   int64  `json:"totalBytes" doc:"Declared pool size."`
	ReserveBytes int64  `json:"reserveBytes" doc:"Headroom kept free."`
}

// ServerDef is a server's declared capacity.
type ServerDef struct {
	Server        string    `json:"server" doc:"Server name."`
	MaxConcurrent int       `json:"maxConcurrent" doc:"Optional host concurrency cap (0 = none)."`
	Pools         []PoolDef `json:"pools" doc:"Declared memory pools."`
}

// BackendDef is one backend's definition. Spawnable backends carry their cmd;
// pure-proxy backends have an empty cmd and forward to Target. Auth headers on
// remote targets are NOT exposed.
type BackendDef struct {
	Index         int    `json:"index" doc:"Position in the model's backend list."`
	Type          string `json:"type" doc:"Cost class (local | claude | …)."`
	Quality       int    `json:"quality" doc:"Relative quality rank."`
	Spawnable     bool   `json:"spawnable" doc:"True if corrallm spawns it (has a cmd)."`
	Server        string `json:"server" doc:"Server it draws capacity from (spawned only)."`
	Target        string `json:"target" doc:"Forward URL (scheme://host:port; headers redacted)."`
	MaxConcurrent int    `json:"maxConcurrent" doc:"Admission slots."`
	MaxTokens     int    `json:"maxTokens" doc:"Per-backend max_tokens clamp (0 = none)."`
	Cmd           string `json:"cmd" doc:"Spawn command (empty for pure-proxy)."`
}

// ModelDef is a served model's residency policy + backend list.
type ModelDef struct {
	Name       string       `json:"name" doc:"Served model name."`
	Persistent bool         `json:"persistent" doc:"Pinned (preloaded, never evicted)."`
	TTL        string       `json:"ttl" doc:"Idle keep-warm window (sticky)."`
	EvictCost  string       `json:"evictCost" doc:"Eviction resistance (sticky)."`
	Spawnable  bool         `json:"spawnable" doc:"Has at least one spawnable backend."`
	Backends   []BackendDef `json:"backends" doc:"Ordered backend list."`
}

// StageView summarizes a group's saturation policy for one backend type.
type StageView struct {
	Type   string `json:"type" doc:"Backend type (or \"default\")."`
	Policy string `json:"policy" doc:"Human-readable stage summary."`
}

// GroupDef is a priority group's (lane's) policy.
type GroupDef struct {
	Name          string      `json:"name" doc:"Group name."`
	Weight        int         `json:"weight" doc:"Fairshare weight."`
	ShareCurrency string      `json:"shareCurrency" doc:"requests | dwell | cost."`
	Interruptible bool        `json:"interruptible" doc:"May a higher group preempt it?"`
	AcceptDegrade bool        `json:"acceptDegrade" doc:"Opts into quality-degrade fall-through."`
	QualityFloor  int         `json:"qualityFloor" doc:"Lowest accepted quality when degrading."`
	Stages        []StageView `json:"stages" doc:"Per-type saturation policy."`
}

// KeyDef maps a caller key to its group.
type KeyDef struct {
	Key   string `json:"key" doc:"Caller key."`
	Group string `json:"group" doc:"Priority group it resolves to."`
}

// OverviewInput has no parameters.
type OverviewInput struct{}

// OverviewOutput is the loaded config rendered for the Overview control plane.
type OverviewOutput struct {
	Body struct {
		Servers []ServerDef `json:"servers" doc:"Declared host capacity."`
		Models  []ModelDef  `json:"models" doc:"Served models + backend definitions."`
		Groups  []GroupDef  `json:"groups" doc:"Priority-group (lane) policies."`
		Keys    []KeyDef    `json:"keys" doc:"Caller key → group mappings."`
	}
}

// stageSummary renders a saturation Stage as a short human-readable policy.
func stageSummary(s config.Stage) string {
	var parts []string
	if s.Preempt {
		parts = append(parts, "preempt")
	}
	if s.Queue {
		parts = append(parts, "queue")
	}
	if s.Spill || s.FallThrough {
		parts = append(parts, "spill")
	}
	if s.Reject {
		parts = append(parts, "reject")
	}
	if s.Then != "" {
		parts = append(parts, "then "+s.Then)
	}
	for dim, lim := range s.Limits {
		parts = append(parts, "limit "+dim+"="+lim)
	}
	if len(parts) == 0 {
		return "reject"
	}
	return strings.Join(parts, ", ")
}

// Overview returns model/lane definitions and declared system capacity.
func (h *Handlers) Overview(_ context.Context, _ *OverviewInput) (*OverviewOutput, error) {
	out := &OverviewOutput{}

	for name, srv := range h.Cfg.Servers {
		sd := ServerDef{Server: name, MaxConcurrent: srv.MaxConcurrent}
		totals, _ := config.ParseSizes(srv.Pools)
		reserve, _ := config.ParseSizes(srv.Reserve)
		for pool, total := range totals {
			sd.Pools = append(sd.Pools, PoolDef{Pool: pool, TotalBytes: total, ReserveBytes: reserve[pool]})
		}
		sort.Slice(sd.Pools, func(i, j int) bool { return sd.Pools[i].Pool < sd.Pools[j].Pool })
		out.Body.Servers = append(out.Body.Servers, sd)
	}
	sort.Slice(out.Body.Servers, func(i, j int) bool { return out.Body.Servers[i].Server < out.Body.Servers[j].Server })

	for name, m := range h.Cfg.Models {
		md := ModelDef{Name: name, Persistent: m.Persistent}
		if m.Sticky != nil {
			md.TTL, md.EvictCost = m.Sticky.TTL, m.Sticky.EvictCost
		}
		for i, b := range m.Backends {
			bd := BackendDef{
				Index: i, Type: b.Type, Quality: b.Quality, Spawnable: b.Cmd != "",
				Server: b.Server, MaxConcurrent: b.Slots(), MaxTokens: b.MaxTokens, Cmd: b.Cmd,
			}
			if t, err := b.ProxyTarget(); err == nil {
				bd.Target = t.URL.String() // headers (auth) intentionally omitted
			}
			if bd.Spawnable {
				md.Spawnable = true
			}
			md.Backends = append(md.Backends, bd)
		}
		out.Body.Models = append(out.Body.Models, md)
	}
	sort.Slice(out.Body.Models, func(i, j int) bool { return out.Body.Models[i].Name < out.Body.Models[j].Name })

	for name, g := range h.Cfg.PriorityGroups {
		gd := GroupDef{
			Name: name, Weight: g.EffectiveWeight(), ShareCurrency: shareCurrencyOf(g),
			Interruptible: g.Interruptible, AcceptDegrade: g.AcceptDegrade, QualityFloor: g.QualityFloor,
		}
		for typ, st := range g.OnSaturated {
			gd.Stages = append(gd.Stages, StageView{Type: typ, Policy: stageSummary(st)})
		}
		sort.Slice(gd.Stages, func(i, j int) bool { return gd.Stages[i].Type < gd.Stages[j].Type })
		out.Body.Groups = append(out.Body.Groups, gd)
	}
	sort.Slice(out.Body.Groups, func(i, j int) bool { return out.Body.Groups[i].Name < out.Body.Groups[j].Name })

	for k, grp := range h.Cfg.Keys {
		out.Body.Keys = append(out.Body.Keys, KeyDef{Key: k, Group: grp})
	}
	sort.Slice(out.Body.Keys, func(i, j int) bool { return out.Body.Keys[i].Key < out.Body.Keys[j].Key })

	return out, nil
}

// shareCurrencyOf returns a group's configured share currency, defaulting to requests.
func shareCurrencyOf(g config.PriorityGroup) string {
	switch g.ShareCurrency {
	case "dwell", "cost":
		return g.ShareCurrency
	default:
		return "requests"
	}
}

// --- load / unload mutations (P8-beyond control plane) ---

// ModelActionInput names the served model to load/unload.
type ModelActionInput struct {
	Body struct {
		Model string `json:"model" doc:"Served model name."`
	}
}

// ModelActionOutput reports the result of a load/unload.
type ModelActionOutput struct {
	Body struct {
		OK      bool   `json:"ok" doc:"Whether the action succeeded."`
		Message string `json:"message" doc:"Human-readable result or error."`
		Backend string `json:"backend" doc:"Backend loaded (load only)."`
		Evicted int    `json:"evicted" doc:"Backends evicted (unload only)."`
	}
}

// LoadModel warms a model on demand (spawns its first spawnable backend).
func (h *Handlers) LoadModel(ctx context.Context, in *ModelActionInput) (*ModelActionOutput, error) {
	out := &ModelActionOutput{}
	name, err := h.Mgr.LoadModel(ctx, in.Body.Model)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Backend = name
	out.Body.Message = "loaded " + name
	return out, nil
}

// UnloadModel evicts a model's resident backends (refuses pinned / in-flight).
func (h *Handlers) UnloadModel(_ context.Context, in *ModelActionInput) (*ModelActionOutput, error) {
	out := &ModelActionOutput{}
	n, err := h.Mgr.UnloadModel(in.Body.Model)
	if err != nil {
		out.Body.Message = err.Error()
		return out, nil
	}
	out.Body.OK = true
	out.Body.Evicted = n
	out.Body.Message = fmt.Sprintf("evicted %d backend(s)", n)
	return out, nil
}

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
