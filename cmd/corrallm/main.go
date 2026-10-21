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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/agentdist"
	"github.com/iodesystems/corrallm/internal/alert"
	"github.com/iodesystems/corrallm/internal/api"
	"github.com/iodesystems/corrallm/internal/auth"
	"github.com/iodesystems/corrallm/internal/bench/task"
	"github.com/iodesystems/corrallm/internal/billing"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/configdb"
	"github.com/iodesystems/corrallm/internal/events"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/handoff"
	"github.com/iodesystems/corrallm/internal/metrics"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"github.com/iodesystems/corrallm/internal/toolchain"
	"github.com/iodesystems/corrallm/internal/tune"
	"github.com/iodesystems/corrallm/internal/webui"
	"github.com/iodesystems/corrallm/ui"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		// Started by a handoff and failing: the predecessor keeps serving and
		// reports this to whoever asked for the upgrade.
		handoff.ReportStartFailure(err)
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
		// EVERY subcommand, not just serve. The warning was written for the
		// daemon — "restarting will not pick up your changes" — and the CLI
		// turned out to be the more dangerous half: a stale `corrallm config
		// export` silently DROPS fields the running daemon understands, because
		// decoding a stored model into an older struct discards keys it has
		// never heard of. That produces a confident, wrong answer rather than a
		// missing one, and it is the shape of answer nobody double-checks.
		//
		// Seen on 2026-09-06: a per-model setting the daemon had stored, was
		// serving from, and kept across a restart was absent from three
		// consecutive exports. The store was right the whole time; the tool
		// used to check it was a build old enough to predate the field.
		//
		// Goes to stderr with the rest of slog, so `config export > file` is
		// unaffected.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { warnIfStaleFor(cmd.Name()) },
	}
	root.AddCommand(newServeCmd(), newAgentCmd(), newConfigCmd(), newDumpGraphQLCmd(), newVersionCmd(), newIntrospectCmd(), newValidateCmd(), newFeaturesCmd(), newServiceCmd(), newToolsCmd(), newAuthCmd())
	return root
}

// validate parses and validates a config WITHOUT starting anything — no port
// bound, no backend spawned, no DB opened.
//
// It exists because `serve` frees the listen port before it validates, so a
// config error took the gateway down instead of failing safe: on 2026-07-26 a
// lane referencing a deleted model exited at startup with :8111 already
// released, and the edge served 503 until someone noticed. A launcher can now
// check first and keep the running instance.
func newValidateCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the config; exit non-zero if it would not start",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Validate what the daemon will actually READ. `service restart`
			// passes the --config recorded in the unit, which on a migrated
			// machine is the retired zero-byte config.yml — so this printed
			// "ok — 0 models, 0 lanes" for every restart and the guard that
			// exists because of the 2026-07-26 outage was gating nothing.
			c, src, err := liveConfig(cfgPath)
			if err != nil {
				return err
			}
			models := c.AllModels()
			ext := 0
			for range c.Extensions {
				ext++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s — %d models, %d lanes, %d extensions\n",
				describeConfigSource(src), len(models), len(c.Lanes), ext)
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "corrallm.yaml", "config file to validate; ignored when it is empty or absent, in which case the database the daemon boots from is validated instead")
	return cmd
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

