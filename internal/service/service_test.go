package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderCarriesTheLaunchersGuarantees: each of these properties replaced
// something the shell launcher did, and dropping any one of them is a silent
// regression in production behaviour rather than a compile error.
func TestRenderCarriesTheLaunchersGuarantees(t *testing.T) {
	u := Unit{Exec: "/usr/bin/corrallm", ConfigPath: "/home/x/.corrallm/config.yml",
		Args: []string{"--addr", "0.0.0.0:8111"}}
	got := u.Render()

	for _, want := range []string{
		// A backend OOM must not take the gateway with it.
		"OOMPolicy=continue",
		// corrallm reaps its own backends; signalling the whole cgroup would
		// turn a graceful drain into a stampede.
		"KillMode=mixed",
		// A drain waits on in-flight work; too short strands VRAM.
		"TimeoutStopSec=90",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q:\n%s", want, got)
		}
	}
}

// TestRenderValidatesBeforeStart: the config is checked in ExecStartPre, because
// `serve` frees the listen port before it parses — restarting onto a broken
// config otherwise takes the gateway down instead of failing safe.
func TestRenderValidatesBeforeStart(t *testing.T) {
	u := Unit{Exec: "/usr/bin/corrallm", ConfigPath: "/etc/corrallm.yml", Args: []string{"--addr", ":1"}}
	got := u.Render()
	if !strings.Contains(got, "ExecStartPre=/usr/bin/corrallm validate --config /etc/corrallm.yml") {
		t.Errorf("no validating ExecStartPre:\n%s", got)
	}
	// ...and ExecStartPre must come before ExecStart, or it validates nothing.
	if strings.Index(got, "ExecStartPre=") > strings.Index(got, "ExecStart=") {
		t.Error("ExecStartPre appears after ExecStart")
	}

	// With no config there is nothing to validate and no pre-step at all.
	bare := Unit{Exec: "/usr/bin/corrallm"}.Render()
	if strings.Contains(bare, "ExecStartPre=") {
		t.Errorf("ExecStartPre emitted with no config:\n%s", bare)
	}
}

// TestRenderPassesServeArgsThrough: the unit must carry exactly the flags the
// operator would have typed.
func TestRenderPassesServeArgsThrough(t *testing.T) {
	u := Unit{Exec: "/usr/bin/corrallm", Args: []string{"--addr", "0.0.0.0:8111", "--web-root", "/srv/ui"}}
	got := u.Render()
	if !strings.Contains(got, "ExecStart=/usr/bin/corrallm serve --addr 0.0.0.0:8111 --web-root /srv/ui") {
		t.Errorf("serve args not passed through:\n%s", got)
	}
}

// TestRenderQuotesAwkwardArguments: systemd does its own unquoting, so a path
// with a space silently becomes two arguments — and a bare % is a specifier it
// would expand.
func TestRenderQuotesAwkwardArguments(t *testing.T) {
	u := Unit{
		Exec: "/opt/my apps/corrallm",
		Args: []string{"--web-root", "/srv/web root", "--bench-config", "/a/100%/x.yml"},
	}
	got := u.Render()
	if !strings.Contains(got, `"/opt/my apps/corrallm"`) {
		t.Errorf("exec path with a space not quoted:\n%s", got)
	}
	if !strings.Contains(got, `"/srv/web root"`) {
		t.Errorf("arg with a space not quoted:\n%s", got)
	}
	if !strings.Contains(got, "100%%") {
		t.Errorf("%% not escaped — systemd would expand it as a specifier:\n%s", got)
	}
}

// TestInstallRefusesRelativeExec: systemd does not search $PATH, so a relative
// ExecStart yields a unit that fails at start with a confusing error.
func TestInstallRefusesRelativeExec(t *testing.T) {
	if _, err := (Unit{Exec: "corrallm"}).Install(t.TempDir()); err == nil {
		t.Error("a relative exec path must be refused")
	} else if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want it to explain the absolute-path requirement", err)
	}
	if _, err := (Unit{}).Install(t.TempDir()); err == nil {
		t.Error("an empty exec path must be refused")
	}
}

// TestInstallWritesTheUnit round-trips a real install into a temp dir.
func TestInstallWritesTheUnit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "systemd", "user") // not yet created
	u := Unit{Exec: "/usr/bin/corrallm", Args: []string{"--addr", ":8111"}}
	p, err := u.Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "corrallm.service" {
		t.Errorf("wrote %q", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ExecStart=/usr/bin/corrallm serve --addr :8111") {
		t.Errorf("unexpected contents:\n%s", b)
	}
}

// TestUnitFileNameHonoursName lets a second instance run alongside the first.
func TestUnitFileNameHonoursName(t *testing.T) {
	if got := (Unit{}).UnitFileName(); got != "corrallm.service" {
		t.Errorf("default name = %q", got)
	}
	if got := (Unit{Name: "corrallm-staging"}).UnitFileName(); got != "corrallm-staging.service" {
		t.Errorf("named unit = %q", got)
	}
}

// TestRenderEmitsEnvironment: the listen address has no flag — `serve` reads
// ADDR from the environment. Passing --addr exits 1 with "unknown flag", and
// under Restart=on-failure that is a crash loop rather than a visible failure.
// This shipped once; the unit must be able to express the environment.
func TestRenderEmitsEnvironment(t *testing.T) {
	u := Unit{Exec: "/usr/bin/corrallm", Env: []string{"ADDR=0.0.0.0:8111", "CORRALLM_HOME=/srv/c"}}
	got := u.Render()
	for _, want := range []string{"Environment=ADDR=0.0.0.0:8111", "Environment=CORRALLM_HOME=/srv/c"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Environment must precede the ExecStart it configures? systemd does not
	// care about order within [Service], but it MUST be inside [Service] and
	// not leak into [Install].
	svc := got[strings.Index(got, "[Service]"):strings.Index(got, "[Install]")]
	if !strings.Contains(svc, "Environment=ADDR=") {
		t.Errorf("Environment landed outside [Service]:\n%s", got)
	}
}
