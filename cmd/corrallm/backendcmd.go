package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/service"
	"github.com/iodesystems/corrallm/internal/store"
)

// restartClaimWindow is how old a pending restart request may be and still be
// run. A reload claims what is pending; an older row's CLI has given up.
const restartClaimWindow = 2 * time.Minute

// restartClaimWait is how long the CLI waits for the daemon to claim its
// request before it concludes the daemon does not know the table (an older
// binary) or did not get the reload.
const restartClaimWait = 20 * time.Second

// newBackendCmd is `corrallm backend`: operator actions on one local backend,
// done through the running daemon without the admin token (it may be
// disabled). The request goes through the database and a reload, as `spend`
// and `config load` do.
func newBackendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backend",
		Short: "Operate on one local backend through the running daemon (no admin token)",
		Args:  cobra.NoArgs,
	}
	var (
		reason  string
		drain   time.Duration
		timeout time.Duration
		unit    string
	)
	restart := &cobra.Command{
		Use:   "restart <model>",
		Short: "Drain one local backend, stop it and start it again from the current config",
		Long: "Drain one local backend, stop it and start it again from the current config.\n\n" +
			"For a changed launch: a new tool build activated (${tool:x} is resolved at\n" +
			"spawn), an env var or flag added to the cmd. The daemon first reloads the\n" +
			"config (SIGHUP to its main pid only), then:\n\n" +
			"  1. draining  the backend takes no new work. A request that reaches it\n" +
			"               waits for the new process; the model's scheduler queue still\n" +
			"               answers 429 + Retry-After when it is full.\n" +
			"  2. in-flight requests finish, up to --drain; past it the backend is\n" +
			"     stopped anyway and the requests still running are cut.\n" +
			"  3. stopping  SIGTERM, then SIGKILL after 15s.\n" +
			"  4. loading   the model starts from the config the reload read; held\n" +
			"               requests are served when it is healthy.\n\n" +
			"A model with no running process is not started; its next request starts it\n" +
			"with the current config. <model> is the served name, or a case-insensitive\n" +
			"part of exactly one local model's name (\"swift\").",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			served, _, err := resolveLocalModel(cfg, args[0])
			if err != nil {
				return err
			}
			reload := func() error {
				self, err := os.Executable()
				if err != nil {
					return err
				}
				return exec.Command(self, "service", "reload", "--name", unit).Run()
			}
			return restartViaDaemon(cmd.Context(), cmd.OutOrStdout(), st, served, reason, drain, timeout,
				reload, 500*time.Millisecond, restartClaimWait)
		},
	}
	f := restart.Flags()
	f.StringVar(&reason, "reason", "", "why, recorded on the backend's stop event")
	f.DurationVar(&drain, "drain", proc.DefaultRestartDrain, "how long in-flight requests may run before the backend is stopped anyway")
	f.DurationVar(&timeout, "timeout", 0, "how long to wait for the whole restart (default: --drain + 15m)")
	f.StringVar(&unit, "name", service.DefaultName, "the daemon's service name (without .service)")
	cmd.AddCommand(restart)
	return cmd
}

// restartViaDaemon files a restart request, asks the daemon to reload, and
// follows the request until it finishes, printing each stage.
func restartViaDaemon(ctx context.Context, out io.Writer, st *store.Store, served, reason string, drain, timeout time.Duration,
	reload func() error, poll, claimWait time.Duration) error {
	if drain <= 0 {
		return errors.New("--drain must be positive")
	}
	if timeout <= 0 {
		timeout = drain + 15*time.Minute
	}
	id, err := st.AddBackendRestart(served, reason, drain, time.Now())
	if err != nil {
		return fmt.Errorf("recording the request: %w", err)
	}
	if err := reload(); err != nil {
		_, _ = st.AbandonBackendRestart(id, "the daemon could not be reloaded: "+err.Error(), time.Now())
		return fmt.Errorf("could not reload the daemon (`corrallm service reload`): %w; nothing was restarted", err)
	}
	fmt.Fprintf(out, "restart %d of %s requested (drain up to %s)\n", id, served, drain)

	start := time.Now()
	last := store.RestartPending
	for {
		r, err := st.GetBackendRestart(id)
		if err != nil {
			return err
		}
		if r.State != last {
			last = r.State
			if !r.Finished() {
				fmt.Fprintf(out, "%s: %s\n", r.State, r.Detail)
			}
		}
		switch {
		case r.State == store.RestartDone:
			fmt.Fprintf(out, "done: %s\n", r.Detail)
			return nil
		case r.State == store.RestartFailed:
			return fmt.Errorf("restart failed: %s", r.Detail)
		case r.State == store.RestartPending && time.Since(start) > claimWait:
			if ok, _ := st.AbandonBackendRestart(id, "no daemon claimed it", time.Now()); ok {
				return fmt.Errorf("the running daemon did not take the request within %s: it may predate `backend restart` "+
					"(upgrade it), or the reload did not reach it; nothing was restarted", claimWait)
			}
		case time.Since(start) > timeout:
			return fmt.Errorf("still %s after %s; the daemon goes on with it (see its log)", r.State, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting (%w); the daemon goes on with the restart", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// runBackendRestarts is the daemon's half: after a reload, claim the pending
// restart requests and run each in the background, writing its progress to
// its row. reloadErr is the reload's outcome: a rejected config fails the
// requests, because the restart was asked for to pick up that config.
func runBackendRestarts(ctx context.Context, st *store.Store, mgr *proc.Manager, reloadErr error) {
	if st == nil {
		return
	}
	now := time.Now()
	reqs, err := st.ClaimBackendRestarts(now.Add(-restartClaimWindow), now)
	if err != nil {
		slog.Warn("backend restart: could not read requests", "err", err)
		return
	}
	for _, r := range reqs {
		if reloadErr != nil {
			_ = st.UpdateBackendRestart(r.ID, store.RestartFailed, "the config reload was rejected, so nothing was restarted: "+reloadErr.Error(), time.Now())
			continue
		}
		go runBackendRestart(ctx, st, mgr, r)
	}
}

func runBackendRestart(ctx context.Context, st *store.Store, mgr *proc.Manager, r store.BackendRestart) {
	update := func(state, detail string) {
		if err := st.UpdateBackendRestart(r.ID, state, detail, time.Now()); err != nil {
			slog.Warn("backend restart: could not record progress", "id", r.ID, "err", err)
		}
	}
	res, err := mgr.Restart(ctx, r.Model, proc.RestartOptions{Reason: r.Reason, DrainTimeout: r.Drain, Progress: update})
	if err != nil {
		update(store.RestartFailed, err.Error())
		return
	}
	update(store.RestartDone, restartSummary(res))
}

func restartSummary(res proc.RestartResult) string {
	if !res.Running {
		return "it was not running; its next request starts it with the current config"
	}
	cmd := "cmd unchanged, " + res.NewCmdHash
	if res.CmdChanged {
		cmd = "cmd changed " + res.OldCmdHash + " -> " + res.NewCmdHash
	}
	if res.StaleConfig {
		cmd += "; WARNING: the config was reloaded after the spawn and its cmd differs, restart again to pick it up"
	}
	return fmt.Sprintf("%s -> %s (%s); drained in %s (%d in flight, %d cut); loaded in %s",
		res.OldID, res.NewID, cmd, res.Drained.Round(time.Millisecond), res.InFlight, res.Cut, res.Loaded.Round(time.Millisecond))
}
