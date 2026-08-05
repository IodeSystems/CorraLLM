package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
		enroll     string
		primary    string
		server     string
		selfUpdate bool
		cfgPath    string
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
			// Precedence: flags, then environment, then the config file. The
			// file is the installed default; a flag is someone overriding it
			// deliberately for one run.
			fc, err := agent.LoadFileConfig(pick(cfgPath, envOr("CORRALLM_AGENT_CONFIG", "./agent.yml")))
			if err != nil {
				return err
			}
			token = pick(token, os.Getenv("CORRALLM_AGENT_TOKEN"), fc.Token)
			primary = pick(primary, os.Getenv("CORRALLM_PRIMARY"), fc.Primary)
			server = pick(server, os.Getenv("CORRALLM_AGENT_SERVER"), fc.Server)
			addr = pick(addr, os.Getenv("CORRALLM_AGENT_ADDR"), fc.Addr, ":6503")
			if fc.SelfUpdate != nil && !cmd.Flags().Changed("self-update") {
				selfUpdate = *fc.SelfUpdate
			}
			enrollTok := pick(enroll, os.Getenv("CORRALLM_ENROLL_TOKEN"), fc.EnrollToken)

			// Enroll FIRST when we have a one-time token and no long-lived one.
			// This is what makes attaching a machine a single command: the agent
			// brings its own measurements, the primary writes the server entry
			// and hands back the credential everything after this uses.
			if token == "" && enrollTok != "" && primary != "" {
				res, err := agent.Enroll(cmd.Context(), primary, enrollTok, server, addr, version)
				if err != nil {
					return fmt.Errorf("enrollment failed: %w", err)
				}
				token, server = res.Token, res.Server
				slog.Info("enrolled with the primary", "server", res.Server, "pools", res.Pools)
				// Persist so a restart does not re-enroll with a token that is
				// already spent — which fails with a confusing "already used".
				fc.Primary, fc.Server, fc.Token, fc.Addr = primary, server, token, addr
				fc.EnrollToken = ""
				if p := pick(cfgPath, envOr("CORRALLM_AGENT_CONFIG", "./agent.yml")); p != "" {
					if err := agent.SaveFileConfig(p, fc); err != nil {
						slog.Warn("could not persist the agent config; a restart will need re-enrolling",
							"path", p, "err", err)
					}
				}
			}
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
			return runAgent(cmd.Context(), addr, token,
				pick(primary, os.Getenv("CORRALLM_PRIMARY")),
				pick(server, os.Getenv("CORRALLM_AGENT_SERVER")), selfUpdate)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfgPath, "config", "", "agent config file (default ./agent.yml or CORRALLM_AGENT_CONFIG)")
	f.StringVar(&addr, "addr", "", "listen address (default from the config file, else :6503)")
	f.StringVar(&token, "token", "", "shared secret the primary must present (or CORRALLM_AGENT_TOKEN)")
	f.BoolVar(&allowNoTok, "allow-no-token", false, "run WITHOUT authentication — exposes a remote shell; isolated networks only")
	f.StringVar(&enroll, "enroll-token", "", "one-time enrollment token; attaches this machine and exchanges it for a long-lived credential (or CORRALLM_ENROLL_TOKEN)")
	f.StringVar(&primary, "primary", "", "base URL of the corrallm to report liveness to, e.g. http://box1:8111 (or CORRALLM_PRIMARY)")
	f.StringVar(&server, "server", "", "the `servers:` key in the primary's config that this machine backs (or CORRALLM_AGENT_SERVER)")
	f.BoolVar(&selfUpdate, "self-update", true, "replace this binary with the primary's build when versions differ AND no backends are running here")
	return cmd
}

func runAgent(ctx context.Context, addr, token, primary, server string, selfUpdate bool) error {
	a := agent.New(version, token)

	// Keep start.sh in step with this build. Self-update replaces the binary and
	// re-execs, so the NEW image is the only thing that knows the new launcher —
	// which makes "on every start" the right time to write it. Anchored on the
	// binary's own directory rather than cwd, so it still finds the install even
	// when someone runs the binary from elsewhere.
	//
	// Non-fatal: a read-only or relocated install directory is no reason to
	// refuse to serve.
	if exe, err := os.Executable(); err == nil {
		if err := agent.ReconcileLauncher(filepath.Dir(exe)); err != nil {
			slog.Warn("agent: could not update start.sh", "err", err)
		}
		// Before anything is spawned: kill backends a previous agent left
		// running. start.sh restarts this process on every crash and every
		// manual stop, and each restart used to strand whatever it supervised
		// as a process nothing could account for or reap.
		a.SetStateDir(filepath.Dir(exe))
	}

	// Listen BEFORE the beacon starts, because the beacon has to advertise the
	// port that was actually bound. With `addr: ":0"` the OS picks one, and the
	// only way anyone learns it is from the listener — which is the point:
	// a dynamically chosen port removes the standing conflict where a stale
	// agent holds :6503 and its replacement cannot start.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	boundAddr := ln.Addr().String()

	srv := &http.Server{Handler: a.Routes()}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Report liveness OUTWARD. The agent knows it is alive, may sit behind NAT,
	// and may change networks; requiring the primary to reach in would make a
	// laptop look permanently down even while it is serving.
	if primary != "" && server != "" {
		b := &agent.Beacon{Primary: primary, Server: server, Token: token, Srv: a,
			SelfUpdate: selfUpdate, Addr: boundAddr}
		go b.Run(sigCtx)
	} else if primary != "" || server != "" {
		slog.Warn("agent: --primary and --server must BOTH be set to heartbeat; not reporting liveness",
			"primary", primary, "server", server)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("corrallm agent listening", "addr", boundAddr, "configured", addr,
			"version", version, "protocol", agent.Protocol, "authenticated", token != "")
		if token == "" {
			slog.Warn("agent is UNAUTHENTICATED — anyone who can reach this port can run commands here")
		}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
