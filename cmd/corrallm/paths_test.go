package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// A bare home implies all three paths, so an operator who sets only --home does
// not end up with the config in one place and the database in another.
func TestDerivePathsFromHome(t *testing.T) {
	t.Setenv("CORRALLM_CONFIG", "")
	t.Setenv("CORRALLM_DB", "")

	p := derivePaths("/srv/corrallm", "", "")
	if got, want := p.config, filepath.Join("/srv/corrallm", "config.yml"); got != want {
		t.Errorf("config = %q, want %q", got, want)
	}
	if got, want := p.db, filepath.Join("/srv/corrallm", "var", "corrallm.db"); got != want {
		t.Errorf("db = %q, want %q", got, want)
	}
	if got, want := p.token, filepath.Join("/srv/corrallm", "admin.token"); got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
	if !p.configDerived {
		t.Error("configDerived = false, want true for a path corrallm chose itself")
	}
}

// Precedence: flag beats env beats home. Getting this backwards would point a
// running daemon at a different config than the one the operator validated.
func TestDerivePathsPrecedence(t *testing.T) {
	t.Setenv("CORRALLM_CONFIG", "/env/config.yml")
	t.Setenv("CORRALLM_DB", "/env/corrallm.db")

	p := derivePaths("/home/x", "/flag/config.yml", "/flag/corrallm.db")
	if p.config != "/flag/config.yml" {
		t.Errorf("config = %q, want the flag to win", p.config)
	}
	if p.db != "/flag/corrallm.db" {
		t.Errorf("db = %q, want the flag to win", p.db)
	}

	p = derivePaths("/home/x", "", "")
	if p.config != "/env/config.yml" {
		t.Errorf("config = %q, want the env var to win over home", p.config)
	}
	if p.db != "/env/corrallm.db" {
		t.Errorf("db = %q, want the env var to win over home", p.db)
	}
}

// configDerived is the permission to CREATE the file. A path the operator named
// must never be bootstrapped into a managed config — that is how a hand-written
// file would get a MANAGED header stamped on it and lose its comments.
func TestConfigNotDerivedWhenNamed(t *testing.T) {
	t.Setenv("CORRALLM_CONFIG", "")
	t.Setenv("CORRALLM_DB", "")
	if p := derivePaths("/home/x", "/named/corrallm.yaml", ""); p.configDerived {
		t.Error("configDerived = true for a --config path; must be false")
	}

	t.Setenv("CORRALLM_CONFIG", "/env/corrallm.yaml")
	if p := derivePaths("/home/x", "", ""); p.configDerived {
		t.Error("configDerived = true for a CORRALLM_CONFIG path; must be false")
	}
}

func TestDefaultHomeHonorsEnv(t *testing.T) {
	t.Setenv("CORRALLM_HOME", "/custom/home")
	if got := defaultHome(); got != "/custom/home" {
		t.Errorf("defaultHome() = %q, want the CORRALLM_HOME override", got)
	}
	t.Setenv("CORRALLM_HOME", "")
	if got := defaultHome(); !strings.HasSuffix(got, ".corrallm") {
		t.Errorf("defaultHome() = %q, want a path ending in .corrallm", got)
	}
}

// The bootstrap is what makes a fresh install writable at all: without a file
// carrying the managed marker, every config edit and every agent enrollment is
// refused with 409.
func TestBootstrapConfigWritesManagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yml")
	if err := bootstrapConfig(path); err != nil {
		t.Fatalf("bootstrapConfig: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bootstrapped config: %v", err)
	}
	if !strings.Contains(string(b), "MANAGED CONFIG") {
		t.Fatalf("bootstrapped config lacks the managed marker; the API would refuse to write it:\n%s", b)
	}
	// It must also load: writing something the daemon cannot parse would turn a
	// first run into a boot failure.
	if _, err := config.Load(path); err != nil {
		t.Fatalf("bootstrapped config does not load: %v", err)
	}
}

// Bootstrapping must never clobber. It runs on EVERY start, not just the first.
func TestBootstrapConfigLeavesExistingUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	const existing = "# MANAGED CONFIG\nmodels:\n  keep-me:\n    proxy: https://example.invalid/v1\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapConfig(path); err != nil {
		t.Fatalf("bootstrapConfig: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != existing {
		t.Fatalf("bootstrapConfig rewrote an existing config:\n%s", b)
	}
}
