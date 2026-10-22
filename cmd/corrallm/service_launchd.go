package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/service"
)

// useLaunchd: `corrallm service` drives launchd on macOS and systemd
// everywhere else. The verbs are the same; what each one maps to is below.
func useLaunchd() bool { return runtime.GOOS == "darwin" }

func launchctl(args ...string) *exec.Cmd {
	c := exec.Command("launchctl", args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c
}

// launchdDomain is the per-user GUI domain: a LaunchAgent there runs in the
// user's login session, which is where macOS grants Local Network access.
func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchdTarget(name string) string {
	return launchdDomain() + "/" + service.Unit{Name: name}.Label()
}

func launchdPlistPath(name string) (string, error) {
	dir, err := service.LaunchAgentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, service.Unit{Name: name}.PlistFileName()), nil
}

// launchdLoaded reports whether the job is in the domain (running or not).
func launchdLoaded(name string) bool {
	return exec.Command("launchctl", "print", launchdTarget(name)).Run() == nil
}

// launchdLoad (re)loads the plist so an edit takes effect. Unlike systemd's
// daemon-reload, a loaded launchd job keeps its old definition until it is
// booted out, and booting out stops it — so this is a restart when running.
func launchdLoad(name string) error {
	p, err := launchdPlistPath(name)
	if err != nil {
		return err
	}
	if launchdLoaded(name) {
		if err := launchctl("bootout", launchdTarget(name)).Run(); err != nil {
			return fmt.Errorf("bootout: %w", err)
		}
		// bootout can return while the job is still leaving the domain (a
		// draining serve takes up to its ExitTimeOut), and a bootstrap in that
		// window fails with error 5 like any other.
		for deadline := time.Now().Add(100 * time.Second); launchdLoaded(name) && time.Now().Before(deadline); {
			time.Sleep(250 * time.Millisecond)
		}
	}
	// RunAtLoad starts it. launchctl's stderr is captured: on failure it says
	// "Try re-running the command as root for richer errors", which is wrong
	// for a per-user LaunchAgent — as root the domain is gui/0, not yours.
	var stderr strings.Builder
	c := exec.Command("launchctl", "bootstrap", launchdDomain(), p)
	c.Stdout, c.Stderr = os.Stdout, &stderr
	if err := c.Run(); err != nil {
		if launchdLoaded(name) {
			// Raced into the domain anyway (an at-login load, a second
			// command): it is loaded, which is what was asked.
			return nil
		}
		msg := strings.TrimSpace(strings.ReplaceAll(stderr.String(), "Try re-running the command as root for richer errors.", ""))
		return fmt.Errorf("launchctl bootstrap %s %s: %s (%w)\n%s", launchdDomain(), p, msg, err, launchdDiagnose(name, p))
	}
	return nil
}

// launchdDiagnose names what is known to make a bootstrap fail. launchctl
// reports nearly all of them as "5: Input/output error".
func launchdDiagnose(name, plist string) string {
	var b strings.Builder
	b.WriteString("Do not rerun as root: this is a per-user LaunchAgent. Checked:\n")
	if out, err := exec.Command("plutil", "-lint", plist).CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "  - the plist does not parse: %s\n", strings.TrimSpace(string(out)))
	} else {
		b.WriteString("  - the plist parses\n")
	}
	label := service.Unit{Name: name}.Label()
	if out, err := exec.Command("launchctl", "print-disabled", launchdDomain()).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, `"`+label+`"`) && (strings.Contains(line, "disabled") || strings.Contains(line, "true")) {
				fmt.Fprintf(&b, "  - the job is DISABLED: run `launchctl enable %s`, then start again\n", launchdTarget(name))
			}
		}
	}
	if os.Getenv("SSH_CONNECTION") != "" {
		b.WriteString("  - this is an ssh session: the gui domain exists only while you are logged in at the Mac's screen\n")
	}
	b.WriteString("  - System Settings → General → Login Items & Extensions: \"Allow in the Background\" must be on for corrallm\n")
	if lp, err := service.LaunchdLogPath(name); err == nil {
		fmt.Fprintf(&b, "  - the job's own output: %s\n", lp)
	}
	return strings.TrimRight(b.String(), "\n")
}

// launchdRefuseRoot: as root, gui/<uid> is gui/0 and ~ is root's home, so
// every command would act on a job that does not exist.
func launchdRefuseRoot() error {
	if os.Getuid() == 0 {
		return fmt.Errorf("run `corrallm service` as your own user, not root: the LaunchAgent is per-user (gui/<your uid>, ~/Library/LaunchAgents)")
	}
	return nil
}

func launchdInstall(out io.Writer, u service.Unit, enable, start bool) error {
	if err := launchdRefuseRoot(); err != nil {
		return err
	}
	name := u.Name
	dir, err := service.LaunchAgentsDir()
	if err != nil {
		return err
	}
	logPath, err := service.LaunchdLogPath(name)
	if err != nil {
		return err
	}
	path, err := u.InstallPlist(dir, logPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", path)
	fmt.Fprintf(out, "  exec   %s\n", u.Exec)
	fmt.Fprintf(out, "  logs   %s\n", logPath)
	if u.ConfigPath != "" {
		fmt.Fprintf(out, "  config %s (validated by `corrallm service restart`; launchd has no pre-start step)\n", u.ConfigPath)
	}
	if enable {
		if err := launchctl("enable", launchdTarget(name)).Run(); err != nil {
			return fmt.Errorf("enable: %w", err)
		}
		fmt.Fprintf(out, "enabled at login\n")
	}
	if start {
		if err := launchdLoad(name); err != nil {
			return err
		}
		fmt.Fprintf(out, "started\n")
	}
	if !enable && !start {
		fmt.Fprintf(out, "not loaded; start it with: corrallm service start --name %s\n", name)
	}
	return nil
}

func launchdUninstall(out io.Writer, name string) error {
	if err := launchdRefuseRoot(); err != nil {
		return err
	}
	if launchdLoaded(name) {
		_ = launchctl("bootout", launchdTarget(name)).Run()
	}
	p, err := launchdPlistPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", p)
	return nil
}

// launchdCtl maps the systemctl passthrough verbs.
func launchdCtl(action, name string) error {
	if err := launchdRefuseRoot(); err != nil {
		return err
	}
	switch action {
	case "start":
		if launchdLoaded(name) {
			return launchctl("kickstart", launchdTarget(name)).Run()
		}
		return launchdLoad(name)
	case "stop":
		// bootout, not kill: KeepAlive would restart a job that exits
		// non-zero, and a stopped job should stay stopped until start.
		if !launchdLoaded(name) {
			return nil
		}
		return launchctl("bootout", launchdTarget(name)).Run()
	case "restart":
		// Reload, not `kickstart -k`: kickstart restarts the definition
		// launchd already holds, so a reinstalled plist would not apply.
		return launchdLoad(name)
	case "reload":
		// launchd signals the job's main process only — the property
		// systemd needs ExecReload=/bin/kill -HUP $MAINPID for.
		return launchctl("kill", "HUP", launchdTarget(name)).Run()
	case "status":
		return launchctl("print", launchdTarget(name)).Run()
	case "logs":
		p, err := service.LaunchdLogPath(name)
		if err != nil {
			return err
		}
		t := exec.Command("tail", "-n", "200", p)
		t.Stdout, t.Stderr = os.Stdout, os.Stderr
		return t.Run()
	}
	return fmt.Errorf("unknown action %q", action)
}

// launchdConfigPath recovers the --config the job was installed with.
func launchdConfigPath(name string) string {
	p, err := launchdPlistPath(name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return flagValue(service.PlistArgs(string(b)), "--config")
}
