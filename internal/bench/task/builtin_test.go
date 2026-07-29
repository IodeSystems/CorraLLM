package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The library must travel IN the binary. It previously came from a relative
// "probes" directory resolved against the runner's cwd — and the daemon that
// spawns llm-bench runs from a different directory entirely, so the default was
// wrong for the only caller that mattered and had to be papered over with an
// absolute --bench-probes path.
func TestBuiltinsResolveWithNoDirectory(t *testing.T) {
	refs, err := ResolveProbes("", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	if len(refs) < 15 {
		t.Fatalf("resolved %d built-in probes, want the whole library", len(refs))
	}
	for _, r := range refs {
		if r.Source != SourceBuiltin {
			t.Errorf("%s: source = %q, want builtin", r.Dir, r.Source)
		}
	}
}

// Fixtures are the reason embedding is not trivial: go:embed refuses to descend
// into a directory containing go.mod, so a fixture's module file is stored
// under another name. If materialization ever stops carrying fixtures, probes
// still LOAD (the directory exists) but every workspace seeds empty — a silent
// wrong answer, which is why this asserts on file content and not just count.
func TestBuiltinFixturesMaterializeWholly(t *testing.T) {
	root, err := MaterializeBuiltins(t.TempDir())
	if err != nil {
		t.Fatalf("MaterializeBuiltins: %v", err)
	}
	fix := filepath.Join(root, "fix-failing-test", "_fixture")
	for _, f := range []string{"gomod.probe", "mathx.go", "mathx_test.go"} {
		b, err := os.ReadFile(filepath.Join(fix, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s materialized empty", f)
		}
	}
	// The module file must NOT be a literal go.mod at rest, or the embed that
	// put it here could not have run.
	if _, err := os.Stat(filepath.Join(fix, "go.mod")); err == nil {
		t.Error("fixture carries a literal go.mod at rest — go:embed cannot descend into that")
	}
}

// Every built-in must load. A library shipped inside the binary cannot be
// fixed by editing a file next to the deployment.
func TestEveryBuiltinProbeLoads(t *testing.T) {
	entries, err := Catalog("", t.TempDir())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	classes := map[string]int{}
	for _, e := range entries {
		if e.Error != "" {
			t.Errorf("%s: %s", e.Dir, e.Error)
			continue
		}
		if e.Class == "" {
			t.Errorf("%s: no class", e.Dir)
		}
		classes[e.Class]++
	}
	// The four tiers the library is built around; losing one silently would
	// mean a whole dimension stopped being measured.
	for _, c := range []string{"capability", "tooluse", "coding", "adversarial"} {
		if classes[c] == 0 {
			t.Errorf("no %s probes in the catalog", c)
		}
	}
}

// A broken probe must appear IN the catalog, carrying its error. Dropping it
// would reproduce exactly the silence the catalog exists to end: a probe that
// never loaded would look identical to one that was never written.
func TestCatalogReportsUnloadableProbes(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken-probe")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "task.yaml"), []byte("name: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Catalog(dir, t.TempDir())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want just the broken one (a user dir REPLACES the built-ins)", len(entries))
	}
	if entries[0].Error == "" {
		t.Fatal("the unparseable probe was reported as healthy")
	}
}

// A user probe reusing a built-in's name is an override, and must say so —
// otherwise "why am I not running the probe I wrote?" has no visible answer.
func TestUserProbeShadowingABuiltinIsLabelled(t *testing.T) {
	dir := t.TempDir()
	name := BuiltinNames()[0]
	src := filepath.Join(dir, name)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "task.yaml"), []byte("name: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := ResolveProbes(dir, t.TempDir())
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	if len(refs) != 1 || refs[0].Source != SourceOverride {
		t.Fatalf("refs = %+v, want one entry marked %q", refs, SourceOverride)
	}
	if !strings.Contains(refs[0].Path, dir) {
		t.Errorf("resolved to %q, want the user copy", refs[0].Path)
	}
}