// sdlHeader marks the dumped snapshot as generated, in the phrasing editors and
// linters recognise ("Code generated ... DO NOT EDIT."). Its descriptions come
// from the handlers' `doc:` struct tags, so a hand edit is lost on the next
// ./bin/gen — change the Go side instead. A `#` comment is valid SDL; only the
// file carries it, not the schema served at /api/schema/graphql.
const sdlHeader = "# Code generated by `corrallm dump-graphql` (./bin/gen) from the Go handlers. DO NOT EDIT.\n" +
	"# Descriptions come from the handlers' `doc:` struct tags; change those and regenerate.\n\n"

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
			if err := os.WriteFile(args[0], []byte(sdlHeader+g.GraphQLSDL()), 0o644); err != nil {
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
			p := derivePaths(defaultHome(), configPath, dbPath)
			return introspect(cmd, introspectOpts{
				configPath: p.config,
				tuneCache:  envOr("CORRALLM_TUNE_CACHE", defaultTuneCachePath(p.db)),
				vramMargin: pickInt(vramMargin, envInt("CORRALLM_VRAM_MARGIN", 512)),
				json:       asJSON,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&configPath, "config", "", "path to the corrallm YAML config (default <home>/config.yml or CORRALLM_CONFIG)")
	f.StringVar(&dbPath, "db", "", "path used only to resolve the default tune-cache location, <db-dir>/vram-profile.json (default <home>/var/corrallm.db or CORRALLM_DB); introspect never opens the DB itself")
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
	// GPUs is every card in the box, each with the pool that budgets it. A list
	// because this command's whole job is telling an operator what the machine
	// looks like, and reporting one card on a two-card box is how a wrong
	// assumption survives the check meant to catch it.
	GPUs     []introspectGPU   `json:"gpus,omitempty"`
	GPUError string            `json:"gpu_error,omitempty"` // set (only) when nvidia-smi is unavailable
	Models   []introspectModel `json:"models"`
}

type introspectGPU struct {
	Name      string `json:"name"`
	UUID      string `json:"uuid,omitempty"`
	BusID     string `json:"pci_bus_id,omitempty"`
	Pool      string `json:"pool,omitempty"`   // the pool that budgets this card, if any
	Server    string `json:"server,omitempty"` // and the server that pool belongs to
	TotalMiB  int    `json:"total_mib"`
	UsedMiB   int    `json:"used_mib"`
	FreeMiB   int    `json:"free_mib"`
	BudgetMiB int    `json:"budget_mib"` // post-eviction budget on THIS card
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
	Device        string `json:"device,omitempty"`      // the card these numbers were measured on
	Pool          string `json:"pool,omitempty"`        // and the pool it draws from
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

	devs, gpuErr := gpu.ProbeAll()
	report := introspectReport{}
	if gpuErr != nil {
		report.GPUError = gpuErr.Error()
	}

	// placementOf resolves a model to the card it runs on and the pool that
	// budgets it — the pair every number below has to be computed against.
	//
	// A model naming no device-bound pool falls back to the first card, which
	// is what this command always did and is still right on a single-GPU box.
	// It is only wrong when there are two, which is exactly when `devices:`
	// exists to say so.
	placementOf := func(mc config.Model) (gpu.Stats, string, bool) {
		if len(devs) == 0 {
			return gpu.Stats{}, "", false
		}
		if named := cfg.DevicePoolsNamedBy(mc.Server, mc.Pool, mc.RAMUsage); len(named) == 1 {
			if sel := cfg.DeviceSelectorFor(mc.Server, named[0]); sel != "" {
				if st, err := gpu.Select(devs, sel); err == nil {
					return st, named[0], true
				}
			}
			return devs[0], named[0], true
		}
		return devs[0], cfg.DevicePoolFor(mc.Server), true
	}

	// Post-eviction budget, PER CARD: a loading model gets its own card minus
	// what won't move there (persistent/pinned models placed on it) and the
	// margin. Summing across cards — or charging every pinned model to one —
	// was harmless while a box had one GPU and is a fabrication once it has
	// two. This CLI can't attribute live pre-crowded (non-corrallm) usage
	// without the running server's process list, so it assumes ~0; the serve
	// path subtracts it.
	budgets := make(map[string]int, len(devs))
	for _, d := range devs {
		b := d.TotalMiB - o.vramMargin
		for name, mc := range cfg.Models {
			if !mc.Persistent {
				continue
			}
			st, pool, ok := placementOf(mc)
			if !ok || st.UUID != d.UUID {
				continue
			}
			if prof, ok := cache.Get(st.Name, name); ok && prof.PeakMiB > 0 {
				b -= prof.PeakMiB
			} else if sz, err := config.ParseSize(mc.RAMUsage[pool]); err == nil && sz > 0 {
				b -= int(sz / (1024 * 1024))
			}
		}
		if b < 0 {
			b = 0
		}
		budgets[d.UUID] = b
	}

	// Which pool claims which card, so the operator can see a card that NOTHING
	// budgets — the state a freshly installed GPU is in.
	poolOwner := map[string][2]string{} // uuid → {server, pool}
	for srvName, srv := range cfg.Servers {
		// An agent's cards are usually in another box. Only a UUID — unique
		// across machines — proves a card here is one it budgets (an agent
		// beside this primary); a bus id names a slot every box has.
		match := gpu.Select
		if srv.Agent != nil {
			match = gpu.SelectByUUID
		}
		for pool, sel := range srv.Devices {
			if st, err := match(devs, sel); err == nil {
				poolOwner[st.UUID] = [2]string{srvName, pool}
			}
		}
	}
	for _, d := range devs {
		g := introspectGPU{
			Name: d.Name, UUID: d.UUID, BusID: d.PCIBusID,
			TotalMiB: d.TotalMiB, UsedMiB: d.UsedMiB, FreeMiB: d.FreeMiB,
			BudgetMiB: budgets[d.UUID],
		}
		if o, ok := poolOwner[d.UUID]; ok {
			g.Server, g.Pool = o[0], o[1]
		}
		report.GPUs = append(report.GPUs, g)
	}

	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mc := cfg.Models[name]
		im := introspectModel{Name: name, ConfigSlots: mc.Slots()}
		if st, pool, ok := placementOf(mc); ok {
			im.Device, im.Pool = st.Name, pool
			if p, ok := cache.Get(st.Name, name); ok {
				im.HasProfile = true
				im.BaseMiB, im.PerSlotMiB, im.PeakMiB, im.MeasuredSlots, im.Ctx = p.BaseMiB, p.PerSlotMiB, p.PeakMiB, p.MeasuredSlots, p.Ctx
				if n, ok := cache.SlotsFor(st.Name, name, budgets[st.UUID]); ok {
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
		for _, g := range report.GPUs {
			// The pool is printed even when absent, and said out loud: a card
			// no pool claims is invisible to the scheduler, which is a real and
			// easily-missed state right after one is installed.
			owner := "no pool declares it"
			if g.Pool != "" {
				owner = g.Server + "/" + g.Pool
			}
			fmt.Fprintf(out, "GPU: %s  %s  [%s]\n", g.Name, g.BusID, owner)
			fmt.Fprintf(out, "     total=%dMiB used=%dMiB free=%dMiB  (margin=%dMiB post-eviction budget=%dMiB)\n",
				g.TotalMiB, g.UsedMiB, g.FreeMiB, o.vramMargin, g.BudgetMiB)
			fmt.Fprintf(out, "     %s\n", g.UUID)
		}
		fmt.Fprintln(out)
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
		agentDir, publicBase                       string
		alertNtfy                                  string
		healthTimeout, activityRetention           time.Duration
		payloadRetention                           time.Duration
		requestTimeout                             time.Duration
		capturePayloads, convertPDFs, ocrPDFs      bool
		audioTranscode                             bool
		insecure                                   bool
		pdfMaxChars, ocrMaxPages                   int
		audioMaxSecs                               int
		realtimeIdle, realtimeMaxSession           time.Duration
		reservationMaxTTL                          time.Duration
		tuneCachePath                              string
		slotCacheDir                               string
		slotCacheMaxGB                             int
		vramMargin                                 int
		benchBin, benchConfig, benchProbes         string
		handoffDrain                               time.Duration
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
			p := derivePaths(home, configPath, dbPath)
			dbPathResolved := p.db
			slog.Info("paths resolved", "home", p.home, "config", p.config, "db", p.db)
			return serve(cmd.Context(), serveOpts{
				home:               p.home,
				webRoot:            pick(webRoot, envOr("WEB_ROOT", "./ui/dist")),
				agentDir:           pick(agentDir, envOr("CORRALLM_AGENT_DIR", "./bin/agents")),
				publicBase:         pick(publicBase, envOr("CORRALLM_PUBLIC_BASE", "")),
				alertNtfy:          pick(alertNtfy, envOr("CORRALLM_ALERT_NTFY", "")),
				configPath:         p.config,
				configDerived:      p.configDerived,
				dbPath:             dbPathResolved,
				addr:               envOr("ADDR", ":6502"),
				healthTimeout:      pickDuration(healthTimeout, envDuration("CORRALLM_HEALTH_TIMEOUT", 0)),
				tokenPath:          p.token,
				insecure:           insecure,
				activityRetention:  pickDuration(activityRetention, envDuration("CORRALLM_ACTIVITY_RETENTION", 30*24*time.Hour)),
				payloadRetention:   pickDuration(payloadRetention, envDuration("CORRALLM_PAYLOAD_RETENTION", 7*24*time.Hour)),
				requestTimeout:     pickDuration(requestTimeout, envDuration("CORRALLM_REQUEST_TIMEOUT", 0)),
				capturePayloads:    capturePayloads,
				convertPDFs:        convertPDFs,
				pdfMaxChars:        pdfMaxChars,
				ocrPDFs:            ocrPDFs,
				ocrMaxPages:        ocrMaxPages,
				audioTranscode:     audioTranscode,
				audioMaxSecs:       audioMaxSecs,
				realtimeIdle:       pickDuration(realtimeIdle, envDuration("CORRALLM_REALTIME_IDLE_TIMEOUT", 5*time.Minute)),
				realtimeMaxSession: pickDuration(realtimeMaxSession, envDuration("CORRALLM_REALTIME_MAX_SESSION", 0)),
				reservationMaxTTL:  pickDuration(reservationMaxTTL, envDuration("CORRALLM_RESERVATION_MAX_TTL", 5*time.Minute)),
				tuneCachePath:      pick(tuneCachePath, envOr("CORRALLM_TUNE_CACHE", defaultTuneCachePath(dbPathResolved))),
				slotCacheDir:       pick(slotCacheDir, envOr("CORRALLM_SLOT_CACHE_DIR", "")),
				slotCacheMaxGB:     pickInt(slotCacheMaxGB, envInt("CORRALLM_SLOT_CACHE_MAX_GB", 20)),
				vramMargin:         pickInt(vramMargin, envInt("CORRALLM_VRAM_MARGIN", 512)),
				benchBin:           benchBin,
				benchConfig:        pick(benchConfig, defaultBenchConfig(p.home)),
				benchProbes:        benchProbes,
				handoffDrain:       pickDuration(handoffDrain, envDuration("CORRALLM_HANDOFF_DRAIN", defaultHandoffDrain)),
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&home, "home", defaultHome(), "directory corrallm roots its own state in — config, SQLite store, admin token, layered .properties (or CORRALLM_HOME)")
	f.StringVar(&service, "service", envOr("CORRALLM_SLOT", "dev"), "service/slot selecting override .properties (dev|current|next)")
	f.StringVar(&webRoot, "web-root", "", "directory to serve the SPA from (default ./ui/dist or WEB_ROOT)")
	f.StringVar(&alertNtfy, "alert-ntfy", "", "ntfy topic URL for operator alerts, e.g. https://ntfy.sh/<topic> (or CORRALLM_ALERT_NTFY): a provider refusing requests, a spend cap reached; empty = none")
	f.StringVar(&publicBase, "public-base", "", "how attached machines reach this daemon, e.g. http://box1:8111 (or CORRALLM_PUBLIC_BASE); used in the install command")
	f.StringVar(&agentDir, "agent-dir", "", "directory of cross-compiled agent binaries to serve at /install/ (default ./bin/agents or CORRALLM_AGENT_DIR; populate with `make agents`)")
	f.StringVar(&configPath, "config", "", "path to the corrallm YAML config (default <home>/config.yml or CORRALLM_CONFIG)")
	f.StringVar(&dbPath, "db", "", "path to the SQLite database (default <home>/var/corrallm.db or CORRALLM_DB)")
	f.DurationVar(&healthTimeout, "health-timeout", 0, "max time a cold backend spawn may take to become healthy (default 120s or CORRALLM_HEALTH_TIMEOUT); raise for large models")
	f.DurationVar(&activityRetention, "activity-retention", 0, "delete activity-log rows older than this (default 720h/30d or CORRALLM_ACTIVITY_RETENTION; 0 disables)")
	f.DurationVar(&payloadRetention, "payload-retention", 0, "clear captured request/response bodies on activity rows older than this, KEEPING the row and every metric on it (default 168h/7d or CORRALLM_PAYLOAD_RETENTION; 0 disables). Payloads are read only to replay a request in the console; metrics are read for as long as they are retained.")
	f.DurationVar(&requestTimeout, "request-timeout", 0, "max wall-clock for one proxied request before corrallm cancels it (or CORRALLM_REQUEST_TIMEOUT; 0 = no corrallm deadline, defer to client + backend)")
	f.BoolVar(&insecure, "insecure", envBool("CORRALLM_INSECURE"), "serve the management API and dashboard with NO admin token — anyone who can reach the port is an operator. For a trusted single-user box or a first look; never on a shared or reachable network.")
	f.BoolVar(&capturePayloads, "capture-payloads", true, "capture per-request request/response payloads onto the activity log (capped; binary audio summarized; cleared at --payload-retention, and dropped with the row at --activity-retention)")
	f.BoolVar(&convertPDFs, "convert-pdfs", true, "auto-extract PDF attachments in chat requests into injected text (via pdftotext) so text models can read them")
	f.IntVar(&pdfMaxChars, "pdf-max-chars", 400000, "cap on extracted text per PDF injected into the prompt")
	f.BoolVar(&ocrPDFs, "ocr-pdfs", true, "OCR fallback for scanned/image PDFs that have no text layer (rasterize via pdftoppm + tesseract); no-op if tesseract is not installed")
	f.IntVar(&ocrMaxPages, "ocr-max-pages", 20, "max pages to OCR per scanned PDF (bounds latency)")
	f.BoolVar(&audioTranscode, "transcode-audio", true, "convert an STT upload (mp3/m4a/webm/flac/…) into 16 kHz mono wav when the backend has declared which audio formats it reads (via ffmpeg). A no-op for a backend that declares none, which is every backend until an operator says otherwise.")
	f.IntVar(&audioMaxSecs, "transcode-audio-max-seconds", 3600, "longest audio corrallm will convert in one request; longer uploads are refused with 413 rather than truncated. 3600s is 115.2 MB of 16 kHz mono wav, held in memory for the length of the request. The old 1800s default refused real 42-minute court hearings, and its stated reason was wrong: the 64 MiB body cap bounds what ARRIVES (compressed) and never sees what this PRODUCES.")
	f.DurationVar(&realtimeIdle, "realtime-idle-timeout", 0, "reap a /v1/realtime ws session after this long with no traffic (default 5m or CORRALLM_REALTIME_IDLE_TIMEOUT; 0 disables)")
	f.DurationVar(&realtimeMaxSession, "realtime-max-session", 0, "hard cap on a /v1/realtime ws session's duration (or CORRALLM_REALTIME_MAX_SESSION; 0 disables)")
	f.DurationVar(&reservationMaxTTL, "reservation-max-ttl", 0, "cap on a /v1/reservations slot lease before it must be renewed (default 5m or CORRALLM_RESERVATION_MAX_TTL)")
	f.StringVar(&tuneCachePath, "tune-cache", "", "path to the VRAM slot auto-tuner's profile cache (default <db-dir>/vram-profile.json or CORRALLM_TUNE_CACHE)")
	f.StringVar(&slotCacheDir, "slot-cache-dir", "", "keep each conversation's KV cache here so a backend with one slot can serve several without reprocessing them (or CORRALLM_SLOT_CACHE_DIR). OFF unless set. The backend must also be started with llama.cpp's --slot-save-path pointing at the SAME directory. Costs ~37 KB per prompt token on disk and saves ~10s of reprocessing per conversation switch.")
	f.IntVar(&slotCacheMaxGB, "slot-cache-max-gb", 0, "cap the slot cache at this many GB, oldest evicted first (default 20 or CORRALLM_SLOT_CACHE_MAX_GB)")
	f.IntVar(&vramMargin, "vram-margin", 0, "MiB of free VRAM kept back when sizing --parallel from a cached profile (default 512 or CORRALLM_VRAM_MARGIN)")
	f.StringVar(&benchBin, "bench-bin", envOr("CORRALLM_BENCH_BIN", "llm-bench"), "llm-bench binary spawned by UI-driven bench runs (same binary you run from a shell)")
	f.StringVar(&benchConfig, "bench-config", envOr("CORRALLM_BENCH_CONFIG", ""), "llm-bench config passed to spawned runs (default: llm-bench's own default)")
	f.StringVar(&benchProbes, "bench-probes", envOr("CORRALLM_BENCH_PROBES", ""), "comma-separated probe directories passed to spawned runs; overrides the config's probeDirs (default: llm-bench's own default)")
	f.DurationVar(&handoffDrain, "handoff-drain", 0, "on a handoff (`corrallm service upgrade`), how long the old process may take to finish its in-flight requests before it exits and cuts the rest (default 10m or CORRALLM_HANDOFF_DRAIN)")
	return cmd
}

// defaultBenchConfig points spawned bench runs at <home>/llm-bench.yaml when it
// exists.
//
// The bench config is per-BOX measurement state — which models to probe on this
// machine, at what concurrency — so it belongs with the rest of corrallm's own
// state rather than in whatever repo happened to hold the launcher. It lived in
// a neighbouring checkout and had to be named by absolute path on every start;
// anyone adopting corrallm inherited a flag pointing at a directory they do not
// have.
//
// Only when the file is there: an absent one keeps llm-bench's own default,
// which is what a box that never benches wants.
func defaultBenchConfig(home string) string {
	if home == "" {
		return ""
	}
	p := filepath.Join(home, "llm-bench.yaml")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// defaultTuneCachePath places the VRAM auto-tuner's profile cache next to the
// SQLite DB by default (same "home/var" convention) — no extra directory to
// manage or document separately.
func defaultTuneCachePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "vram-profile.json")
}

type serveOpts struct {
	// home is corrallm's home directory. Carried so a MANAGED tool's default
	// install prefix (<home>/tools/<name>) resolves the same way the CLI
	// resolves it.
	home                                  string
	webRoot, configPath, dbPath, addr     string
	configDerived                         bool
	agentDir, publicBase                  string
	alertNtfy                             string
	healthTimeout                         time.Duration
	tokenPath                             string
	insecure                              bool
	activityRetention                     time.Duration
	payloadRetention                      time.Duration
	requestTimeout                        time.Duration
	capturePayloads, convertPDFs, ocrPDFs bool
	audioTranscode                        bool
	pdfMaxChars, ocrMaxPages              int
	audioMaxSecs                          int
	realtimeIdle, realtimeMaxSession      time.Duration
	reservationMaxTTL                     time.Duration
	tuneCachePath                         string
	slotCacheDir                          string
	slotCacheMaxGB                        int
	vramMargin                            int
	benchBin, benchConfig, benchProbes    string
	handoffDrain                          time.Duration
}

// pauseStore adapts *store.Store to proc.PauseStore. The manager keeps the
// interface narrow so it never imports the store package (same reason
// quota.CounterStore exists); this is the one place the two meet.
type pauseStore struct{ st *store.Store }

func (p pauseStore) SavePause(target, reason string, atMS, resumeAtMS int64) error {
	return p.st.SavePause(target, reason, atMS, resumeAtMS)
}

func (p pauseStore) DeletePause(target string) error { return p.st.DeletePause(target) }

func (p pauseStore) LoadPauses() ([]proc.PersistedPause, error) {
	rows, err := p.st.LoadPauses()
	if err != nil {
		return nil, err
	}
	out := make([]proc.PersistedPause, len(rows))
	for i, r := range rows {
		out[i] = proc.PersistedPause{Target: r.Target, Reason: r.Reason, AtMS: r.AtMS, ResumeAtMS: r.ResumeAtMS}
	}
	return out, nil
}

func serve(ctx context.Context, o serveOpts) error {
	// A process started by a predecessor's handoff inherits the listener and
	// the control socket; anything else gets nil and listens itself.
	succ, err := handoff.Inherit()
	if err != nil {
		return err
	}
	if succ != nil {
		slog.Info("taking over from a running corrallm", "predecessor", succ.PredecessorPID)
	}
	// Before the config: ${...} references in it resolve through this store, so
	// it has to be installed first or a credential-backed provider silently
	// resolves to an empty header on the very first load.
	if err := config.LoadCredentials(filepath.Dir(o.configPath)); err != nil {
		return err
	}
	// The store opens BEFORE the config, because the config lives in it now.
	st, err := store.Open(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// Requests the last process died serving left a marker and no row. Only a
	// DEAD writer's: in a handoff the predecessor is still serving its own.
	st.SetOwner(selfIdentity())
	if n, err := st.SweepInflight(processAlive); err != nil {
		slog.Warn("sweep in-flight requests", "err", err)
	} else if n > 0 {
		slog.Warn("requests cut by the last stop, recorded", "count", n)
	}

	if err := configdb.Apply(ctx, st.DB()); err != nil {
		return err
	}
	if n, err := configdb.PruneRevisions(ctx, st.DB(), 100); err == nil && n > 0 {
		slog.Info("pruned old config revisions", "removed", n)
	}
	cfgSource := &configdb.Source{DB: st.DB()}
	cfg, err := loadConfig(ctx, cfgSource, o.configPath)
	if err != nil {
		return err
	}
	slog.Info("config loaded", "from", "database", "db", o.dbPath,
		"servers", len(cfg.Servers), "models", len(cfg.Models), "groups", len(cfg.PriorityGroups))

	// The models chosen off provider directories, and where they were placed.
	// Loaded before the first refresh: a selection is the only record that a
	// hand-chosen model exists, so a gap here is a gap in what corrallm serves.
	if st != nil {
		if rows, err := st.LoadSelections(); err != nil {
			slog.Warn("load selections", "err", err)
		} else if len(rows) > 0 {
			api.InstallSelections(cfg, rows)
			slog.Info("model selections loaded", "count", len(rows))
		}
	}

	mgr := proc.NewManager(cfg)
	// Why each backend came up, kept where somebody can read it later. The
	// activity log has always carried load_ms — the time a request waited on a
	// spawn — and never what caused the spawn or what it displaced.
	mgr.SetLoadRecorder(loadRecorder{st})
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
	// Probed capabilities become the answer to "what does this model accept",
	// ahead of anything declared. The catalog is what makes this matter: bench
	// reads modalities from /v1/models, so this is what stops a vision-capable
	// model being skipped for vision probes because nobody wrote it down.
	mgr.InstallProbedModalities()
	mgr.SetVRAMMargin(o.vramMargin)
	defer mgr.Shutdown()
	// Restore operator pauses BEFORE preload: a paused pinned model must not
	// warm itself back up just because corrallm restarted.
	mgr.UsePauseStore(pauseStore{st: st})
	// Preload pinned (persistent) models in the background so boot isn't blocked.
	go mgr.Preload(ctx)

	scheduler := sched.NewWithConfig(cfg)
	scheduler.SetMaxReservationTTL(o.reservationMaxTTL)
	if succ != nil {
		// Until the predecessor reports what it holds: no admission (its
		// slots on each backend are unknown), and no local spawn (its
		// children may still hold the GPU). See watchPredecessor.
		scheduler.HoldAdmission()
		mgr.HoldLocalSpawns()
	}
	// Shared between the heartbeat endpoint (writes) and the manager (reads):
	// one view of which agent-backed servers are reporting in.
	liveness := agent.NewLiveness()
	mgr.SetLiveness(liveness)
	// ${tool:x} in a model's cmd resolves through the toolchain registry, on the
	// host the model runs on, at spawn time. Wired as a function so proc keeps
	// no dependency on the toolchain package.
	//
	// Cfg reads the CURRENT config (mgr.SetConfig swaps it on reload), so a tool
	// added or repointed by a config reload is picked up without a restart.
	toolReg := &toolchain.Registry{
		Home: o.home,
		Cfg:  func() *config.Config { return mgr.Config() },
		RunnerFor: func(server string) (toolchain.Runner, error) {
			c := mgr.Config()
			srv, ok := c.Servers[server]
			if !ok {
				return nil, fmt.Errorf("no server %q in config", server)
			}
			if srv.Agent != nil {
				return agent.NewToolRunner(
					agent.NewRemoteHost(server, srv.Agent.Endpoints, srv.Agent.ExpandedToken())), nil
			}
			return &toolchain.Local{Server: server}, nil
		},
	}
	mgr.ExpandCmd = toolReg.ExpandTools
	// A finished build is checked by trialling a model on it, and bin/ goes back
	// to the previous build when that fails.
	toolBuilds := &toolchain.Builder{Reg: toolReg, Check: mgr.CheckTool, Bench: mgr.BenchTool}
	// The scheduled drift check. Until this existed a pin only rotted in public:
	// llama.cpp ships several builds a day and "how far behind are we" was
	// answered solely by somebody opening the Tooling panel. Checking is on by
	// default (one `git ls-remote` per tool); BUILDING is opt-in per tool, since
	// it is minutes of pegged GPU that replaces the binary resident models run on.
	toolWatch := &toolchain.Watcher{Reg: toolReg, Builder: toolBuilds}
	if st != nil {
		toolBuilds.History = api.BuildHistoryStore{S: st}
		// A build is a child of this process, so a restart kills it. A row left
		// saying "running" would be a lie that never resolves and a spinner in
		// the UI for a build that died days ago.
		if n, err := st.InterruptStaleToolBuilds(context.Background()); err != nil {
			slog.Warn("could not settle interrupted tool builds", "err", err)
		} else if n > 0 {
			slog.Warn("marked tool builds interrupted by a restart", "count", n)
		}
		if n, err := st.PruneToolBuilds(context.Background(), 100); err == nil && n > 0 {
			slog.Info("pruned old tool build records", "removed", n)
		}
	}

	agentDist := &agentdist.Handler{Dir: o.agentDir, Version: version}
	h := &api.Handlers{Version: version, Cfg: cfg, Store: st, Mgr: mgr, Sched: scheduler, Tools: toolReg, Builds: toolBuilds,
		Liveness: liveness, AgentDist: agentDist, Verified: api.NewVerifiedStore(),
		ConfigPath: o.configPath, PublicBase: o.publicBase,
		ConfigSource: cfgSource,
		// The note is built per-edit now, not fixed here. It used to read "edited
		// through the dashboard" for every change ever made, which tells an
		// operator that SOMETHING changed and nothing about whether it was
		// theirs — the question they open the page with. One shared admin token
		// means corrallm cannot know a person; it records what changed and the
		// address it came from, which is enough to recognise your own machine.
		UpdateConfig: func(c context.Context, note string, fn func(*config.Config) error) error {
			if note == "" {
				note = "configuration changed"
			}
			_, err := cfgSource.WithNote(note).Update(c, fn)
			return err
		},
	}

	// Admin token gates the management surface (/api/*). Generated into
	// <home>/admin.token on first run; the dashboard's login screen points there.
	//
	// --insecure removes that gate entirely. It exists because the first thing
	// someone evaluating corrallm wants is to SEE it, and "find a token file on
	// the server" is a real wall in front of that — one people route around in
	// worse ways than a documented flag. The trade is stated plainly rather than
	// hidden: with no gate, anyone who can reach the port can unload models,
	// rewrite config, and start bench runs.
	//
	// The token is still created and loaded, so turning the flag off restores
	// the same credential rather than minting a new one and invalidating every
	// client that had it.
	adminToken, created, err := auth.LoadOrCreateToken(o.tokenPath)
	if err != nil {
		return err
	}
	if created {
		slog.Info("generated admin token", "path", o.tokenPath)
	} else {
		slog.Info("loaded admin token", "path", o.tokenPath)
	}
	// SSO sign-in (auth.oidc). The signing key sits beside the admin token and
	// is made the same way; deleting it signs every session out.
	sessionKeyPath := filepath.Join(filepath.Dir(o.tokenPath), "session.key")
	sessionKey, _, err := auth.LoadOrCreateToken(sessionKeyPath)
	if err != nil {
		return err
	}
	sso := &auth.SSO{Key: []byte(sessionKey), Settings: func() *auth.OIDCSettings {
		return oidcSettings(mgr.Config(), o.publicBase)
	}, AdminToken: adminToken,
		TokenEnabled: func() bool {
			c := mgr.Config()
			return c == nil || c.Auth.AdminTokenOn()
		},
		// Per-user revocation and last sign-in live in the store, not config.
		NotBefore:    st.AuthNotBefore,
		RecordSignIn: st.AuthRecordSignIn,
	}
	if st := sso.Settings(); st != nil {
		slog.Info("sso enabled", "issuer", st.Issuer, "redirect", st.RedirectURL, "users", len(st.Users))
		if st.RedirectURL == "" {
			slog.Warn("sso: no callback URL — set auth.oidc.redirectUrl or --public-base; /auth/login will refuse until then")
		}
	}
	if o.insecure {
		// WARN, and louder when the address is one other machines can reach.
		// "Insecure on localhost" is a defensible choice on a single-user box;
		// "insecure on 0.0.0.0" is an unauthenticated control plane on the
		// network, and the log is the only place that distinction gets made.
		if exposedAddr(o.addr) {
			slog.Warn("INSECURE MODE ON A NON-LOOPBACK ADDRESS — the management API and dashboard "+
				"are open to anyone who can reach this port: unload models, rewrite config, run benches. "+
				"Bind to 127.0.0.1 or drop --insecure",
				"addr", o.addr)
		} else {
			slog.Warn("insecure mode: no admin token required for /api/* — anyone who can reach this port is an operator",
				"addr", o.addr)
		}
	}

	router := chi.NewRouter()
	// Which process answered. Across a handoff two serve the same port, and
	// this is how a client — or the operator reading a trace — tells them apart.
	pidStr := strconv.Itoa(os.Getpid())
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(proxy.PidHeader, pidStr)
			next.ServeHTTP(w, r)
		})
	})
	// Ahead of RealIP, which OVERWRITES r.RemoteAddr with a client-supplied
	// header. Anything making a trust decision needs the address the connection
	// actually came from, not one the caller can set.
	router.Use(captureConnAddr)
	router.Use(middleware.RealIP)
	// After RealIP so a config revision records the caller's address rather than
	// the front proxy's: a revision that says only "edited through the dashboard"
	// cannot answer "was it me?", which is the question that page exists for.
	router.Use(api.SourceMiddleware)
	router.Use(middleware.Recoverer)
	if !o.insecure {
		router.Use(auth.Middleware(adminToken, sso)) // gates /api/*; /v1, /upstream, /health, SPA pass through
	}
	// Public: these are how a person without a credential gets one.
	router.Get("/auth/login", sso.Login)
	router.Get("/auth/callback", sso.Callback)
	router.Post("/auth/logout", sso.Logout)
	router.Get("/auth/whoami", sso.WhoAmI)

	// BuildGateway mounts REST + GraphQL (/api/graphql) + schema views onto router.
	if _, err := api.BuildGateway(router, h); err != nil {
		return err
	}

	// Plain liveness probe for load balancers / monitoring (and llama-swap
	// compatibility). Untracked — bypasses the scheduler — and answered directly
	// here so it can't fall through to the SPA catch-all (which would 200 with
	// HTML and mask an unhealthy process). The richer op stays at /api/v1/health.
	//
	// It also tells a LOCAL caller where the admin token lives. The login screen
	// needs that to be useful, and it cannot sit behind /api — the whole point is
	// that the operator has no token yet. It is a path, not a credential, but it
	// names the server's home directory, so it is withheld from anyone off-box:
	// whoever installs corrallm has shell access anyway, and a remote visitor to
	// the fronted dashboard has no business reading server paths.
	healthz := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// `insecure` is advertised to EVERYONE, not just a local caller. It is
		// not a secret — it is observable by making one unauthenticated /api
		// call — and the dashboard needs it to know not to render a login for a
		// gate that is not there. tokenPath stays local-only: it is a path, and
		// it names the server's home directory.
		if localCaller(r) {
			fmt.Fprintf(w, `{"status":"ok","version":%q,"tokenPath":%q,"insecure":%t}`, version, o.tokenPath, o.insecure)
			return
		}
		fmt.Fprintf(w, `{"status":"ok","version":%q,"insecure":%t}`, version, o.insecure)
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
	if succ != nil {
		// Forwards from the predecessor carry its time already waited; this
		// is what lets them be believed.
		px.SetPredecessorToken(succ.Token)
	}
	// Wired after construction: the proxy is built later than the handlers, and
	// the admin API reads its roster / quota / in-flight snapshots.
	h.Proxy = px
	// llm-bench is spawned as the SAME binary a human runs from a shell, so a
	// UI-started run is reproducible by copying its logged invocation.
	h.Bench = api.NewBenchRunner()
	h.BenchBin = o.benchBin
	h.BenchConfig = o.benchConfig
	h.BenchProbeDirs = task.SplitProbeDirs(o.benchProbes)
	px.SetBroker(broker)
	alertNotifier := alert.New(o.alertNtfy)
	px.SetAlerter(alertNotifier)
	// corrallm's own callers pass credential allowlists: the running bench and
	// an admin's dashboard (Test tab). Not under --insecure, where every
	// browser would count as an admin.
	bench := h.Bench
	px.SetTrustedCaller(func(r *http.Request, key string) bool {
		return bench.IsRunKey(key) || (!o.insecure && auth.DashboardAdmin(r, adminToken, sso))
	})
	// The running bench also heads each model's admission queue: queue time
	// behind other callers would otherwise be measured as model time.
	px.SetPriorityCaller(bench.IsRunKey)
	px.SetRequestTimeout(o.requestTimeout)
	px.SetCapturePayloads(o.capturePayloads)
	px.SetSlotCache(o.slotCacheDir, int64(o.slotCacheMaxGB)<<30)
	// Global ingestion config: built-in defaults ← legacy flags ← config `convert:`.
	audioSwitch := "on"
	if !o.audioTranscode {
		audioSwitch = "off"
	}
	convertGlobal := config.DefaultConvert().
		Merge(config.ConvertConfig{MaxChars: o.pdfMaxChars, MaxPages: o.ocrMaxPages, OCR: &o.ocrPDFs,
			Audio: audioSwitch, AudioMaxSeconds: o.audioMaxSecs}).
		Merge(cfg.Convert)
	px.SetConvert(o.convertPDFs, convertGlobal)
	px.SetRealtimeTimeouts(o.realtimeIdle, o.realtimeMaxSession)
	px.Mount(router)

	// Prometheus exposition — registered ahead of the SPA catch-all so /metrics
	// is scraped rather than served the web-UI shell (chi matches the specific
	// route before the "/*" wildcard).
	router.Handle("/metrics", metrics.Handler())

	// Agent distribution. PUBLIC on purpose, like /v1 and /health: an
	// unenrolled machine has to be able to fetch the installer and the binary.
	// Neither is a credential — the binary is what `make agents` builds from
	// this repo and does nothing until given a token — so the gate is the
	// enrollment token the installer is handed, not the download.
	dist := agentDist
	dist.Mount(router.Get, func(r *http.Request) string {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	})

	// The SPA is served for everything not claimed above.
	router.Handle("/*", webui.Handler(o.webRoot, ui.DistFS()))

	// Counted so a handoff drain knows when the last request has finished
	// (see handoff.StopAccepting for why that is not Shutdown's job).
	conns := &handoff.Conns{}
	srv := &http.Server{
		Addr:              o.addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ConnState:         conns.Hook,
	}
	// Live-event streams never finish on their own; end them when shutdown
	// starts so the drain waits only for real requests.
	srv.RegisterOnShutdown(broker.Close)

	// Graceful shutdown: SIGINT/SIGTERM stops the listener and (via the defers)
	// tears down spawned backends — otherwise a kill leaves child processes
	// orphaned (their process groups never get signalled).
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Everything this process does on its own schedule — tool watches, sweeps,
	// samplers, pruning, billing — runs under jobsCtx, which a handoff cancels:
	// the draining process finishes requests and nothing else.
	jobsCtx, stopJobs := context.WithCancel(sigCtx)
	defer stopJobs()

	// Enrollment writes config; reloading makes the new server usable without a
	// restart. Set here rather than at construction because it closes over the
	// proxy, which does not exist yet at that point.
	h.Reload = func() error { return reloadInto(ctx, cfgSource, st, mgr, scheduler, px, h) }

	// SIGHUP re-reads the config without dropping the listener or the resident
	// backends. Config and keys were boot-time-only before this, so every edit
	// cost a restart — which on this box means evicting a 27B and paying a cold
	// load, and (until agentkit learned to retry transport errors) failing every
	// in-flight client request.
	go watchReload(sigCtx, cfgSource, st, mgr, scheduler, px, h)

	// Drift checks run on each tool's own cadence. Reads config per pass, so a
	// reload that changes `check:`/`rebuild:` takes effect without a restart.
	go toolWatch.Run(jobsCtx)

	// Expire stale slot reservations (a keyed caller can lease headroom for its
	// lane; the lease must be renewed or it auto-frees). Stops on shutdown.
	scheduler.StartReaper(jobsCtx)

	// Lift timed pauses when they come due. Every read expires a pause lazily,
	// so this is not what makes a resume correct — it is what makes one happen
	// for a pinned model, which nothing ever requests by name.
	mgr.StartPauseSweeper(jobsCtx)
	mgr.StartIdleSweeper(jobsCtx)
	// Reload models whose weights sat too long on memory without ECC. A
	// bench run measuring the model holds it off until the run ends.
	mgr.SetRefreshHold(func(model string) string {
		if h.Bench.Benching(model) {
			return "a bench run is measuring it"
		}
		return ""
	})
	mgr.StartNonECCRefresh(jobsCtx)

	// A payload retention at or past the row retention can never fire: the row
	// is deleted first, taking its payload with it. Silently doing nothing is
	// the wrong answer for a setting an operator deliberately typed.
	if o.payloadRetention > 0 && o.activityRetention > 0 && o.payloadRetention >= o.activityRetention {
		slog.Warn("payload-retention is not shorter than activity-retention, so it will never clear anything — "+
			"rows are deleted at activity-retention and take their payloads with them",
			"payloadRetention", o.payloadRetention, "activityRetention", o.activityRetention)
	}

	// Sample instantaneous per-lane queue depth so it's visible before requests
	// resolve (the activity log is completion-driven). Stops on shutdown.
	go runQueueSampler(jobsCtx, scheduler, st, 5*time.Second, o.activityRetention, o.payloadRetention)
	// Conversation rows for activity recorded before activity_conversation
	// existed, from the captured payloads still on disk. Resumes on restart and
	// is a no-op once caught up.
	go st.BackfillConversations(jobsCtx)

	// Publish per-model residency to Prometheus (corrallm_model_loaded +
	// load-timestamp → uptime). Sampled rather than event-driven so the gauges
	// self-heal after any missed load/evict signal.
	go runResidencySampler(jobsCtx, mgr, 10*time.Second)

	// Periodically refresh each opted-in provider's free-model roster (P16e), so a
	// free model that churns out (goes paid or is removed) is skipped proactively.
	// Only started when a backend opts in via freeTier.refresh.
	if px.HasRosterRefresh() {
		go runRosterRefresh(jobsCtx, px, 30*time.Minute)
	}

	// Reconcile metered spend against what each provider billed (Fireworks:
	// recognised by host). The first pass waits a minute so startup is not
	// competing with a billing call; then hourly, the cadence providers settle
	// their hours at. CORRALLM_BILLING_RECONCILE=off disables it.
	if !strings.EqualFold(envOr("CORRALLM_BILLING_RECONCILE", "on"), "off") {
		rec := billing.New(st, px.Config, px, alertNotifier)
		go rec.Run(jobsCtx, time.Minute, time.Hour)
	}

	ln := net.Listener(nil)
	if succ != nil {
		ln = succ.Listener
	} else if ln, err = net.Listen("tcp", o.addr); err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("corrallm listening", "addr", ln.Addr().String(), "version", version, "pid", os.Getpid())
		errCh <- srv.Serve(ln)
	}()

	if succ != nil {
		// Tell systemd this process is the service now, BEFORE the predecessor
		// is told to drain: its exit must not read as the unit stopping.
		if _, err := handoff.Notify(fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid())); err != nil {
			slog.Warn("could not tell systemd the main pid moved", "err", err)
		}
		go primeAgents(ctx, mgr)
		if err := succ.Ready(); err != nil {
			slog.Error("could not tell the predecessor this process is ready", "err", err)
		}
		go watchPredecessor(succ, scheduler, mgr, px, st)
	} else if _, err := handoff.Notify("READY=1"); err != nil {
		slog.Warn("could not tell systemd the service is ready", "err", err)
	}

	// SIGUSR2 hands this process over to a new one (corrallm service upgrade).
	upgraded := make(chan *handoff.Predecessor, 1)
	go watchUpgrade(sigCtx, ln, upgraded)

	select {
	case err := <-errCh:
		return err
	case pred := <-upgraded:
		drainForSuccessor(pred, srv, ln, conns, broker, scheduler, mgr, px, h, toolBuilds, stopJobs, o.handoffDrain)
		return nil
	case <-sigCtx.Done():
		slog.Info("shutdown signal received")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			// Running out of drain is a fact about the traffic, not a failure of
			// this process, and returning it made every busy restart exit 1 and
			// log `status=1/FAILURE` — a routine reload indistinguishable from a
			// real fault in the journal.
			//
			// The requests are not abandoned by saying so. Shutdown stops the
			// listener and waits; it never cancels a handler. What ends them is
			// the deferred mgr.Shutdown() below, which SIGTERMs every backend the
			// instant this returns — so whatever was still running dies either
			// way, and the only question is whether the exit code lies about it.
			slog.Warn("shutdown drain expired with requests still in flight",
				"drain", shutdownDrain, "err", err)
		}
		return nil
	}
}

