package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/service"
)

// newServiceCmd manages corrallm's own systemd user service.
//
// Everything here shells out to `systemctl --user` rather than reimplementing
// it: the unit is the contract, and an operator must be able to use systemctl
// and journalctl directly without corrallm being in the way.
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and control corrallm as a systemd user service",
		Long: "Install and control corrallm as a systemd user service.\n\n" +
			"Building the binary is deliberately NOT part of this: a service cannot\n" +
			"build itself. Use `go install ./cmd/corrallm` then `corrallm service restart`.",
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
	)
	cmd := &cobra.Command{
		Use:   "install [-- <serve flags>...]",
		Short: "Write the systemd user unit (everything after -- is passed to `serve`)",
		Long: "Write the systemd user unit.\n\n" +
			"Arguments after `--` are passed through to `corrallm serve` verbatim, so the\n" +
			"unit carries exactly the flags you would have typed:\n\n" +
			"  corrallm service install --enable -- --addr 0.0.0.0:8111 --web-root /srv/ui\n",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			// validate before it starts, without asking for it twice.
			if cfgPath == "" {
				cfgPath = flagValue(args, "--config")
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
			if v := flagValue(args, "--addr"); v != "" {
				return fmt.Errorf("`serve` has no --addr flag; it reads the listen address from ADDR.\n"+
					"  use: --env ADDR=%s", v)
			}
			u := service.Unit{Name: name, Exec: execBin, Args: args, ConfigPath: cfgPath, WorkingDir: workDir, Env: envs}
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
	cmd.Flags().StringArrayVar(&envs, "env", nil, "environment for the unit as KEY=VALUE (repeatable); the listen address is ADDR=host:port")
	cmd.Flags().BoolVar(&enable, "enable", false, "also enable the unit so it starts at login")
	cmd.Flags().BoolVar(&start, "start", false, "also start the unit now")
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
		Short: "Stop, disable and remove the unit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

// newServiceCtlCmds are the thin systemctl passthroughs, plus a restart that
// validates first.
func newServiceCtlCmds() []*cobra.Command {
	var cmds []*cobra.Command
	for _, verb := range []string{"start", "stop", "status", "logs"} {
		var name string
		action := verb
		c := &cobra.Command{
			Use:   action,
			Short: "systemctl --user " + action + " the corrallm unit",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				unit := name + ".service"
				if action == "logs" {
					j := exec.Command("journalctl", "--user", "-u", unit, "-n", "200", "--no-pager")
					j.Stdout, j.Stderr = os.Stdout, os.Stderr
					return j.Run()
				}
				return systemctl(action, unit).Run()
			},
		}
		c.Flags().StringVar(&name, "name", service.DefaultName, "unit name (without .service)")
		cmds = append(cmds, c)
	}

	var rname, rcfg string
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Validate the config, then restart the unit",
		Long: "Validate the config, then restart the unit.\n\n" +
			"The validation is the point: `serve` frees the listen port before it parses,\n" +
			"so restarting onto a broken config takes the gateway down instead of leaving\n" +
			"the running instance alone. Checking here means a bad config costs nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			unit := rname + ".service"
			cfg := rcfg
			if cfg == "" {
				cfg = unitConfigPath(unit)
			}
			if cfg != "" {
				self, err := os.Executable()
				if err != nil {
					return err
				}
				v := exec.Command(self, "validate", "--config", cfg)
				v.Stdout, v.Stderr = cmd.OutOrStdout(), os.Stderr
				if err := v.Run(); err != nil {
					return fmt.Errorf("config invalid — NOT restarting; the running instance is untouched")
				}
			}
			return systemctl("restart", unit).Run()
		},
	}
	restart.Flags().StringVar(&rname, "name", service.DefaultName, "unit name (without .service)")
	restart.Flags().StringVar(&rcfg, "config", "", "config to validate (default: the one recorded in the unit)")
	return append(cmds, restart)
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
