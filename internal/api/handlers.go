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

// keys returns a map's keys as a slice (GraphQL needs a concrete list shape).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