// shutdownDrain is how long in-flight requests get to finish on a restart.
//
// Was 10s, against a systemd TimeoutStopSec of 90 — corrallm held itself to a
// ninth of the budget it had. Nothing is paid for the larger number when the
// server is quiet: Shutdown returns the moment the last handler does, which is
// the same reasoning Manager.Shutdown already applies to reaping backends
// ("polled, not slept … only a genuinely stuck backend pays the grace period").
//
// It deliberately does NOT cover a transcription. oidio diarizes at roughly a
// third of realtime, so a 24-minute recording occupies a handler for over five
// minutes, and a drain that waited for one would make every reload take that
// long — on a box seeing 26 restarts in a day. A request that outlives a
// restart is the client's problem to retry, and raglit's stt client now does.
const shutdownDrain = 60 * time.Second

// runRosterRefresh refreshes the free-model roster once at startup and then on
// an interval, until shutdown. The first pass runs after a short delay so a slow
// provider does not hold up serving.
func runRosterRefresh(ctx context.Context, px *proxy.Proxy, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	px.RefreshRoster(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			px.RefreshRoster(ctx)
		}
	}
}

// runQueueSampler periodically snapshots the scheduler's per-lane load and
// persists it (sparse — idle lanes are skipped). It also runs periodic
// maintenance: pruning old lane samples (48h) and old activity (activityRetention).
// runResidencySampler publishes per-model residency (loaded + load timestamp)
// to Prometheus every interval, so corrallm_model_loaded / _load_timestamp track
// which models are warm and for how long (uptime = now - load timestamp).
func runResidencySampler(ctx context.Context, mgr *proc.Manager, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		snap := mgr.Snapshot()
		res := make([]metrics.Resident, 0, len(snap.Models))
		for _, m := range snap.Models {
			if m.State != "ready" {
				continue
			}
			res = append(res, metrics.Resident{Model: m.ModelName, LoadedUnix: m.ReadyAtMS / 1000})
		}
		metrics.SetResidency(res)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func runQueueSampler(ctx context.Context, sc *sched.Scheduler, st *store.Store, interval, activityRetention, payloadRetention time.Duration) {
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
				// Payloads age out BEFORE rows, and the order here matters only
				// in that the row prune has already removed anything past
				// activityRetention — so this never touches a row on its way out.
				if payloadRetention > 0 {
					if n, err := st.PrunePayloads(time.Now().Add(-payloadRetention).UnixMilli()); err != nil {
						slog.Warn("prune payloads", "err", err)
					} else if n > 0 {
						slog.Info("cleared captured payloads (rows and their metrics kept)",
							"rows", n, "retention", payloadRetention)
					}
				}
			}
		}
	}
}

