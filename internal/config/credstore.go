package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// CredentialsFileName is the secret store beside the managed config.
//
// It is a SEPARATE file from config.yml on purpose. /api/v1/config/* serves the
// YAML verbatim — that is how the dashboard's editor works, and how every
// config in this repo's own plan docs got quoted — so a secret written there is
// a secret served over HTTP, backed up, and pasted into chat windows. Keeping
// them apart is what lets the config stay safe to read and share, which it is
// today and should remain.
//
// Format is the same `key=value` properties-lite the operator knobs use, so
// there is one parser and one mental model. Referenced from config the way it
// always has been:
//
//	headers: {authorization: "Bearer ${OPENROUTER_KEY_WORK}"}
//
// That indirection is deliberate: no new schema, nothing to migrate, and a
// credential added by a UI is a line in this file rather than a change to the
// document the API hands out.
const CredentialsFileName = "credentials"

var (
	secretsMu sync.RWMutex
	secrets   map[string]string
)

// LoadCredentials reads the credential store under home and installs it as the
// first source consulted by ${...} expansion.
//
// Missing is fine — every deployment that predates this keeps resolving from
// the process environment exactly as before, which is what makes this additive.
//
// REFUSES a group- or world-readable file, the way ssh refuses a private key,
// rather than loading it with a warning nobody reads. Failing here is loud and
// recoverable (`chmod 600`); loading is silent and permanent.
func LoadCredentials(home string) error {
	path := filepath.Join(home, CredentialsFileName)
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("credentials file %s is mode %04o: it holds secrets and must not be readable by group or others — chmod 600 %s",
			path, perm, path)
	}
	m, err := loadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	secretsMu.Lock()
	secrets = m
	secretsMu.Unlock()
	// A shadowed variable is the one confusing outcome here: the operator edits
	// the file, nothing changes, and there is no way to tell why. Name them.
	for k := range m {
		if _, ok := os.LookupEnv(k); ok {
			slog.Warn("credentials file shadows an environment variable of the same name; the file wins",
				"key", k, "path", path)
		}
	}
	slog.Info("credentials loaded", "path", path, "keys", len(m))
	return nil
}

// lookupSecret resolves one ${NAME}: the credential store first, then the
// process environment.
//
// File-first because the store is the DELIBERATE, managed source — a value put
// there by an operator or a UI must take effect, and env-first would let an
// ambient variable silently win over the thing someone just typed. Shadowing is
// warned about at load, so the precedence is discoverable rather than folklore.
func lookupSecret(name string) string {
	secretsMu.RLock()
	v, ok := secrets[name]
	secretsMu.RUnlock()
	if ok {
		return v
	}
	return os.Getenv(name)
}

// SecretNames lists the keys the store provides, for diagnostics. Values are
// never returned — nothing in corrallm has a reason to read one back out, and a
// getter is how they end up in a log line.
func SecretNames() []string {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	out := make([]string, 0, len(secrets))
	for k := range secrets {
		out = append(out, k)
	}
	return out
}
