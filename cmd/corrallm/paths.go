package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/iodesystems/corrallm/internal/config"
)

// corrallm roots everything it owns — the managed config, the SQLite store, the
// admin token, the layered .properties — under ONE home.
//
// It used to root them under three independent cwd-relative defaults (./home,
// ./corrallm.yaml, ./home/var/corrallm.db), which meant the identity of a
// running daemon depended on the directory it was launched from. Starting it
// from somewhere else silently produced a DIFFERENT daemon: fresh token, empty
// database, no models. One home makes "which instance is this" answerable by
// looking at a single path, and makes a raw `corrallm serve` land somewhere
// sensible instead of scattering state through the caller's working directory.

// defaultHome is the home used when neither --home nor CORRALLM_HOME says
// otherwise: a per-user directory, because a daemon's state is not a property
// of the checkout it happened to be started from.
//
// The ./home fallback is for the case where there is no user home to speak of
// (a scratch container, a daemon user with no passwd entry). Returning an error
// there would refuse to start over something that has a perfectly good answer.
func defaultHome() string {
	if p := os.Getenv("CORRALLM_HOME"); p != "" {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "./home"
	}
	return filepath.Join(h, ".corrallm")
}

// resolvedPaths is the set of on-disk locations one home implies.
type resolvedPaths struct {
	home   string
	config string
	db     string
	token  string
	// configDerived reports that config was computed from home rather than
	// named by --config or CORRALLM_CONFIG.
	//
	// It is the permission to CREATE that file. corrallm may freely bootstrap a
	// managed config at a path it chose itself; stamping a "MANAGED CONFIG"
	// header onto a path a human named is exactly what requireManaged exists to
	// prevent, so first-run bootstrap must know the difference.
	configDerived bool
}

// derivePaths resolves the config/db/token locations from home, letting an
// explicit flag or environment variable win over the derived default.
//
// Precedence, strongest first: the flag, the environment variable, then home.
// This mirrors what --tune-cache already does with the db directory.
func derivePaths(home, configFlag, dbFlag string) resolvedPaths {
	r := resolvedPaths{
		home:  home,
		token: filepath.Join(home, "admin.token"),
		db:    pick(dbFlag, envOr("CORRALLM_DB", filepath.Join(home, "var", "corrallm.db"))),
	}
	if cfg := pick(configFlag, os.Getenv("CORRALLM_CONFIG")); cfg != "" {
		r.config = cfg
		return r
	}
	r.config = filepath.Join(home, "config.yml")
	r.configDerived = true
	return r
}

// bootstrapConfig writes an empty MANAGED config when none exists yet.
//
// Without it a fresh install is readable but not writable. config.Load treats a
// missing file as an empty config, so the daemon boots and serves — but every
// write path (the dashboard's config editor, agent enrollment) goes through
// requireManaged, which reads the file and demands a "MANAGED CONFIG" marker.
// No file means no marker, so a brand-new instance answers 409 to the very
// first thing an operator tries to do with it. Creating the file up front is
// what makes the UI usable from minute one.
//
// Only ever called for a DERIVED path. A --config the operator named may be a
// hand-written file that does not exist yet, or a typo'd path; creating a
// managed file at either is worse than failing to.
func bootstrapConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	if err := config.Save(path, &config.Config{}); err != nil {
		return fmt.Errorf("bootstrap config: %w", err)
	}
	slog.Info("wrote an empty managed config", "path", path)
	return nil
}