// envBool reads a boolean environment variable, accepting the spellings people
// actually type. Anything unrecognised is false: a security-relevant switch must
// not turn ON because of a typo in a value.
// exposedAddr reports whether a listen address is reachable from another
// machine, which is what decides how loud the insecure-mode warning should be.
//
// Unknown shapes are treated as EXPOSED. The failure directions are not
// symmetric: an over-loud warning on a loopback bind is noise, while a missing
// one on a public bind is an unauthenticated control plane nobody was told
// about.
func exposedAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		// A bare ":8111" fails to split on some inputs but is the common
		// "all interfaces" spelling, so treat it as exposed too.
		host = strings.TrimSpace(addr)
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
	}
	switch host {
	case "localhost":
		return false
	case "":
		return true // ":8111" — every interface
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// pick returns the first non-empty value — a precedence chain read left to
// right, e.g. flag, environment, config file, built-in default.
func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// watchReload re-reads the config on SIGHUP and installs it in every holder.
//
// A bad config NEVER replaces a good one. Load already parses, resolves
// extensions and validates; if any of that fails the running config stays and
// the error is logged. This is the same discipline `corrallm validate` exists
// for — the port must not go down because someone fat-fingered a pool size.
//
// Order is deliberate: residency and admission learn about the new config
// BEFORE the proxy starts routing on it. Each holder swaps its own pointer, so
// there is a window of a few microseconds where they disagree; updating the
// request entry point last means that window can only ever have downstream
// components knowing MORE than the proxy, never less.
//
// What this does not do: it will not un-spawn a backend whose model was
// deleted, and it will not resize a resident backend whose ramUsage changed.
// Residency is about processes that are already running and holding real
// memory; the config is a statement of intent for the NEXT spawn. Evicting
// someone's warm 27B because a file changed is not a reload, it is a restart
// with extra steps.
func watchReload(ctx context.Context, src *configdb.Source, st *store.Store, mgr *proc.Manager, sc *sched.Scheduler, px *proxy.Proxy, h *api.Handlers) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			// From the DATABASE. A SIGHUP used to re-read the file, which is
			// now neither what the daemon booted from nor what the dashboard
			// edits — reloading it would silently revert every change made
			// through the UI since startup.
			cfg, err := src.Load(ctx)
			if err != nil {
				slog.Error("config reload REJECTED; keeping the running config", "err", err)
				continue
			}
			applyConfig(cfg, st, mgr, sc, px, h)
			slog.Info("config reloaded", "from", "database",
				"servers", len(cfg.Servers), "models", len(cfg.Models),
				"lanes", len(cfg.Lanes), "groups", len(cfg.PriorityGroups))
		}
	}
}

