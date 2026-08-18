// Package recipes carries the tool build/probe scripts as embedded data.
//
// It is a leaf on purpose: it imports nothing of corrallm's, so both
// internal/config (which validates that a declared `recipe:` exists) and
// internal/toolchain (which runs it) can depend on it without a cycle.
//
// WHY THE SCRIPTS ARE EMBEDDED. The agent is the same corrallm binary
// cross-compiled, and it already self-updates on a build-id mismatch. Embedding
// means recipes version with the agent and reach every host over a path that is
// already proven — no second distribution channel to build, secure and debug.
// The agent extracts them to its state dir and runs them from there.
//
// Consequence worth knowing: the AGENT's embedded copy is what runs, not the
// primary's. Between a primary upgrade and an agent's self-update the two can
// differ, and the older recipe is the one that answers. Self-update converges
// them within a heartbeat or two.
package recipes

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// extractMu serialises Extract against itself.
//
// A survey runs every (tool, host) pair CONCURRENTLY and each one extracts, so
// without this several goroutines rewrite the same directory at once. The
// atomic rename below is what makes a reader safe; this only keeps them from
// doing redundant work and racing each other's temp files.
var extractMu sync.Mutex

//go:embed *.sh
var files embed.FS

// commonName is the shared helper library, sourced by every recipe. It is not
// itself a recipe and must never appear in Names().
const commonName = "common.sh"

// Names lists the tools that have a recipe, e.g. "llama.cpp", "ninfer".
func Names() []string {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if n == commonName || !strings.HasSuffix(n, ".sh") {
			continue
		}
		out = append(out, strings.TrimSuffix(n, ".sh"))
	}
	sort.Strings(out)
	return out
}

// Has reports whether a recipe exists for this name.
func Has(name string) bool {
	if name == "" || name == strings.TrimSuffix(commonName, ".sh") {
		return false
	}
	_, err := files.Open(name + ".sh")
	return err == nil
}

// Extract writes every recipe into dir, returning the directory written.
//
// All of them, not just the one being run: a recipe sources common.sh from its
// own directory, so extracting one file alone produces a script that cannot
// start. Writing the set is also idempotent and cheap enough to do on every
// call, which keeps a stale extraction from outliving an agent update.
func Extract(dir string) error {
	extractMu.Lock()
	defer extractMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("recipe dir: %w", err)
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := files.ReadFile(e.Name())
		if err != nil {
			return err
		}
		// WRITE, THEN RENAME. os.WriteFile truncates before it writes, so a
		// bash already opening that path sees a half-written script — and since
		// a survey extracts from several goroutines while other recipes are
		// executing, that window is hit intermittently. It surfaced as a probe
		// returning unparseable output, which reads like a broken recipe rather
		// than a torn file. A rename is atomic, so a reader gets the whole old
		// file or the whole new one.
		//
		// 0o700: these are executed with whatever privileges the agent has. Not
		// group- or world-writable, or anyone on the box could rewrite what the
		// agent is about to run as itself.
		final := filepath.Join(dir, e.Name())
		tmp, err := os.CreateTemp(dir, "."+e.Name()+".*")
		if err != nil {
			return fmt.Errorf("write recipe %s: %w", e.Name(), err)
		}
		if _, err := tmp.Write(b); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("write recipe %s: %w", e.Name(), err)
		}
		if err := tmp.Chmod(0o700); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("chmod recipe %s: %w", e.Name(), err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("write recipe %s: %w", e.Name(), err)
		}
		if err := os.Rename(tmp.Name(), final); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("install recipe %s: %w", e.Name(), err)
		}
	}
	return nil
}

// Script returns one recipe's source, for tests and for `corrallm tools show`.
func Script(name string) ([]byte, error) {
	if !Has(name) {
		return nil, fmt.Errorf("no recipe %q (have: %s)", name, strings.Join(Names(), ", "))
	}
	return files.ReadFile(name + ".sh")
}
