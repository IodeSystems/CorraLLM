package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/agent"
)

// agent runs this machine as compute for a remote corrallm.
//
// One binary, a subcommand rather than a separate corrallm-agent: an agent that
// spawns, supervises and measures processes is substantially the code `serve`
// already runs, and a second binary would duplicate internal/host and drift
// from it. The two modes stay separate where it matters — `serve` gates /api
// with the admin token, the agent has its own credential and its own surface.
func newAgentCmd() *cobra.Command {
	var (
		addr       string
		token      string
		allowNoTok bool
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run this machine as compute for a remote corrallm (spawns backends on its behalf)",
		Long: "Run this machine as compute for a remote corrallm.\n\n" +
			"The agent executes shell commands sent by the primary — that is what it is for.\n" +
			"Treat exposing it exactly as you would treat exposing a shell: require a token,\n" +
			"and put it on a network you trust (LAN or VPN), not the open internet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token = pick(token, os.Getenv("CORRALLM_AGENT_TOKEN"))
			// Refusing by default is the point. This endpoint runs arbitrary
			// shell commands, so an agent that comes up unauthenticated because
			// someone forgot a flag is a remote shell on the network. Make the
			// insecure case something you have to say out loud.
			if token == "" && !allowNoTok {
				return errors.New("no agent token: set --token or CORRALLM_AGENT_TOKEN.\n" +
					"This endpoint executes shell commands sent by the primary; running it\n" +
					"unauthenticated exposes a remote shell. Pass --allow-no-token only on an\n" +
					"isolated network where you accept that.")
			}
			return runAgent(cmd.Context(), addr, token)
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", envOr("CORRALLM_AGENT_ADDR", ":6503"), "listen address")
	f.StringVar(&token, "token", "", "shared secret the primary must present (or CORRALLM_AGENT_TOKEN)")
	f.BoolVar(&allowNoTok, "allow-no-token", false, "run WITHOUT authentication — exposes a remote shell; isolated networks only")
	return cmd
}

func runAgent(ctx context.Context, addr, token string) error {
	a := agent.New(version, token)

	srv := &http.Server{Addr: addr, Handler: a.Routes()}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("corrallm agent listening", "addr", addr, "version", version,
			"protocol", agent.Protocol, "authenticated", token != "")
		if token == "" {
			slog.Warn("agent is UNAUTHENTICATED — anyone who can reach this port can run commands here")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-sigCtx.Done():
	}

	// Stop the backends BEFORE the listener: leaving them running would strand
	// processes the primary can no longer reach a handle for, and nothing would
	// ever reap them.
	slog.Info("corrallm agent shutting down; stopping backends")
	a.Shutdown()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("agent shutdown: %w", err)
	}
	return nil
}