// applyConfig installs a validated config in every holder.
//
// Order is deliberate: residency and admission learn about it BEFORE the proxy
// starts routing on it, so the brief window where holders disagree can only
// have downstream components knowing more than the entry point, never less.
//
// The carry-over lives HERE, not in the callers, because there are two reload
// paths — SIGHUP and the config API — and they drifted: the API path
// reinstalled selections and the signal path did not, so a SIGHUP silently
// dropped every hand-chosen model and every pool. A duplicated step is a step
// one copy will forget.
func applyConfig(cfg *config.Config, st *store.Store, mgr *proc.Manager, sc *sched.Scheduler, px *proxy.Proxy, h *api.Handlers) {
	// A reload builds a new Config, so anything held on the old one has to come
	// across. Two sources, two recoveries: pooled models are somebody else's
	// catalogue and cannot be refetched synchronously, so the previous overlay
	// is adopted; selections are durable and are reinstalled from the store.
	cfg.AdoptRuntime(h.Config())
	if st != nil {
		if rows, err := st.LoadSelections(); err != nil {
			slog.Warn("reload: load selections", "err", err)
		} else {
			api.InstallSelections(cfg, rows)
		}
	}
	mgr.SetConfig(cfg)
	sc.SetConfig(cfg)
	h.SetConfig(cfg)
	px.SetConfig(cfg)
}

