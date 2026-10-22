package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/service"
)

// newServiceCmd manages corrallm's own systemd user service, or its launchd
// LaunchAgent on macOS (service_launchd.go).
//
// Everything here shells out to `systemctl --user` / `launchctl` rather than
// reimplementing them: the unit is the contract, and an operator must be able
// to use systemctl and journalctl (or launchctl) directly without corrallm
// being in the way.
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and control corrallm as a per-user service (systemd on Linux, launchd on macOS)",
		Long: "Install and control corrallm as a systemd user service, or on macOS as a\n" +
			"launchd LaunchAgent (~/Library/LaunchAgents/" + service.LabelPrefix + "<name>.plist,\n" +
			"logs in ~/Library/Logs/corrallm/<name>.log).\n\n" +
			"Building the binary is deliberately NOT part of this: a service cannot\n" +
			"build itself. Use `make install` then `corrallm service restart`.",
	}
	cmd.AddCommand(newServiceInstallCmd(), newServiceUninstallCmd())
	cmd.AddCommand(newServiceCtlCmds()...)
	return cmd
}

func systemctl(args ...string) *exec.Cmd {
	c := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c
}

// hasUserSystemd reports whether a user systemd is reachable. An ssh session
// with no login session, or a container, has none — and a confusing systemctl
// error is worse than saying so.
func hasUserSystemd() bool {
	c := exec.Command("systemctl", "--user", "is-system-running")
	return c.Run() == nil || c.ProcessState.ExitCode() < 4
}

