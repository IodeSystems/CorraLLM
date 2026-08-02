package task

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/corrallm/probes"
)

// The built-in probe library, and the catalog of what is loadable.
//
// Two problems solved here, and they are the same problem seen from either end.
// A RUNNER needs the built-ins to exist as real files (fixtures are copied into
// a workspace; the MCP server jails the agent inside a real directory), so the
// embedded tree is materialized to disk on demand. A CALLER needs to know what
// probes exist WITHOUT running them — corrallm could report every result a run
// produced but could not answer "what could I run?", so a probe that was never
// picked up looked identical to one that ran and passed nothing.

// BuiltinDirName is where MaterializeBuiltins writes, under the given root.
const BuiltinDirName = "builtin-probes"

// MaterializeBuiltins writes the embedded probe library into root/builtin-probes
// and returns that path, ready to hand to a loader as a tasks directory.
//
// Idempotent by content: a probe directory that already exists with the right
// bytes is left alone, so repeated runs in a persistent workspace do not churn
// the tree (and do not disturb a fixture a previous run is still reading).
//
// It also PRUNES what is no longer embedded. Writing without pruning made a
// deleted probe immortal: the extraction is long-lived (an OS temp dir that
// survives every run on the box), so a probe removed from the library kept
// being resolved as a built-in and kept running, on every machine that had ever
// benched. Found by deleting four probes and watching them still execute — the
// library is only the source of truth if removal propagates too.
func MaterializeBuiltins(root string) (string, error) {
	dst := filepath.Join(root, BuiltinDirName)
	want := map[string]bool{}
	err := fs.WalkDir(probes.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// probes.go is the embed declaration, not a probe.
		if p == "." || p == "probes.go" {
			return nil
		}
		out := filepath.Join(dst, p)
		want[filepath.Clean(out)] = true
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := probes.FS.ReadFile(p)
		if err != nil {
			return err
		}
		if cur, err := os.ReadFile(out); err == nil && string(cur) == string(b) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("materialize built-in probes: %w", err)
	}
	if err := pruneUnembedded(dst, want); err != nil {
		return "", fmt.Errorf("materialize built-in probes: %w", err)
	}
	return dst, nil
}

// pruneUnembedded deletes anything under dst that the embedded library no
// longer contains. Scoped to dst, which MaterializeBuiltins owns outright —
// nothing else writes there, so a path that is not embedded is a leftover.
//
// Deepest-first, so a directory is considered only after its contents are gone.
// A prune failure is reported rather than swallowed: silently keeping a stale
// probe is the exact failure this exists to end.
func pruneUnembedded(dst string, want map[string]bool) error {
	var stale []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing materialized yet
			}
			return err
		}
		if filepath.Clean(p) == filepath.Clean(dst) || want[filepath.Clean(p)] {
			return nil
		}
		stale = append(stale, p)
		if d.IsDir() {
			return fs.SkipDir // its contents go with it
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := len(stale) - 1; i >= 0; i-- {
		if err := os.RemoveAll(stale[i]); err != nil {
			return err
		}
	}
	return nil
}