// reloadInto re-reads path and applies it. Used by enrollment, which writes the
// config and needs the new server usable without a restart.
// reloadInto re-reads the stored config and installs it everywhere.
//
// Called after an API edit. It reads back from the STORE rather than reusing
// the in-memory config that was just saved, so what the daemon runs is always
// what persistence actually holds — if a write silently dropped something, the
// reload surfaces it immediately instead of at the next restart.
func reloadInto(ctx context.Context, src *configdb.Source, st *store.Store, mgr *proc.Manager, sc *sched.Scheduler, px *proxy.Proxy, h *api.Handlers) error {
	cfg, err := src.Load(ctx)
	if err != nil {
		return err
	}
	applyConfig(cfg, st, mgr, sc, px, h)
	return nil
}

// loadConfig reads the configuration, importing the legacy file exactly once.
//
// THE ONE-TIME IMPORT. An empty database with a config.yml beside it is an
// install that predates the move to SQLite; the file is read, stored and
// verified, and from then on the database is the only thing consulted. An empty
// database with no file is a fresh install, which serves nothing until
// something is configured — the same state a missing config file always
// produced.
//
// The import VERIFIES rather than trusting the write: the config read back out
// of the tables must be semantically equal to the one parsed from the file. A
// silent partial import would be the worst possible outcome here, because the
// daemon would come up serving a subset of what the operator declared and
// nothing would say so.
func loadConfig(ctx context.Context, src *configdb.Source, path string) (*config.Config, error) {
	empty, err := configdb.IsEmpty(ctx, src.DB)
	if err != nil {
		return nil, err
	}
	if empty && configdb.FileExists(path) {
		slog.Warn("importing the legacy config file into the database (one time)", "path", path)
		src = src.WithNote("imported from " + path)
		if _, err := src.ImportFile(ctx, path); err != nil {
			return nil, fmt.Errorf("importing %s: %w", path, err)
		}
		if err := src.VerifyAgainstFile(ctx, path); err != nil {
			return nil, fmt.Errorf("the import did not verify, so nothing will be trusted: %w", err)
		}
		// RETIRE THE FILE, but never without a copy first.
		//
		// It is deleted because a file nobody reads is a trap — edited,
		// restarted, and nothing changes. It is BACKED UP because this is the
		// moment the database becomes the only copy, and a verified import is
		// still a first import. The backup is a plain YAML file next to where
		// the config used to be; `corrallm config export` regenerates one any
		// time.
		backup := path + ".imported-" + time.Now().Format("20060102-150405")
		if err := os.Rename(path, backup); err != nil {
			slog.Warn("imported, but could not retire the old config file; it will be ignored",
				"path", path, "err", err)
		} else {
			slog.Info("retired the old config file", "moved_to", backup)
		}
		slog.Info("config imported and verified; the database is now authoritative",
			"hint", "corrallm config export writes it back out as YAML")
	} else if !empty && configdb.FileExists(path) {
		// A file that is no longer read is a trap: someone edits it, restarts,
		// and nothing changes. Say so on every boot until it is gone.
		slog.Warn("config.yml is present but NOT read — the database is authoritative",
			"path", path, "hint", "corrallm config export > "+path+" to refresh it, or delete it")
	}
	return src.Load(ctx)
}

