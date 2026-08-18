package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A fresh install writes NO config file.
//
// It used to write an empty managed one, because the API's requireManaged check
// demanded a file carrying a "MANAGED CONFIG" marker before it would accept an
// edit. Config lives in the database now, so there is nothing to mark — and a
// file created here would be imported and immediately retired by the boot path,
// which is churn that looks like a bug.
func TestFreshInstallWritesNoConfigFile(t *testing.T) {
	home := t.TempDir()
	p := derivePaths(home, "", "")
	if _, err := os.Stat(p.config); !os.IsNotExist(err) {
		t.Errorf("a config file exists on a fresh install (%v); config belongs in the database", err)
	}
}
