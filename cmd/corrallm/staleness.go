package main

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stale-binary warning.
//
// The trap this exists for, seen on this box in raglit: a binary twenty commits
// behind sat on PATH shadowing a current one and answered a real command with
// "unknown command" — a feature that existed, in a build nobody had replaced.
// The same is available here every time someone edits corrallm and forgets to
// reinstall before restarting the service.
//
// It WARNS and does nothing else. corrallm deliberately does not rebuild or
// restart itself, and the reason is the one its own agent already documents:
// this process supervises backend process groups holding tens of GB. raglit
// hot-reloads with syscall.Exec, which keeps the PID and the cgroup — but it
// also skips Shutdown, so here it would leave every llama-server and oidio
// orphaned, untracked and still holding VRAM the new image believes is free.
// internal/proc/manager.go records what that looks like: "a survivor is
// untracked, unkillable by any later eviction… every subsequent spawn then dies
// with a cudaMalloc OOM".
//
// So the safe reload is a full stop that drains and reaps — `make install &&
// corrallm service restart` — and the only thing missing was noticing that you
// needed it.

// srcDir is the module directory, stamped by `make build|install|dist`. Empty
// for a plain `go install` or a released build, which disables the check: a
// binary shipped elsewhere has no source tree to compare against, and a warning
// naming a directory that does not exist on that machine is worse than silence.
var srcDir = ""

// warnIfStale logs once at startup when the source tree is newer than this
// binary. Never fatal, never blocking: being out of date is a nuisance, not a
// reason to refuse to serve.
func warnIfStale() { warnIfStaleFor("serve") }

// warnIfStaleFor is warnIfStale told which command is running, so it can say
// what being stale actually COSTS here.
//
// The two are different failures and want different sentences. A stale daemon
// serves yesterday's behaviour, which is a nuisance you notice. A stale CLI
// decodes the stored config into ITS OWN structs and silently drops every field
// it has never heard of — so it answers confidently with a config that is
// missing settings the daemon is serving from, which is the kind of answer
// nobody double-checks. Telling someone to restart the service would not have
// fixed that, and pointed away from the real cause.
func warnIfStaleFor(cmdName string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := srcDir
	if dir == "" {
		// Not stamped. That is either a released build — no source tree to
		// compare against, stay silent — or a plain `go build -o bin/corrallm`,
		// which is how the trap this whole file exists for actually got built.
		// Telling them apart is one question: is this binary sitting inside a
		// module? A release installed to /usr/local/bin is not; a hand-built one
		// in ./bin is.
		dir = moduleRootAbove(exe)
		if dir == "" {
			return
		}
	}
	st, err := os.Stat(exe)
	if err != nil {
		return
	}
	newest, name := newestBuildInput(dir, st.ModTime())
	if newest.IsZero() {
		return
	}
	msg, fix := "this binary predates the source tree — restarting will not pick up your changes",
		"make install && corrallm service restart"
	if cmdName != "serve" {
		msg = "this CLI predates the source tree — it may DROP stored settings it does not know about"
		fix = "make build"
	}
	slog.Warn(msg,
		"command", cmdName,
		"built", st.ModTime().Format(time.RFC3339),
		"newest_source", newest.Format(time.RFC3339),
		"file", name,
		"fix", fix)
}

// newestBuildInput returns the modtime and path of the newest file under dir
// that affects the build, or the zero time when nothing is newer than t.
func newestBuildInput(dir string, t time.Time) (time.Time, string) {
	var newest time.Time
	var which string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		// An unreadable corner must not be reported as staleness — the check is
		// advisory, and a false alarm trains people to ignore it.
		if err != nil {
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "out", "local", "bin", "tmp":
				return filepath.SkipDir
			}
			return nil
		}
		if !buildInput(d.Name()) {
			return nil
		}
		info, e := d.Info()
		if e != nil || !info.ModTime().After(t) {
			return nil
		}
		if info.ModTime().After(newest) {
			newest, which = info.ModTime(), p
		}
		return nil
	})
	if which != "" {
		if rel, err := filepath.Rel(dir, which); err == nil {
			which = rel
		}
	}
	return newest, which
}

// buildInput reports whether a filename affects the compiled binary.
//
// The UI is deliberately NOT included: it is served from --web-root as files on
// disk, not compiled in, so a changed .tsx does not make this binary stale and
// flagging it would cry wolf on every dashboard edit.
func buildInput(name string) bool {
	switch name {
	case "go.mod", "go.sum", "go.work":
		return true
	}
	return strings.HasSuffix(name, ".go")
}

// moduleRootAbove finds the module directory a binary is sitting in, or "" when
// it is not in one.
//
// The stamped srcDir is better and is preferred: it says where the binary was
// BUILT from, which is the honest question. This is the fallback for a binary
// built without the Makefile, where the only evidence available is where it
// landed — good enough, because the case it catches is `go build -o bin/x` in
// the tree you are editing.
//
// Walks up rather than assuming ./bin, so a binary built to the module root or
// any subdirectory of it is still checked. Stops at the filesystem root.
func moduleRootAbove(exe string) string {
	dir := filepath.Dir(exe)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
