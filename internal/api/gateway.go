package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/iodesystems/gwag/gw/gat"
)

// BuildGateway registers every corrallm operation once against a gat gateway
// and finalizes it: each op is reachable over REST (huma) on router, and over
// GraphQL at POST {prefix}/graphql with the SDL at GET {prefix}/schema/graphql.
// The same typed handler backs every transport. RegisterHuma builds the GraphQL
// schema, so GraphQLSDL() is valid on the returned gateway.
//
// Passing a fresh *Handlers{} (no deps) is valid for schema dumping — the
// registration only reflects handler signatures, it does not invoke them.
func BuildGateway(router chi.Router, h *Handlers) (*gat.Gateway, error) {
	humaAPI := humachi.New(router, huma.DefaultConfig("corrallm", "0.1.0"))
	g, err := gat.New()
	if err != nil {
		return nil, err
	}
	// Emit Long (int64) as a JSON string for a uniform id contract in the UI.
	g.LongAsNumber(false)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/api/v1/health",
		Summary:     "Liveness probe + build version.",
		Tags:        []string{"meta"},
	}, h.Health)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "configSummary",
		Method:      http.MethodGet,
		Path:        "/api/v1/config/summary",
		Summary:     "Names declared in the loaded config (servers, models, groups).",
		Tags:        []string{"meta"},
	}, h.ConfigSummary)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "recentActivity",
		Method:      http.MethodGet,
		Path:        "/api/v1/activity",
		Summary:     "Most recent proxied-request records (dwell/tokens/$).",
		Tags:        []string{"observability"},
	}, h.RecentActivity)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "activityDetail",
		Method:      http.MethodGet,
		Path:        "/api/v1/activity/detail",
		Summary:     "One activity row with captured request/response payloads (P10c).",
		Tags:        []string{"observability"},
	}, h.ActivityDetail)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "overview",
		Method:      http.MethodGet,
		Path:        "/api/v1/overview",
		Summary:     "Model/lane definitions and declared system capacity.",
		Tags:        []string{"observability"},
	}, h.Overview)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "loadModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/models/load",
		Summary:     "Warm a model on demand (spawn its first spawnable backend).",
		Tags:        []string{"control"},
	}, h.LoadModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "beginCalibration",
		Method:      http.MethodPost,
		Path:        "/api/v1/calibrate/begin",
		Summary:     "Claim exclusive access for a measurement run (EVICTS models; all other callers get 429).",
		Tags:        []string{"control"},
	}, h.BeginCalibration)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "endCalibration",
		Method:      http.MethodPost,
		Path:        "/api/v1/calibrate/end",
		Summary:     "Release the calibration lease early.",
		Tags:        []string{"control"},
	}, h.EndCalibration)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "calibrationStatus",
		Method:      http.MethodGet,
		Path:        "/api/v1/calibrate/status",
		Summary:     "Report whether an exclusive calibration lease is held.",
		Tags:        []string{"observability"},
	}, h.CalibrationStatus)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "publishTuneProfile",
		Method:      http.MethodPost,
		Path:        "/api/v1/measurements/tune",
		Summary:     "Publish an externally-measured VRAM profile (llm-bench).",
		Tags:        []string{"control"},
	}, h.PublishTuneProfile)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "publishVerifiedCapability",
		Method:      http.MethodPost,
		Path:        "/api/v1/measurements/capability",
		Summary:     "Publish an OBSERVED capability verdict (llm-bench).",
		Tags:        []string{"control"},
	}, h.PublishVerifiedCapability)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "unloadModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/models/unload",
		Summary:     "Evict a model's resident backends (refuses pinned / in-flight).",
		Tags:        []string{"control"},
	}, h.UnloadModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "modelLogs",
		Method:      http.MethodGet,
		Path:        "/api/v1/models/logs",
		Summary:     "Recent stdout/stderr of a spawned backend.",
		Tags:        []string{"observability"},
	}, h.ModelLogs)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "residency",
		Method:      http.MethodGet,
		Path:        "/api/v1/residency",
		Summary:     "Server pool budgets/usage and currently resident backends.",
		Tags:        []string{"observability"},
	}, h.Residency)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "usageRollup",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/rollup",
		Summary:     "Per-model usage rollup (requests/tokens/dwell/$) over a window.",
		Tags:        []string{"observability"},
	}, h.UsageRollup)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "usageSeries",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/series",
		Summary:     "Per-key usage time-series (requests/$/energy/dwell), bucketed.",
		Tags:        []string{"observability"},
	}, h.UsageSeries)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "queueDepth",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/queue-depth",
		Summary:     "Sampled per-lane queue depth (active/waiting) over time.",
		Tags:        []string{"observability"},
	}, h.QueueDepth)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "usageSeriesByGroup",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/series-by-group",
		Summary:     "Per-priority-group usage time-series (for starvation watch).",
		Tags:        []string{"observability"},
	}, h.UsageSeriesByGroup)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "usageByKey",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/by-key",
		Summary:     "Per-caller-key usage rollup (requests/tokens/dwell/$/energy).",
		Tags:        []string{"observability"},
	}, h.UsageByKey)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "groups",
		Method:      http.MethodGet,
		Path:        "/api/v1/groups",
		Summary:     "Priority groups + live per-backend admission load.",
		Tags:        []string{"observability"},
	}, h.Groups)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "reservations",
		Method:      http.MethodGet,
		Path:        "/api/v1/reservations",
		Summary:     "Live slot reservations (interactive-headroom leases).",
		Tags:        []string{"observability"},
	}, h.Reservations)

	// Finalize: ingest the OpenAPI doc, build the GraphQL schema, mount
	// /api/graphql + /api/schema/*.
	if err := gat.RegisterHuma(humaAPI, g, "/api"); err != nil {
		return nil, err
	}
	return g, nil
}
