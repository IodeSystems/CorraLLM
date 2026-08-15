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
		OperationID: "utilization",
		Method:      http.MethodGet,
		Path:        "/api/v1/utilization",
		Summary:     "Per-model pressure: slots in use, queue, promises made, and estimated vs measured wait.",
		Tags:        []string{"observability"},
	}, h.Utilization)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "serviceProfiles",
		Method:      http.MethodGet,
		Path:        "/api/v1/utilization/profiles",
		Summary:     "Per-caller service-time distribution: how long each caller's work occupies a slot, and how variable.",
		Tags:        []string{"observability"},
	}, h.ServiceProfiles)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "retryPromises",
		Method:      http.MethodGet,
		Path:        "/api/v1/activity/promises",
		Summary:     "Callers we told to come back later, when they are due, and whether they returned.",
		Tags:        []string{"observability"},
	}, h.RetryPromises)

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
		OperationID: "startBenchRun",
		Method:      http.MethodPost,
		Path:        "/api/v1/bench/run",
		Summary:     "Spawn llm-bench. Queues for admission like any other caller: holds no lease, evicts nothing, locks nobody out.",
		Tags:        []string{"control"},
	}, h.StartBenchRun)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchStatus",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/status",
		Summary:     "Progress and captured output of the current or last bench run.",
		Tags:        []string{"observability"},
	}, h.BenchStatus)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "cancelBenchRun",
		Method:      http.MethodPost,
		Path:        "/api/v1/bench/cancel",
		Summary:     "Stop an in-flight bench run.",
		Tags:        []string{"control"},
	}, h.CancelBenchRun)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "publishBenchResult",
		Method:      http.MethodPost,
		Path:        "/api/v1/measurements/result",
		Summary:     "Publish one model's aggregate outcome from a bench run (llm-bench).",
		Tags:        []string{"control"},
	}, h.PublishBenchResult)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "publishBenchProbeResults",
		Method:      http.MethodPost,
		Path:        "/api/v1/measurements/probes",
		Summary:     "Publish a bench run's per-probe detail, including skipped probes (llm-bench).",
		Tags:        []string{"control"},
	}, h.PublishBenchProbeResults)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchProbeCatalog",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probes/catalog",
		Summary:     "Which probes EXIST and would run — the library, not results. Includes ones that fail to load.",
		Tags:        []string{"observability"},
	}, h.BenchProbeCatalog)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchProbes",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probes",
		Summary:     "One model's last bench run broken out by capability, probe by probe.",
		Tags:        []string{"observability"},
	}, h.BenchProbesByCapability)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "requestOverhead",
		Method:      http.MethodGet,
		Path:        "/api/v1/overhead",
		Summary:     "Queue + load time corrallm added to one caller's requests to one model in a window.",
		Tags:        []string{"observability"},
	}, h.RequestOverhead)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchRuns",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/runs",
		Summary:     "Index of benchmark runs, newest first — what has been measured and when.",
		Tags:        []string{"observability"},
	}, h.BenchRuns)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchRunDetail",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/run",
		Summary:     "What one run did: every model in it, probe by probe (optionally one model).",
		Tags:        []string{"observability"},
	}, h.BenchRunDetail)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchProbeHistory",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probe/history",
		Summary:     "One probe across every model and run that ran it, plus its description.",
		Tags:        []string{"observability"},
	}, h.BenchProbeHistory)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "freeRoster",
		Method:      http.MethodGet,
		Path:        "/api/v1/free-roster",
		Summary:     "Each provider's currently-free model roster (P16e).",
		Tags:        []string{"observability"},
	}, h.FreeRoster)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "quotaLedger",
		Method:      http.MethodGet,
		Path:        "/api/v1/quota",
		Summary:     "Free-tier budget ledger: each remote backend's remaining rate-limit budget (P16).",
		Tags:        []string{"observability"},
	}, h.QuotaLedger)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchArmMatrix",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/arms",
		Summary:     "A/B arms compared across every model that ran them, paired per probe.",
		Tags:        []string{"observability"},
	}, h.BenchArmMatrix)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchProbeDetail",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probe/detail",
		Summary:     "One probe's stage-by-stage metrics and check verdicts — why it scored what it did.",
		Tags:        []string{"observability"},
	}, h.BenchProbeDetail)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchTranscript",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probe/transcript",
		Summary:     "One probe's recorded conversation, read from the run's artifacts on disk.",
		Tags:        []string{"observability"},
	}, h.BenchTranscript)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchJournal",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/probe/journal",
		Summary:     "One probe's tool-call journal, including bait and poisoned-result flags.",
		Tags:        []string{"observability"},
	}, h.BenchJournal)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchCapabilityMatrix",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/capabilities",
		Summary:     "Models ranked within each capability — the comparison a run-wide pass rate cannot make.",
		Tags:        []string{"observability"},
	}, h.BenchCapabilityMatrix)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchResults",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/results",
		Summary:     "Latest bench result per model, or one model's history.",
		Tags:        []string{"observability"},
	}, h.BenchResults)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "benchPlan",
		Method:      http.MethodGet,
		Path:        "/api/v1/bench/plan",
		Summary:     "Which models lack measurement data, and what to run for them.",
		Tags:        []string{"observability"},
	}, h.BenchPlan)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "unloadAllModels",
		Method:      http.MethodPost,
		Path:        "/api/v1/models/unload-all",
		Summary:     "Evict every evictable resident (calibration primitive; pinned/in-flight are reported as skipped).",
		Tags:        []string{"control"},
	}, h.UnloadAllModels)

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
		OperationID: "listApprovals",
		Method:      http.MethodGet,
		Path:        "/api/v1/approvals",
		Summary:     "Every approval decision, plus the discovered models still awaiting one.",
		Tags:        []string{"config"},
	}, h.ListApprovals)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "decideApproval",
		Method:      http.MethodPost,
		Path:        "/api/v1/approvals/decide",
		Summary:     "Approve or reject a discovered model on one credential, with the lanes and position it should take.",
		Tags:        []string{"config"},
	}, h.DecideApproval)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "pauseModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/models/pause",
		Summary:     "Take a model out of service: unload it and never load it again until resumed (or until resumeAt).",
		Tags:        []string{"control"},
	}, h.PauseModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "unpauseModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/models/unpause",
		Summary:     "Return a paused model to service (reloading it if it is pinned).",
		Tags:        []string{"control"},
	}, h.UnpauseModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "pauseExtension",
		Method:      http.MethodPost,
		Path:        "/api/v1/extensions/pause",
		Summary:     "Take an extension (and every model it provides) out of service until resumed.",
		Tags:        []string{"control"},
	}, h.PauseExtension)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "unpauseExtension",
		Method:      http.MethodPost,
		Path:        "/api/v1/extensions/unpause",
		Summary:     "Return a paused extension and all its models to service.",
		Tags:        []string{"control"},
	}, h.UnpauseExtension)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "loadExtension",
		Method:      http.MethodPost,
		Path:        "/api/v1/extensions/load",
		Summary:     "Start an extension's process (loads every model it provides).",
		Tags:        []string{"control"},
	}, h.LoadExtension)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "unloadExtension",
		Method:      http.MethodPost,
		Path:        "/api/v1/extensions/unload",
		Summary:     "Stop an extension (drains in-flight requests; unloads every model it provides).",
		Tags:        []string{"control"},
	}, h.UnloadExtension)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "extensions",
		Method:      http.MethodGet,
		Path:        "/api/v1/extensions",
		Summary:     "Declared extensions, the models they provide, and process state.",
		Tags:        []string{"observability"},
	}, h.Extensions)

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
		OperationID: "usageSeriesByModel",
		Method:      http.MethodGet,
		Path:        "/api/v1/usage/series-by-model",
		Summary:     "Per-served-model usage time-series, optionally scoped to one caller key.",
		Tags:        []string{"observability"},
	}, h.UsageSeriesByModel)

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
		OperationID: "keys",
		Method:      http.MethodGet,
		Path:        "/api/v1/keys",
		Summary:     "Caller keys: configured lanes plus keys seen in traffic but never assigned.",
		Tags:        []string{"observability"},
	}, h.Keys)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "groups",
		Method:      http.MethodGet,
		Path:        "/api/v1/groups",
		Summary:     "Priority groups + live per-backend admission load.",
		Tags:        []string{"observability"},
	}, h.Groups)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "entryYaml",
		Method:      http.MethodGet,
		Path:        "/api/v1/config/{kind}/{name}/yaml",
		Summary:     "One model, server or lane as YAML, for editing.",
		Tags:        []string{"config"},
	}, h.EntryYAML)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "putEntryYaml",
		Method:      http.MethodPut,
		Path:        "/api/v1/config/{kind}/{name}/yaml",
		Summary:     "Replace a model, server or lane from YAML; validated against the whole config.",
		Tags:        []string{"config"},
	}, h.PutEntryYAML)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "deleteEntry",
		Method:      http.MethodDelete,
		Path:        "/api/v1/config/{kind}/{name}",
		Summary:     "Remove a model, server or lane.",
		Tags:        []string{"config"},
	}, h.DeleteEntry)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "upsertModel",
		Method:      http.MethodPut,
		Path:        "/api/v1/config/models/{name}",
		Summary:     "Create or replace a model; validates, writes and applies it live.",
		Tags:        []string{"config"},
	}, h.UpsertModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "modelSpec",
		Method:      http.MethodGet,
		Path:        "/api/v1/config/models/{name}/spec",
		Summary:     "One model in the shape the form edits, plus the fields the form does not cover.",
		Tags:        []string{"config"},
	}, h.GetModelSpec)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "probeModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/config/models/{name}/probe",
		Summary:     "Ask a CONFIGURED model what it can do — context, slots, modalities, tools, measured footprint. Leaves it as it found it.",
		Tags:        []string{"config"},
	}, h.ProbeModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "trialModel",
		Method:      http.MethodPost,
		Path:        "/api/v1/config/models/trial",
		Summary:     "Spawn an uncommitted cmd, report every stage and log line, then tear it down. Writes nothing.",
		Tags:        []string{"config"},
	}, h.TrialModel)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "updateNotes",
		Method:      http.MethodPut,
		Path:        "/api/v1/config/notes/{kind}/{name}",
		Summary:     "Set the notes kept with a model, server or lane.",
		Tags:        []string{"config"},
	}, h.UpdateNotes)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "agentEnroll",
		Method:      http.MethodPost,
		Path:        "/api/v1/agents/enroll",
		Summary:     "Attach a machine using a one-time enrollment token; creates its server entry.",
		Tags:        []string{"agents"},
	}, h.AgentEnroll)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "mintEnrollmentToken",
		Method:      http.MethodPost,
		Path:        "/api/v1/agents/tokens",
		Summary:     "Mint a one-time enrollment token (admin).",
		Tags:        []string{"agents"},
	}, h.MintEnrollmentToken)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "agentHeartbeat",
		Method:      http.MethodPost,
		Path:        "/api/v1/agents/heartbeat",
		Summary:     "An agent reports that it is alive (authenticated by its own per-server token).",
		Tags:        []string{"agents"},
	}, h.AgentHeartbeat)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "cancelRequest",
		Method:      http.MethodPost,
		Path:        "/api/v1/requests/cancel",
		Summary:     "Cancel one in-flight request (queued, loading or streaming).",
		Tags:        []string{"control"},
	}, h.CancelRequest)

	gat.Register(humaAPI, g, huma.Operation{
		OperationID: "activeRequests",
		Method:      http.MethodGet,
		Path:        "/api/v1/active",
		Summary:     "In-flight requests (queued / loading / streaming).",
		Tags:        []string{"observability"},
	}, h.ActiveRequests)

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