// BuiltinNames lists the probe directories carried in the binary, without
// touching the filesystem. Cheap enough to call for a catalog request.
func BuiltinNames() []string {
	ents, err := fs.ReadDir(probes.FS, ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// Source says where a catalog entry came from, because the answer changes what
// you do about it: a broken built-in is our bug, a broken user probe is theirs,
// and a user probe that SHADOWS a built-in is neither — it is a deliberate
// override that should be visible rather than surprising.
type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"
	// SourceOverride is a user probe whose name matches a built-in. The user
	// copy wins, exactly as it does at run time.
	SourceOverride Source = "override"
)

// CatalogEntry is one loadable probe, described without running it.
type CatalogEntry struct {
	// Dir is the directory name — the identity on disk, and therefore what
	// shadowing is resolved on. Name is what the probe CALLS itself, which is
	// what results are recorded under. They are usually equal; a catalog that
	// assumed so would mis-report an override.
	Dir    string `json:"dir"`
	Name   string `json:"name"`
	Class  string `json:"class"`
	Source Source `json:"source"`
	// Summary is a one-line gloss for list views.
	Summary string `json:"summary,omitempty"`
	// Description is the probe's FULL prose — what it seeds, what it asks, what
	// the checks assert and how to read a failure. Markdown.
	//
	// Separate from Summary because they answer different questions and a
	// catalog needs both: a list of twenty probes wants one line each, and the
	// reader who stops on one of them wants the whole thing. Truncating to the
	// summary was not a display choice, it was data loss — the prose existed in
	// the probe and had no way to reach a reader.
	Description string `json:"description,omitempty"`
	Run         string `json:"run,omitempty"`      // "", "warm"
	Requires    string `json:"requires,omitempty"` // effective capability, when the probe demands one
	Checks      int    `json:"checks"`
	Stages      int    `json:"stages"`
	// Error is set when the probe FAILED to load. Such an entry is still
	// returned: a probe that cannot be parsed is precisely what a catalog is
	// for, and dropping it would reproduce the silence this endpoint exists to
	// end.
	Error string `json:"error,omitempty"`
}

// ProbeRef is one resolved probe directory, before it is parsed.
type ProbeRef struct {
	Dir    string // directory name (the identity)
	Path   string // absolute path to load from
	Source Source
}

// ResolveProbes is THE rule for which probes exist, and both the runner and the
// catalog go through it — a catalog that resolved differently from the runner
// would be a confident lie about what is about to run.
//
// The rule: userDirs REPLACE the built-in library when given, and the built-ins
// are what you get when none are.
//
// Replace, not merge. Merging was tried first and is wrong twice over: a caller
// who points at three probes of their own means those three, not those three
// plus twenty of ours; and it makes every built-in a dependency of every run, so
// one malformed probe in the library fails a run that never asked for it — which
// is exactly how a latent `capability-tts` bug turned into four failing tests.
//
// So `--tasks-dir` is an OVERRIDE, which is what it has always read as. What
// changed is only that it is no longer REQUIRED: with no flag the library comes
// from the binary rather than from a directory that happens to be next to the
// working directory.
//
// SEVERAL dirs, because a probe belongs to whatever it measures, not to this
// repo. A tool with its own probes keeps them in its own tree and REFERENCES
// the directory here; changing them there changes what runs here, with nothing
// to copy and nothing to keep in sync. Union across the named dirs — that is
// still "those probes and no others", just sourced from more than one place, so
// neither reason above is weakened.
//
// Later dirs win a name collision, matching the flag order the caller wrote,
// and the winner is labelled an override so a shadowed probe is visible rather
// than surprising. A name that also exists in the built-in library is labelled
// the same way even though the built-ins are not in play: the caller who reads
// it is asking "why is this not the probe I wrote", and the answer is the same.
//
// tmpRoot is where the embedded library is materialized so it can be read.
func ResolveProbes(userDirs []string, tmpRoot string) ([]ProbeRef, error) {
	dirs, src := userDirs, SourceUser
	if len(dirs) == 0 {
		root, err := MaterializeBuiltins(tmpRoot)
		if err != nil {
			return nil, err
		}
		dirs, src = []string{root}, SourceBuiltin
	}
	builtin := map[string]bool{}
	for _, n := range BuiltinNames() {
		builtin[n] = true
	}
	// byDir keeps ONE ref per probe name so a later dir can shadow an earlier
	// one; order is restored by the sort below, so map iteration never leaks
	// into the result.
	byDir := map[string]ProbeRef{}
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if !isProbeDir(p) {
				continue
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			s := src
			// A user probe that reuses a built-in's name, or one already
			// claimed by an earlier dir, is reported as an override.
			if s == SourceUser {
				if _, shadowed := byDir[e.Name()]; shadowed || builtin[e.Name()] {
					s = SourceOverride
				}
			}
			byDir[e.Name()] = ProbeRef{Dir: e.Name(), Path: abs, Source: s}
		}
	}
	out := make([]ProbeRef, 0, len(byDir))
	for _, r := range byDir {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// SplitProbeDirs parses a comma-separated probe-directory list — the form a
// flag, an env var and a YAML scalar can all carry unchanged. Blank entries are
// dropped so a trailing comma or an unset segment is not read as "the current
// directory", which would silently pull in whatever the process happens to be
// sitting in.
func SplitProbeDirs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Catalog describes every probe ResolveProbes finds, without running any.
func Catalog(userDirs []string, tmpRoot string) ([]CatalogEntry, error) {
	refs, err := ResolveProbes(userDirs, tmpRoot)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(refs))
	for _, r := range refs {
		t, err := LoadDir(r.Path)
		if err != nil {
			out = append(out, CatalogEntry{Dir: r.Dir, Name: r.Dir, Source: r.Source, Error: err.Error()})
			continue
		}
		out = append(out, describe(t, r.Dir, r.Source))
	}
	return out, nil
}

func isProbeDir(p string) bool {
	for _, f := range []string{"task.yaml", ProbeFile} {
		if _, err := os.Stat(filepath.Join(p, f)); err == nil {
			return true
		}
	}
	return false
}

func describe(t *Task, name string, src Source) CatalogEntry {
	e := CatalogEntry{
		Dir:      name,
		Name:     name,
		Class:    t.Class,
		Source:   src,
		Run:      t.Run,
		Requires: t.Requires.EffectiveCapability(),
		Stages:   len(t.Stages),
	}
	if t.Name != "" {
		e.Name = t.Name
	}
	for _, s := range t.Stages {
		e.Checks += len(s.Checks)
	}
	e.Description = strings.TrimSpace(t.Description)
	if e.Summary == "" {
		e.Summary = firstLine(t.Description)
	}
	return e
}

// firstLine reduces a description to a one-line gloss.
//
// It skips MARKDOWN FURNITURE — headings, blank lines, list and table markers,
// block quotes — and returns the first line of actual prose. A description that
// opens with "## What this measures" is well-formed, and taking its literal
// first line would put that heading in every list view as though it were the
// summary of the probe.
func firstLine(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		// Headings, list bullets, table rows/separators, quotes: structure, not
		// a sentence.
		if strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "|") ||
			strings.HasPrefix(ln, ">") || strings.HasPrefix(ln, "- ") ||
			strings.HasPrefix(ln, "* ") || strings.HasPrefix(ln, "---") {
			continue
		}
		if len(ln) > 160 {
			ln = ln[:160]
		}
		return strings.TrimSpace(ln)
	}
	return ""
}
