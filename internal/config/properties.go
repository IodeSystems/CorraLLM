// Package config holds two layers of configuration:
//
//   - properties.go: the layered .properties loader for process env / secrets
//     (operator knobs: ADDR, DB path, log level). Same standard as redline2.
//   - config.go: the corrallm YAML domain config (servers, models,
//     priorityGroups, costs) — the scheduler's source of truth.
//
// .properties feed os.Getenv; the YAML file is parsed into typed structs.
package config

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileOrder lists the property files for a service, LOW→HIGH precedence (later
// overrides earlier): base defaults, shared secrets, then the per-service tiers.
// All are optional.
func fileOrder(service string) []string {
	return []string{
		"application.properties",       // base defaults (committed)
		"secret.properties",            // shared secrets (gitignored)
		service + ".properties",        // per-service config (committed; e.g. ADDR)
		service + ".local.properties",  // per-service local override (gitignored)
		service + ".secret.properties", // per-service secret override (gitignored)
	}
}

// LoadProperties merges the layered property files for service under home.
// Missing files are skipped; the result is the effective config map.
func LoadProperties(home, service string) (map[string]string, error) {
	merged := map[string]string{}
	for _, name := range fileOrder(service) {
		m, err := loadFile(filepath.Join(home, name))
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			merged[k] = v
		}
	}
	return merged, nil
}

// LoadInto loads the layered config and applies it to the process environment,
// but ONLY for keys not already set — so env (a container, the slot unit)
// overrides the files. Returns the number of keys applied. A missing home dir is
// fine (0 applied): dev/container runs with no property files use env only.
func LoadInto(home, service string) (int, error) {
	m, err := LoadProperties(home, service)
	if err != nil {
		return 0, err
	}
	n := 0
	for k, v := range m {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func loadFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

// parse reads a Java-properties-lite file: `key=value` or `key: value` lines,
// `#` or `!` comments, blank lines ignored, surrounding whitespace trimmed.
func parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		sep := strings.IndexAny(line, "=:")
		if sep < 0 {
			continue
		}
		key := strings.TrimSpace(line[:sep])
		val := strings.TrimSpace(line[sep+1:])
		if key == "" {
			continue
		}
		out[key] = val
	}
	return out, sc.Err()
}
