// Package api holds corrallm's typed handlers and the gat gateway wiring. Each
// operation is registered once with gat.Register and is thereby reachable over
// REST (huma), GraphQL, and gRPC — the "register once → typed everywhere" loop.
// P0 ships the meta operations (health, version) that exercise the whole
// codegen pipeline; the inference proxy + scheduler operations land in P1+.
package api

import (
	"context"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/store"
)

// Handlers carries the dependencies every operation needs. It grows as phases
// add the scheduler, residency, and cost subsystems.
type Handlers struct {
	Version string
	Cfg     *config.Config
	Store   *store.Store
	Mgr     *proc.Manager // residency introspection (P8)
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

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
