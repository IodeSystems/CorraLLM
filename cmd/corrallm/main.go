// Command corrallm is the OpenAI-compatible reverse proxy + model lifecycle
// manager + fairshare scheduler. P0 ships the scaffold: a gat gateway (REST +
// GraphQL), the SPA, config loading, and the SQLite store. The proxy core and
// scheduler land in later phases (see plan/plan.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/api"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
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
	root.AddCommand(newServeCmd(), newDumpGraphQLCmd(), newVersionCmd())
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

func newServeCmd() *cobra.Command {
	var (
		home, service, webRoot, configPath, dbPath string
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
			return serve(cmd.Context(), serveOpts{
				webRoot:    pick(webRoot, envOr("WEB_ROOT", "./ui/dist")),
				configPath: pick(configPath, envOr("CORRALLM_CONFIG", "./corrallm.yaml")),
				dbPath:     pick(dbPath, envOr("CORRALLM_DB", "./home/var/corrallm.db")),
				addr:       envOr("ADDR", ":6502"),
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&home, "home", envOr("CORRALLM_HOME", "./home"), "config home holding the layered .properties files")
	f.StringVar(&service, "service", envOr("CORRALLM_SLOT", "dev"), "service/slot selecting override .properties (dev|current|next)")
	f.StringVar(&webRoot, "web-root", "", "directory to serve the SPA from (default ./ui/dist or WEB_ROOT)")
	f.StringVar(&configPath, "config", "", "path to the corrallm YAML config (default ./corrallm.yaml or CORRALLM_CONFIG)")
	f.StringVar(&dbPath, "db", "", "path to the SQLite database (default ./home/var/corrallm.db or CORRALLM_DB)")
	return cmd
}

type serveOpts struct {
	webRoot, configPath, dbPath, addr string
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
	defer mgr.Shutdown()
	// Preload pinned (persistent) models in the background so boot isn't blocked.
	go mgr.Preload(ctx)

	h := &api.Handlers{Version: version, Cfg: cfg, Store: st}

	router := chi.NewRouter()
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)

	// BuildGateway mounts REST + GraphQL (/api/graphql) + schema views onto router.
	if _, err := api.BuildGateway(router, h); err != nil {
		return err
	}

	// OpenAI-compatible inference passthrough (raw, streaming — bypasses gat),
	// gated by the fairshare admission scheduler.
	proxy.New(cfg, mgr, sched.NewWithConfig(cfg), st).Mount(router)

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
