// Package api holds corrallm's typed handlers and the gat gateway wiring. Each
// operation is registered once with gat.Register and is thereby reachable over
// REST (huma), GraphQL, and gRPC — the "register once → typed everywhere" loop.
// P0 ships the meta operations (health, version) that exercise the whole
// codegen pipeline; the inference proxy + scheduler operations land in P1+.
package api

import (
	"context"
	"sort"
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

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