func newServiceInstallCmd() *cobra.Command {
	var (
		name    string
		execBin string
		cfgPath string
		workDir string
		enable  bool
		start   bool
		envs    []string
		asAgent bool
	)
	cmd := &cobra.Command{
		Use:   "install [-- <serve flags>...]",
		Short: "Write the service definition (everything after -- is passed to `serve`)",
		Long: "Write the service definition: a systemd user unit on Linux, a LaunchAgent on macOS.\n\n" +
			"Arguments after `--` are passed through to `corrallm serve` verbatim, so the\n" +
			"unit carries exactly the flags you would have typed:\n\n" +
			"  corrallm service install --enable --env ADDR=0.0.0.0:8111 -- --web-root /srv/ui\n\n" +
			"With --agent the service runs `corrallm agent` instead (args go to `agent`), and\n" +
			"stopping it leaves the agent's model backends running for the next agent:\n\n" +
			"  corrallm service install --agent --enable -- --config /path/agent.yml\n",
		RunE: func(cmd *cobra.Command, args []string) error {
			if asAgent && !cmd.Flags().Changed("name") {
				name = service.DefaultAgentName
			}
			if execBin == "" {
				p, err := os.Executable()
				if err != nil {
					return fmt.Errorf("locate this binary (pass --exec): %w", err)
				}
				// Resolve symlinks so the unit names the real file rather than a
				// shim that may be repointed later without anyone noticing.
				if r, err := filepath.EvalSymlinks(p); err == nil {
					p = r
				}
				execBin = p
			}
			if !filepath.IsAbs(execBin) {
				abs, err := filepath.Abs(execBin)
				if err != nil {
					return err
				}
				execBin = abs
			}

			// Pick the config out of the passthrough args so the unit can
			// validate before it starts, without asking for it twice. Not for
			// an agent: `validate` checks a PRIMARY config.
			if cfgPath == "" && !asAgent {
				cfgPath = flagValue(args, "--config")
			}
			if asAgent && workDir == "" {
				if c := flagValue(args, "--config"); c != "" {
					if abs, err := filepath.Abs(c); err == nil {
						workDir = filepath.Dir(abs)
					}
				}
			}
			if cfgPath != "" {
				if abs, err := filepath.Abs(cfgPath); err == nil {
					cfgPath = abs
				}
			}
			if workDir == "" && cfgPath != "" {
				workDir = filepath.Dir(cfgPath)
			}

			// serve has no --addr: it reads ADDR from the environment. Passing
			// the flag exits 1 with "unknown flag", and Restart=on-failure turns
			// that into a crash loop rather than a visible failure — which is
			// exactly how this went out the first time. Refuse it here, where
			// the fix is one line away, instead of at 2am in a journal.
			if v := flagValue(args, "--addr"); v != "" && !asAgent {
				return fmt.Errorf("`serve` has no --addr flag; it reads the listen address from ADDR.\n"+
					"  use: --env ADDR=%s", v)
			}
			u := service.Unit{Name: name, Exec: execBin, Args: args, ConfigPath: cfgPath, WorkingDir: workDir, Env: envs, Agent: asAgent}
			if useLaunchd() {
				// A LaunchAgent starts with launchd's PATH (/usr/bin:/bin:…),
				// not the login shell's: the Mac agent could not find cmake in
				// /opt/homebrew/bin and failed every tool build unseen. Carry
				// the installing shell's PATH unless one was given.
				if !slices.ContainsFunc(u.Env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
					u.Env = append(u.Env, "PATH="+os.Getenv("PATH"))
				}
				return launchdInstall(cmd.OutOrStdout(), u, enable, start)
			}
			dir, err := service.UserUnitDir()
			if err != nil {
				return err
			}
			path, err := u.Install(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote %s\n", path)
			fmt.Fprintf(out, "  exec   %s\n", execBin)
			if cfgPath != "" {
				fmt.Fprintf(out, "  config %s (validated before each start)\n", cfgPath)
			}
			fmt.Fprintf(out, "  serve  %s\n", strings.Join(args, " "))

			if !hasUserSystemd() {
				fmt.Fprintf(out, "\nNOTE: no user systemd is reachable here, so the unit was written but not loaded.\n")
				return nil
			}
			if err := systemctl("daemon-reload").Run(); err != nil {
				return fmt.Errorf("daemon-reload: %w", err)
			}
			if enable {
				if err := systemctl("enable", u.UnitFileName()).Run(); err != nil {
					return fmt.Errorf("enable: %w", err)
				}
				fmt.Fprintf(out, "enabled at login\n")
			}
			if start {
				if err := systemctl("restart", u.UnitFileName()).Run(); err != nil {
					return fmt.Errorf("start: %w", err)
				}
				fmt.Fprintf(out, "started\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", service.DefaultName, "unit name (without .service)")
	cmd.Flags().StringVar(&execBin, "exec", "", "path to the corrallm binary (default: this one, symlinks resolved)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "config to validate before each start (default: --config from the passthrough args)")
	cmd.Flags().StringVar(&workDir, "working-dir", "", "unit WorkingDirectory (default: the config's directory)")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "environment for the service as KEY=VALUE (repeatable); the listen address is ADDR=host:port")
	cmd.Flags().BoolVar(&enable, "enable", false, "also enable the service so it starts at login")
	cmd.Flags().BoolVar(&start, "start", false, "also start the service now")
	cmd.Flags().BoolVar(&asAgent, "agent", false, "install a unit for `corrallm agent` (default name "+service.DefaultAgentName+"; backends survive a stop)")
	return cmd
}

// flagValue finds "--name value" or "--name=value" in a passthrough arg list.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func newServiceUninstallCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop, disable and remove the service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if useLaunchd() {
				return launchdUninstall(cmd.OutOrStdout(), name)
			}
			unit := name + ".service"
			if hasUserSystemd() {
				// Best-effort: a unit that was never started or never enabled
				// must not make removal fail.
				_ = systemctl("stop", unit).Run()
				_ = systemctl("disable", unit).Run()
			}
			dir, err := service.UserUnitDir()
			if err != nil {
				return err
			}
			p := filepath.Join(dir, unit)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			if hasUserSystemd() {
				_ = systemctl("daemon-reload").Run()
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", p)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", service.DefaultName, "unit name (without .service)")
	return cmd
}

// unitSel is the --name / --agent / --all choice every control verb takes.
type unitSel struct {
	name  string
	agent bool
	all   bool
}

func (u *unitSel) flags(c *cobra.Command) {
	c.Flags().StringVar(&u.name, "name", service.DefaultName, "unit name (without .service)")
	c.Flags().BoolVar(&u.agent, "agent", false, "the agent's unit ("+service.DefaultAgentName+" unless --name says otherwise)")
	c.Flags().BoolVar(&u.all, "all", false, "every unit `corrallm service install` wrote: the server first, then agents")
}

// resolve turns the flags into the units to act on.
func (u *unitSel) resolve(c *cobra.Command) ([]service.Installed, error) {
	if u.all {
		if c.Flags().Changed("name") || u.agent {
			return nil, fmt.Errorf("--all already means every unit; drop --name/--agent")
		}
		dir, err := service.UserUnitDir()
		if useLaunchd() {
			dir, err = service.LaunchAgentsDir()
		}
		if err != nil {
			return nil, err
		}
		units, err := service.FindInstalled(dir, useLaunchd())
		if err != nil {
			return nil, err
		}
		if len(units) == 0 {
			return nil, fmt.Errorf("no corrallm units installed in %s", dir)
		}
		return units, nil
	}
	name := u.name
	if u.agent && !c.Flags().Changed("name") {
		name = service.DefaultAgentName
	}
	return []service.Installed{{Name: name, Agent: u.agent || installedIsAgent(name)}}, nil
}

// installedIsAgent reads the installed unit or plist: an agent's --config is
// an agent.yml, which `validate` would reject as a server config.
func installedIsAgent(name string) bool {
	if useLaunchd() {
		p, err := launchdPlistPath(name)
		if err != nil {
			return false
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		args := service.PlistArgs(string(b))
		return len(args) > 1 && args[1] == "agent"
	}
	dir, err := service.UserUnitDir()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".service"))
	return err == nil && service.UnitIsAgent(string(b))
}

// eachUnit runs fn on every selected unit, naming each when there is more
// than one, and carries on past a failure so one bad unit does not leave the
// rest untouched.
func eachUnit(c *cobra.Command, sel *unitSel, fn func(u service.Installed) error) error {
	units, err := sel.resolve(c)
	if err != nil {
		return err
	}
	var failed []string
	for _, u := range units {
		if len(units) > 1 {
			fmt.Fprintf(c.OutOrStdout(), "== %s\n", u.Name)
		}
		if err := fn(u); err != nil {
			if len(units) == 1 {
				return err
			}
			fmt.Fprintf(c.ErrOrStderr(), "%s: %v\n", u.Name, err)
			failed = append(failed, u.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// newServiceCtlCmds are the thin systemctl passthroughs, plus a restart that
// validates first. Each takes --name, --agent or --all.
func newServiceCtlCmds() []*cobra.Command {
	var cmds []*cobra.Command
	for _, verb := range []string{"start", "stop", "reload", "status", "logs"} {
		var sel unitSel
		action := verb
		short := map[string]string{
			"start":  "Start the service",
			"stop":   "Stop the service (it drains in-flight requests first)",
			"reload": "Re-read the config in the running daemon (SIGHUP to corrallm only, never its backends)",
			"status": "Show whether the service is running",
			"logs":   "Show the service's recent log",
		}[action]
		c := &cobra.Command{
			Use:   action,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return eachUnit(c, &sel, func(u service.Installed) error {
					if useLaunchd() {
						return launchdCtl(action, u.Name)
					}
					unit := u.Name + ".service"
					if action == "logs" {
						j := exec.Command("journalctl", "--user", "-u", unit, "-n", "200", "--no-pager")
						j.Stdout, j.Stderr = os.Stdout, os.Stderr
						return j.Run()
					}
					return systemctl(action, unit).Run()
				})
			},
		}
		sel.flags(c)
		cmds = append(cmds, c)
	}

	var rsel unitSel
	var rcfg string
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Validate the config, then restart the service",
		Long: "Validate the config, then restart the service.\n\n" +
			"The validation is the point: `serve` frees the listen port before it parses,\n" +
			"so restarting onto a broken config takes the gateway down instead of leaving\n" +
			"the running instance alone. Checking here means a bad config costs nothing.\n\n" +
			"--agent restarts the agent instead (its backends keep running); --all restarts\n" +
			"every installed unit, the server first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return eachUnit(cmd, &rsel, func(u service.Installed) error {
				unit := u.Name + ".service"
				// An agent's --config is an agent.yml: there is no server
				// config to validate, and `validate` would reject it.
				if !u.Agent {
					cfg := rcfg
					if cfg == "" {
						if useLaunchd() {
							cfg = launchdConfigPath(u.Name)
						} else {
							cfg = unitConfigPath(unit)
						}
					}
					if cfg != "" {
						self, err := os.Executable()
						if err != nil {
							return err
						}
						// Bounded: validate opens the database the running server
						// holds, and a server wedged on it would hold this too.
						fmt.Fprintf(cmd.OutOrStdout(), "validating %s\n", cfg)
						ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
						defer cancel()
						v := exec.CommandContext(ctx, self, "validate", "--config", cfg)
						v.Stdout, v.Stderr = cmd.OutOrStdout(), os.Stderr
						if err := v.Run(); err != nil {
							if ctx.Err() != nil {
								return fmt.Errorf("validate did not finish in 30s — the running server may be stuck on its database. NOT restarting. "+
									"Kill it instead; its supervisor starts it again (launchd: `launchctl kill QUIT gui/$(id -u)/com.iodesystems.%s`, "+
									"which also writes every goroutine's stack to its log)", u.Name)
							}
							return fmt.Errorf("config invalid — NOT restarting; the running instance is untouched")
						}
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "restarting %s (a server drains in-flight requests first: up to 90s)\n", u.Name)
				if useLaunchd() {
					return launchdCtl("restart", u.Name)
				}
				return systemctl("restart", unit).Run()
			})
		},
	}
	rsel.flags(restart)
	restart.Flags().StringVar(&rcfg, "config", "", "config to validate (default: the one recorded in the unit)")
	return append(cmds, restart, newServiceUpgradeCmd(), newServiceLoginAdminCmd())
}

// newServiceUpgradeCmd hands the running daemon over to the binary now at its
// path, without dropping a connection or a request (blue-green; see
// internal/handoff). The old process finishes its in-flight requests and
// exits on its own.
func newServiceUpgradeCmd() *cobra.Command {
	var (
		name    string
		pid     int
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Hand the running daemon over to the binary at its path, without downtime",
		Long: "Hand the running daemon over to the binary at its path, without downtime.\n\n" +
			"The running process starts the new binary with its listening socket; the new\n" +
			"one takes every connection from the moment it is ready, and the old one\n" +
			"finishes its in-flight requests (up to --handoff-drain, default 10m) and exits.\n" +
			"If the new binary fails to start, the old one keeps serving and this command\n" +
			"says why and exits 3 (any other failure: 1). Needs a service installed by\n" +
			"`corrallm service install`. Linux only: launchd (macOS) cannot follow the\n" +
			"main process to a new pid, so there use `corrallm service restart`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if useLaunchd() && pid == 0 {
				// Exit 1, not 3: the binary is fine, the supervisor is the
				// limit, so bin/deploy falls back to a restart.
				return fmt.Errorf("launchd cannot follow the main process to a new pid, so there is no handoff under it; use `corrallm service restart`")
			}
			// The unit's main pid, when the daemon runs under one: it moving
			// to a new process is proof of a handoff on its own, should the
			// status file be unreadable from here.
			unitPID := func() int {
				out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", name+".service").Output()
				if err != nil {
					return 0
				}
				n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
				return n
			}
			viaUnit := pid == 0
			if viaUnit {
				if pid = unitPID(); pid == 0 {
					return fmt.Errorf("%s.service is not running (or pass --pid)", name)
				}
			}
			sent := time.Now()
			if err := sendUpgrade(pid); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "asked pid %d to hand over; waiting for its successor\n", pid)
			for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
				var st handoffStatus
				found := false
				for _, p := range handoffStatusPaths(pid) {
					b, err := os.ReadFile(p)
					if err == nil && json.Unmarshal(b, &st) == nil && st.From == pid && st.AtMS >= sent.UnixMilli() {
						found = true
						break
					}
				}
				if !found {
					if viaUnit {
						if now := unitPID(); now != 0 && now != pid {
							fmt.Fprintf(cmd.OutOrStdout(), "handed over: pid %d -> %d (systemd's main pid moved)\n", pid, now)
							return nil
						}
					}
					continue
				}
				if !st.OK {
					// Exit 3 when the NEW BINARY is the problem: a caller that
					// would fall back to a restart (bin/deploy) must not restart
					// onto a binary that does not start. Any other failure is 1.
					fmt.Fprintf(cmd.ErrOrStderr(), "handoff failed; pid %d is still serving: %s\n", pid, st.Error)
					if strings.HasPrefix(st.Error, "handoff: successor") {
						os.Exit(3)
					}
					return fmt.Errorf("handoff failed; pid %d is still serving", pid)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "handed over: pid %d -> %d; the old process exits once its requests finish\n", pid, st.To)
				return nil
			}
			return fmt.Errorf("no handoff result from pid %d within %s (looked in %s)", pid, timeout,
				strings.Join(handoffStatusPaths(pid), ", "))
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", service.DefaultName, "unit name (without .service), to find the running pid")
	f.IntVar(&pid, "pid", 0, "the daemon to hand over (default: the unit's MainPID)")
	f.DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the new process to report ready")
	return cmd
}

// unitConfigPath recovers the --config the unit was installed with, so restart
// validates the same file the service will actually read.
func unitConfigPath(unit string) string {
	dir, err := service.UserUnitDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, unit))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "ExecStart=") && !strings.HasPrefix(line, "ExecStartPre=") {
			continue
		}
		if v := flagValue(strings.Fields(line), "--config"); v != "" {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
