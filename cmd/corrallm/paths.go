package main

import (
	"os"
	"path/filepath"
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

// bootstrapConfig is GONE, and its reason with it.
//
// It used to write an empty managed config so a fresh install had a file with
// the "MANAGED CONFIG" marker in it — without which requireManaged made the
// dashboard answer 409 to the very first edit. Config lives in the database
// now: there is no file to mark, and no marker to check, because nothing else
// writes the tables. Creating one would be worse than useless, since the boot
// path would import the empty file and immediately retire it.
