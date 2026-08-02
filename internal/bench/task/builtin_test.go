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
	refs, err := ResolveProbes(nil, t.TempDir())
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
	entries, err := Catalog(nil, t.TempDir())
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
	entries, err := Catalog([]string{dir}, t.TempDir())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	var broken *CatalogEntry
	for i := range entries {
		if entries[i].Dir == "broken-probe" {
			broken = &entries[i]
		}
	}
	if broken == nil {
		t.Fatalf("the broken probe is missing from %d entries", len(entries))
	}
	if broken.Error == "" {
		t.Fatal("the unparseable probe was reported as healthy")
	}
	// And it did not take the library down with it.
	if len(entries) != len(BuiltinNames())+1 {
		t.Errorf("got %d entries, want %d builtins + the broken one", len(entries), len(BuiltinNames()))
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
	refs, err := ResolveProbes([]string{dir}, t.TempDir())
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	var got *ProbeRef
	for i := range refs {
		if refs[i].Dir == name {
			got = &refs[i]
		}
	}
	if got == nil || got.Source != SourceOverride {
		t.Fatalf("%s = %+v, want it marked %q", name, got, SourceOverride)
	}
	if !strings.Contains(got.Path, dir) {
		t.Errorf("resolved to %q, want the user copy", got.Path)
	}
	// Shadowing ONE built-in must not displace the rest.
	if len(refs) != len(BuiltinNames()) {
		t.Errorf("resolved %d, want %d — an override replaces one probe, not the library",
			len(refs), len(BuiltinNames()))
	}
}

// probeDir writes a minimal loadable probe and returns nothing — the caller
// already knows where it put it.
func probeDir(t *testing.T, root, name, body string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "task.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// SEVERAL directories, because a probe belongs to whatever it measures. A tool
// keeps its probes in its own tree and the box references the directory, so
// editing them there changes what runs here with nothing to copy.
//
// The built-in library is ALWAYS present underneath. It used to be replaced by
// any named directory, which meant a box hosting three teams' libraries
// silently lost the capability probes — and the UI checkbox they drive — for
// adding a directory.
func TestResolveProbesOverlaysDirsOnTheBuiltins(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	probeDir(t, a, "alpha", "name: alpha\n")
	probeDir(t, b, "beta", "name: beta\n")

	refs, err := ResolveProbes([]string{a, b}, t.TempDir())
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	got := map[string]Source{}
	for _, r := range refs {
		got[r.Dir] = r.Source
	}
	if got["alpha"] != SourceUser || got["beta"] != SourceUser {
		t.Errorf("user probes = %v/%v, want both %q", got["alpha"], got["beta"], SourceUser)
	}
	// Every built-in survived.
	for _, n := range BuiltinNames() {
		if _, ok := got[n]; !ok {
			t.Errorf("built-in %s vanished when directories were named", n)
		}
	}
	if len(refs) != len(BuiltinNames())+2 {
		t.Errorf("resolved %d, want %d builtins + 2", len(refs), len(BuiltinNames()))
	}
}

// A name in two referenced dirs: the LATER one wins, matching the order the
// caller wrote, and says so. Silent shadowing is the failure this labelling
// exists to prevent — it is the same question ("why am I not running the probe
// I wrote?") whether the thing shadowed is a built-in or another directory.
func TestResolveProbesLaterDirShadowsEarlier(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	probeDir(t, a, "dup", "name: from-a\n")
	probeDir(t, b, "dup", "name: from-b\n")

	refs, err := ResolveProbes([]string{a, b}, t.TempDir())
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	var dup *ProbeRef
	for i := range refs {
		if refs[i].Dir == "dup" {
			dup = &refs[i]
		}
	}
	if dup == nil {
		t.Fatalf("refs = %+v, want a dup entry", refs)
	}
	if !strings.HasPrefix(dup.Path, b) {
		t.Errorf("resolved to %q, want the LATER dir %q", dup.Path, b)
	}
	if dup.Source != SourceOverride {
		t.Errorf("source = %q, want %q — a shadowed probe must be visible", dup.Source, SourceOverride)
	}
}

// The list form a flag, an env var and a YAML scalar all carry. A blank entry
// must not become ".", which would silently pull in whatever directory the
// process happens to be sitting in.
func TestSplitProbeDirs(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"a", []string{"a"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b,", []string{"a", "b"}},
	} {
		got := SplitProbeDirs(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitProbeDirs(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitProbeDirs(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// A probe DELETED from the library must stop running.
//
// The extraction is long-lived — an OS temp dir shared by every run on the box
// — and MaterializeBuiltins used to write without pruning, so a removed probe
// stayed on disk and kept resolving as a built-in. It was immortal on every
// machine that had ever benched, and invisible: the library said 16, the runner
// ran 20. Found while moving four probes to the repo that owns them.
func TestMaterializeBuiltinsPrunesWhatIsNoLongerEmbedded(t *testing.T) {
	root := t.TempDir()
	dst, err := MaterializeBuiltins(root)
	if err != nil {
		t.Fatalf("MaterializeBuiltins: %v", err)
	}
	fresh := len(BuiltinNames())
	if fresh == 0 {
		t.Fatal("no built-in probes to test against")
	}

	// A probe from a previous version of the library, left behind by an
	// extraction that predates its removal.
	ghost := filepath.Join(dst, "ghost-probe")
	if err := os.MkdirAll(filepath.Join(ghost, "_fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"task.yaml", "_fixture/seed.go"} {
		if err := os.WriteFile(filepath.Join(ghost, f), []byte("name: ghost\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// And a stray file directly under the extraction root.
	if err := os.WriteFile(filepath.Join(dst, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := MaterializeBuiltins(root); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Errorf("a probe no longer embedded survived re-materialization (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "leftover.txt")); !os.IsNotExist(err) {
		t.Error("a stray file under the extraction root survived")
	}

	// Pruning must not take the real library with it.
	refs, err := ResolveProbes(nil, root)
	if err != nil {
		t.Fatalf("ResolveProbes: %v", err)
	}
	if len(refs) != fresh {
		t.Errorf("resolved %d probes after pruning, want the %d embedded", len(refs), fresh)
	}
	for _, r := range refs {
		if r.Dir == "ghost-probe" {
			t.Error("the ghost is still resolvable")
		}
	}
}
