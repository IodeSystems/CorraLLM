package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

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
	}
	// RunAtLoad starts it.
	if err := launchctl("bootstrap", launchdDomain(), p).Run(); err != nil {
		return fmt.Errorf("bootstrap %s: %w", p, err)
	}
	return nil
}

func launchdInstall(out io.Writer, u service.Unit, enable, start bool) error {
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
		if launchdLoaded(name) {
			return launchctl("kickstart", "-k", launchdTarget(name)).Run()
		}
		return launchdLoad(name)
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
