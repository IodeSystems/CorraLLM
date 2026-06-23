// Package api holds corrallm's typed handlers and the gat gateway wiring. Each
// operation is registered once with gat.Register and is thereby reachable over
// REST (huma), GraphQL, and gRPC — the "register once → typed everywhere" loop.
// P0 ships the meta operations (health, version) that exercise the whole
// codegen pipeline; the inference proxy + scheduler operations land in P1+.
package api

import (
	"context"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Handlers carries the dependencies every operation needs. It grows as phases
// add the scheduler, residency, and cost subsystems.
type Handlers struct {
	Version string
	Cfg     *config.Config
	Store   *store.Store
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

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