// loadRecorder writes proc's load events to the store.
//
// Failures are logged and swallowed: a spawn that worked must never be reported
// as failed because the note about it could not be written, and this runs on the
// load path where anything that blocks adds latency to the very thing it is only
// describing.
type loadRecorder struct{ st *store.Store }

func (r loadRecorder) RecordLoad(ev proc.ModelLoadEvent) {
	if r.st == nil {
		return
	}
	if err := r.st.InsertModelLoad(store.ModelLoad{
		TS: time.Now().UnixMilli(), Model: ev.Model, Server: ev.Server,
		Requester: ev.Requester, Evicted: strings.Join(ev.Evicted, ","),
		MS: ev.MS, OK: ev.OK, Err: ev.Err,
	}); err != nil {
		slog.Warn("could not record a model load", "model", ev.Model, "err", err)
	}
}

// oidcSettings resolves auth.oidc for the SSO handlers: the client secret
// expanded from the credential store, the redirect URL defaulted from
// --public-base, the users with their roles. Nil when SSO is not configured
// or switched off.
func oidcSettings(c *config.Config, publicBase string) *auth.OIDCSettings {
	if c == nil || !c.Auth.OIDCEnabled() {
		return nil
	}
	o := c.Auth.OIDC
	users := map[string]auth.Role{}
	for _, u := range c.Auth.EffectiveUsers() {
		users[u.Email] = auth.Role(u.Role)
	}
	return &auth.OIDCSettings{
		Issuer:               strings.TrimSpace(o.Issuer),
		ClientID:             o.ClientID,
		ClientSecret:         config.ExpandSecrets(o.ClientSecret),
		RedirectURL:          api.OIDCRedirectURL(o, publicBase),
		Scopes:               o.ScopesOrDefault(),
		Users:                users,
		AllowUnverifiedEmail: o.AllowUnverifiedEmail,
	}
}
