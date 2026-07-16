// Command corrallm is the OpenAI-compatible reverse proxy + model lifecycle
// manager + fairshare scheduler. P0 ships the scaffold: a gat gateway (REST +
// GraphQL), the SPA, config loading, and the SQLite store. The proxy core and
// scheduler land in later phases (see plan/plan.md).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/api"
	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/tune"
	"github.com/iodesystems/corrallm/internal/webui"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "corrallm",
		Short:         "OpenAI-compatible LLM reverse proxy, lifecycle manager, and fairshare scheduler",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newDumpGraphQLCmd(), newVersionCmd(), newIntrospectCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

// dump-graphql renders the gat SDL to a file with no DB and no server — the
// committed snapshot the UI codegen validates against (see bin/gen).
func newDumpGraphQLCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dump-graphql <path>",
		Short: "Write the GraphQL SDL snapshot (no DB, no server)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			g, err := api.BuildGateway(chi.NewRouter(), &api.Handlers{})
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[0], []byte(g.GraphQLSDL()), 0o644); err != nil {
				return err
			}
			slog.Info("wrote GraphQL SDL", "path", args[0])
			return nil
		},
	}
}

// introspect reports live GPU VRAM and each configured model's cached
// slot-tuning profile. Read-only by design: it loads the config to enumerate
// models but never opens the SQLite store or spawns anything — a diagnostic
// a human (or a script, via --json) runs alongside a running `serve` without
// disturbing it.
func newIntrospectCmd() *cobra.Command {
	var (
		configPath, dbPath string
		vramMargin         int
		asJSON             bool
	)
	cmd := &cobra.Command{
		Use:   "introspect",
		Short: "Report GPU VRAM and cached slot-tuning profiles (read-only; spawns nothing)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbPathResolved := pick(dbPath, envOr("CORRALLM_DB", "./home/var/corrallm.db"))
			return introspect(cmd, introspectOpts{
				configPath: pick(configPath, envOr("CORRALLM_CONFIG", "./corrallm.yaml")),
				tuneCache:  envOr("CORRALLM_TUNE_CACHE", defaultTuneCachePath(dbPathResolved)),
				vramMargin: pickInt(vramMargin, envInt("CORRALLM_VRAM_MARGIN", 512)),
				json:       asJSON,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&configPath, "config", "", "path to the corrallm YAML config (default ./corrallm.yaml or CORRALLM_CONFIG)")
	f.StringVar(&dbPath, "db", "", "path used only to resolve the default tune-cache location, <db-dir>/vram-profile.json (default ./home/var/corrallm.db or CORRALLM_DB); introspect never opens the DB itself")
	f.IntVar(&vramMargin, "vram-margin", 0, "MiB of free VRAM kept back when computing the slot count a model would tune to right now (default 512 or CORRALLM_VRAM_MARGIN; must match serve's setting to predict its behavior)")
	f.BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	return cmd
}

type introspectOpts struct {
	configPath, tuneCache string
	vramMargin            int
	json                  bool
}

// introspectReport is the `corrallm introspect` output shape (JSON or table).
type introspectReport struct {
	GPU      *introspectGPU    `json:"gpu,omitempty"`
	GPUError string            `json:"gpu_error,omitempty"` // set (only) when nvidia-smi is unavailable
	Models   []introspectModel `json:"models"`
}

type introspectGPU struct {
	Name     string `json:"name"`
	TotalMiB int    `json:"total_mib"`
	UsedMiB  int    `json:"used_mib"`
	FreeMiB  int    `json:"free_mib"`
}

type introspectModel struct {
	Name          string `json:"name"`
	ConfigSlots   int    `json:"config_slots"` // maxConcurrent (today's behavior, unconditionally)
	HasProfile    bool   `json:"has_profile"`
	BaseMiB       int    `json:"base_mib,omitempty"`
	PerSlotMiB    int    `json:"per_slot_mib,omitempty"`
	PeakMiB       int    `json:"peak_mib,omitempty"`
	MeasuredSlots int    `json:"measured_slots,omitempty"`
	Ctx           int    `json:"ctx,omitempty"`
	TunedSlots    int    `json:"tuned_slots,omitempty"` // what SlotsFor picks against CURRENT free VRAM; 0 = would not tune
}

func introspect(cmd *cobra.Command, o introspectOpts) error {
	out := cmd.OutOrStdout()

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	cache, err := tune.New(o.tuneCache)
	if err != nil {
		return err
	}

	stats, gpuErr := gpu.Probe()
	report := introspectReport{}
	budget := 0
	if gpuErr != nil {
		report.GPUError = gpuErr.Error()
	} else {
		report.GPU = &introspectGPU{Name: stats.Name, TotalMiB: stats.TotalMiB, UsedMiB: stats.UsedMiB, FreeMiB: stats.FreeMiB}
		budget = stats.FreeMiB - o.vramMargin
	}

	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mc := cfg.Models[name]
		im := introspectModel{Name: name, ConfigSlots: mc.Slots()}
		if gpuErr == nil {
			if p, ok := cache.Get(stats.Name, name); ok {
				im.HasProfile = true
				im.BaseMiB, im.PerSlotMiB, im.PeakMiB, im.MeasuredSlots, im.Ctx = p.BaseMiB, p.PerSlotMiB, p.PeakMiB, p.MeasuredSlots, p.Ctx
				if n, ok := cache.SlotsFor(stats.Name, name, budget); ok {
					im.TunedSlots = n
				}
			}
		}
		report.Models = append(report.Models, im)
	}

	if o.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	if report.GPUError != "" {
		fmt.Fprintf(out, "GPU introspection unavailable: %s\n", report.GPUError)
		fmt.Fprintf(out, "(model profiles below are as last cached; live tuned-slot counts can't be computed without a GPU read)\n\n")
	} else {
		fmt.Fprintf(out, "GPU: %s  total=%dMiB used=%dMiB free=%dMiB  (margin=%dMiB budget=%dMiB)\n\n",
			report.GPU.Name, report.GPU.TotalMiB, report.GPU.UsedMiB, report.GPU.FreeMiB, o.vramMargin, budget)
	}
	for _, m := range report.Models {
		if !m.HasProfile {
			fmt.Fprintf(out, "  %-30s config_slots=%-3d  no cached profile\n", m.Name, m.ConfigSlots)
			continue
		}
		fmt.Fprintf(out, "  %-30s config_slots=%-3d tuned_slots=%-3d base=%dMiB per_slot=%dMiB peak=%dMiB measured_slots=%d ctx=%d\n",
			m.Name, m.ConfigSlots, m.TunedSlots, m.BaseMiB, m.PerSlotMiB, m.PeakMiB, m.MeasuredSlots, m.Ctx)
	}
	return nil
}

func newServeCmd() *cobra.Command {
	var (
		home, service, webRoot, configPath, dbPath string
		healthTimeout, activityRetention           time.Duration
		requestTimeout                             time.Duration
		capturePayloads, convertPDFs, ocrPDFs      bool
		pdfMaxChars, ocrMaxPages                   int
		realtimeIdle, realtimeMaxSession           time.Duration
		reservationMaxTTL                          time.Duration
		tuneCachePath                              string
		vramMargin                                 int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the proxy server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if n, err := config.LoadInto(home, service); err != nil {
				return fmt.Errorf("properties: %w", err)
			} else if n > 0 {
				slog.Info("properties loaded", "keys", n, "home", home, "service", service)
			}
			dbPathResolved := pick(dbPath, envOr("CORRALLM_DB", "./home/var/corrallm.db"))
			return serve(cmd.Context(), serveOpts{
				webRoot:            pick(webRoot, envOr("WEB_ROOT", "./ui/dist")),
				configPath:         pick(configPath, envOr("CORRALLM_CONFIG", "./corrallm.yaml")),
				dbPath:             dbPathResolved,
				addr:               envOr("ADDR", ":6502"),
				healthTimeout:      pickDuration(healthTimeout, envDuration("CORRALLM_HEALTH_TIMEOUT", 0)),
				tokenPath:          filepath.Join(home, "admin.token"),
				activityRetention:  pickDuration(activityRetention, envDuration("CORRALLM_ACTIVITY_RETENTION", 30*24*time.Hour)),
				requestTimeout:     pickDuration(requestTimeout, envDuration("CORRALLM_REQUEST_TIMEOUT", 0)),
				capturePayloads:    capturePayloads,
				convertPDFs:        convertPDFs,
				pdfMaxChars:        pdfMaxChars,
				ocrPDFs:            ocrPDFs,
				ocrMaxPages:        ocrMaxPages,
				realtimeIdle:       pickDuration(realtimeIdle, envDuration("CORRALLM_REALTIME_IDLE_TIMEOUT", 5*time.Minute)),
				realtimeMaxSession: pickDuration(realtimeMaxSession, envDuration("CORRALLM_REALTIME_MAX_SESSION", 0)),
				reservationMaxTTL:  pickDuration(reservationMaxTTL, envDuration("CORRALLM_RESERVATION_MAX_TTL", 5*time.Minute)),
				tuneCachePath:      pick(tuneCachePath, envOr("CORRALLM_TUNE_CACHE", defaultTuneCachePath(dbPathResolved))),
				vramMargin:         pickInt(vramMargin, envInt("CORRALLM_VRAM_MARGIN", 512)),
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&home, "home", envOr("CORRALLM_HOME", "./home"), "config home holding the layered .properties files")
	f.StringVar(&service, "service", envOr("CORRALLM_SLOT", "dev"), "service/slot selecting override .properties (dev|current|next)")
	f.StringVar(&webRoot, "web-root", "", "directory to serve the SPA from (default ./ui/dist or WEB_ROOT)")
	f.StringVar(&configPath, "config", "", "path to the corrallm YAML config (default ./corrallm.yaml or CORRALLM_CONFIG)")
	f.StringVar(&dbPath, "db", "", "path to the SQLite database (default ./home/var/corrallm.db or CORRALLM_DB)")
	f.DurationVar(&healthTimeout, "health-timeout", 0, "max time a cold backend spawn may take to become healthy (default 120s or CORRALLM_HEALTH_TIMEOUT); raise for large models")
	f.DurationVar(&activityRetention, "activity-retention", 0, "delete activity-log rows older than this (default 720h/30d or CORRALLM_ACTIVITY_RETENTION; 0 disables)")
	f.DurationVar(&requestTimeout, "request-timeout", 0, "max wall-clock for one proxied request before corrallm cancels it (or CORRALLM_REQUEST_TIMEOUT; 0 = no corrallm deadline, defer to client + backend)")
	f.BoolVar(&capturePayloads, "capture-payloads", true, "capture per-request request/response payloads onto the activity log (capped; binary audio summarized; pruned with --activity-retention)")
	f.BoolVar(&convertPDFs, "convert-pdfs", true, "auto-extract PDF attachments in chat requests into injected text (via pdftotext) so text models can read them")
	f.IntVar(&pdfMaxChars, "pdf-max-chars", 400000, "cap on extracted text per PDF injected into the prompt")
	f.BoolVar(&ocrPDFs, "ocr-pdfs", true, "OCR fallback for scanned/image PDFs that have no text layer (rasterize via pdftoppm + tesseract); no-op if tesseract is not installed")
	f.IntVar(&ocrMaxPages, "ocr-max-pages", 20, "max pages to OCR per scanned PDF (bounds latency)")
	f.DurationVar(&realtimeIdle, "realtime-idle-timeout", 0, "reap a /v1/realtime ws session after this long with no traffic (default 5m or CORRALLM_REALTIME_IDLE_TIMEOUT; 0 disables)")
	f.DurationVar(&realtimeMaxSession, "realtime-max-session", 0, "hard cap on a /v1/realtime ws session's duration (or CORRALLM_REALTIME_MAX_SESSION; 0 disables)")
	f.DurationVar(&reservationMaxTTL, "reservation-max-ttl", 0, "cap on a /v1/reservations slot lease before it must be renewed (default 5m or CORRALLM_RESERVATION_MAX_TTL)")
	f.StringVar(&tuneCachePath, "tune-cache", "", "path to the VRAM slot auto-tuner's profile cache (default <db-dir>/vram-profile.json or CORRALLM_TUNE_CACHE)")
	f.IntVar(&vramMargin, "vram-margin", 0, "MiB of free VRAM kept back when sizing --parallel from a cached profile (default 512 or CORRALLM_VRAM_MARGIN)")
	return cmd
}

// defaultTuneCachePath places the VRAM auto-tuner's profile cache next to the
// SQLite DB by default (same "home/var" convention) — no extra directory to
// manage or document separately.
func defaultTuneCachePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "vram-profile.json")
}

type serveOpts struct {
	webRoot, configPath, dbPath, addr     string
	healthTimeout                         time.Duration
	tokenPath                             string
	activityRetention                     time.Duration
	requestTimeout                        time.Duration
	capturePayloads, convertPDFs, ocrPDFs bool
	pdfMaxChars, ocrMaxPages              int
	realtimeIdle, realtimeMaxSession      time.Duration
	reservationMaxTTL                     time.Duration
	tuneCachePath                         string
	vramMargin                            int
}

func serve(ctx context.Context, o serveOpts) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	slog.Info("config loaded", "path", o.configPath,
		"servers", len(cfg.Servers), "models", len(cfg.Models), "groups", len(cfg.PriorityGroups))

	st, err := store.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	mgr := proc.NewManager(cfg)
	if o.healthTimeout > 0 {
		mgr.SetHealthTimeout(o.healthTimeout)
		slog.Info("health timeout overridden", "timeout", o.healthTimeout)
	}
	// VRAM slot auto-tuner: a missing/empty cache file is fine (empty cache,
	// introspect stays a no-op until models have measured once); a read/parse
	// error is the only thing that aborts boot, same as a broken YAML config.
	tuneCache, err := tune.New(o.tuneCachePath)
	if err != nil {
		return fmt.Errorf("tune cache: %w", err)
	}
	mgr.SetTuneCache(tuneCache)
	mgr.SetVRAMMargin(o.vramMargin)
	defer mgr.Shutdown()
	// Preload pinned (persistent) models in the background so boot isn't blocked.
	go mgr.Preload(ctx)

	scheduler := sched.NewWithConfig(cfg)
	scheduler.SetMaxReservationTTL(o.reservationMaxTTL)
	h := &api.Handlers{Version: version, Cfg: cfg, Store: st, Mgr: mgr, Sched: scheduler}

	// Admin token gates the management surface (/api/*). Generated into
	// <home>/admin.token on first run; the dashboard's login screen points there.
	adminToken, created, err := auth.LoadOrCreateToken(o.tokenPath)
	if err != nil {
		return err
	}
	if created {
		slog.Info("generated admin token", "path", o.tokenPath)
	} else {
		slog.Info("loaded admin token", "path", o.tokenPath)
	}

	router := chi.NewRouter()
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(auth.Middleware(adminToken)) // gates /api/*; /v1, /upstream, /health, SPA pass through

	// BuildGateway mounts REST + GraphQL (/api/graphql) + schema views onto router.
	if _, err := api.BuildGateway(router, h); err != nil {
		return err
	}

	// Plain liveness probe for load balancers / monitoring (and llama-swap
	// compatibility). Untracked — bypasses the scheduler — and answered directly
	// here so it can't fall through to the SPA catch-all (which would 200 with
	// HTML and mask an unhealthy process). The richer op stays at /api/v1/health.
	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	}
	router.Get("/health", healthz)
	router.Get("/healthz", healthz)

	// Live UI events (SSE): the proxy publishes activity/changed events that the
	// observability views subscribe to instead of polling.
	broker := events.NewBroker()
	router.Get("/api/v1/events", broker.ServeSSE)

	// OpenAI-compatible inference passthrough (raw, streaming — bypasses gat),
	// gated by the fairshare admission scheduler (shared with the lanes read op).
	px := proxy.New(cfg, mgr, scheduler, st)
	px.SetBroker(broker)
	px.SetRequestTimeout(o.requestTimeout)
	px.SetCapturePayloads(o.capturePayloads)
	// Global ingestion config: built-in defaults ← legacy flags ← config `convert:`.
	convertGlobal := config.DefaultConvert().
		Merge(config.ConvertConfig{MaxChars: o.pdfMaxChars, MaxPages: o.ocrMaxPages, OCR: &o.ocrPDFs}).
		Merge(cfg.Convert)
	px.SetConvert(o.convertPDFs, convertGlobal)
	px.SetRealtimeTimeouts(o.realtimeIdle, o.realtimeMaxSession)
	px.Mount(router)

	// The SPA is served for everything not claimed above.
	router.Handle("/*", webui.Handler(o.webRoot))

	srv := &http.Server{
		Addr:              o.addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown: SIGINT/SIGTERM stops the listener and (via the defers)
	// tears down spawned backends — otherwise a kill leaves child processes
	// orphaned (their process groups never get signalled).
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Expire stale slot reservations (a keyed caller can lease headroom for its
	// lane; the lease must be renewed or it auto-frees). Stops on shutdown.
	scheduler.StartReaper(sigCtx)

	// Sample instantaneous per-lane queue depth so it's visible before requests
	// resolve (the activity log is completion-driven). Stops on shutdown.
	go runQueueSampler(sigCtx, scheduler, st, 5*time.Second, o.activityRetention)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("corrallm listening", "addr", o.addr, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-sigCtx.Done():
		slog.Info("shutdown signal received")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// runQueueSampler periodically snapshots the scheduler's per-lane load and
// persists it (sparse — idle lanes are skipped). It also runs periodic
// maintenance: pruning old lane samples (48h) and old activity (activityRetention).
func runQueueSampler(ctx context.Context, sc *sched.Scheduler, st *store.Store, interval, activityRetention time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var sincePrune time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			agg := map[string]*store.LaneSample{}
			for _, b := range sc.Snapshot().Backends {
				for _, g := range b.Groups {
					s := agg[g.Group]
					if s == nil {
						s = &store.LaneSample{Group: g.Group}
						agg[g.Group] = s
					}
					s.Active += g.Active
					s.Waiting += g.Waiting
				}
			}
			if len(agg) > 0 {
				samples := make([]store.LaneSample, 0, len(agg))
				for _, s := range agg {
					samples = append(samples, *s)
				}
				if err := st.InsertLaneSamples(time.Now().UnixMilli(), samples); err != nil {
					slog.Warn("lane sample", "err", err)
				}
			}
			if sincePrune += interval; sincePrune >= 5*time.Minute {
				sincePrune = 0
				if err := st.PruneLaneSamples(time.Now().Add(-48 * time.Hour).UnixMilli()); err != nil {
					slog.Warn("prune lane samples", "err", err)
				}
				if activityRetention > 0 {
					if n, err := st.PruneActivity(time.Now().Add(-activityRetention).UnixMilli()); err != nil {
						slog.Warn("prune activity", "err", err)
					} else if n > 0 {
						slog.Info("pruned activity", "rows", n, "retention", activityRetention)
					}
				}
			}
		}
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func pick(flagVal, def string) string {
	if flagVal != "" {
		return flagVal
	}
	return def
}

// pickDuration prefers a non-zero flag value, else the env-derived default.
func pickDuration(flagVal, def time.Duration) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	return def
}

// envDuration parses a duration env var (e.g. "600s"), falling back to def.
func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// pickInt prefers a positive flag value, else the env-derived default.
func pickInt(flagVal, def int) int {
	if flagVal > 0 {
		return flagVal
	}
	return def
}

// envInt parses an int env var, falling back to def.
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
