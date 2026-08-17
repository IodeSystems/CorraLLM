package recipes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesExcludesTheSharedLibrary(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no recipes embedded; the //go:embed pattern matched nothing")
	}
	for _, n := range names {
		if n == "common" {
			t.Error("common.sh is the shared helper library, not a recipe — it must never be offered as a tool")
		}
	}
}

func TestTheShippedToolsHaveRecipes(t *testing.T) {
	for _, want := range []string{"llama.cpp", "ninfer"} {
		if !Has(want) {
			t.Errorf("no recipe for %q (have: %v)", want, Names())
		}
	}
	if Has("common") {
		t.Error("Has(\"common\") must be false — see TestNamesExcludesTheSharedLibrary")
	}
	if Has("") || Has("nope") {
		t.Error("Has must reject names with no recipe")
	}
}

// Extract must write the WHOLE set, not just one file: every recipe sources
// common.sh from its own directory, so extracting one alone produces a script
// that cannot start.
func TestExtractWritesTheSharedLibraryToo(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "common.sh")); err != nil {
		t.Fatalf("common.sh not extracted: %v — every recipe sources it and would fail to start", err)
	}
	for _, n := range Names() {
		fi, err := os.Stat(filepath.Join(dir, n+".sh"))
		if err != nil {
			t.Fatalf("recipe %s not extracted: %v", n, err)
		}
		if fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("recipe %s is not executable by its owner (mode %v)", n, fi.Mode().Perm())
		}
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("recipe %s is group/world writable (mode %v) — anyone on the box could rewrite what the agent runs as itself", n, fi.Mode().Perm())
		}
	}
}

func TestExtractIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := range 2 {
		if err := Extract(dir); err != nil {
			t.Fatalf("Extract #%d: %v", i+1, err)
		}
	}
}

// Every recipe must answer the read-only verbs. A recipe that silently lacks one
// would report as a runtime error on a host, which is the worst place to find out.
func TestEveryRecipeHandlesTheReadOnlyVerbs(t *testing.T) {
	for _, n := range Names() {
		b, err := Script(n)
		if err != nil {
			t.Fatalf("Script(%s): %v", n, err)
		}
		src := string(b)
		for _, verb := range []string{"probe)", "upstream)", "preflight)", "install-deps)"} {
			if !strings.Contains(src, verb) {
				t.Errorf("recipe %s has no %s case", n, strings.TrimSuffix(verb, ")"))
			}
		}
	}
}

// The output contract: the JSON result goes to stdout and everything else to
// stderr. A recipe that echoes progress to stdout corrupts its own answer, and
// the failure looks like a parse error rather than a stray echo.
func TestRecipesDoNotEchoToStdout(t *testing.T) {
	for _, n := range Names() {
		b, err := Script(n)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "echo ") {
				continue
			}
			if !strings.Contains(trimmed, ">&2") {
				t.Errorf("%s.sh:%d echoes to stdout, which corrupts the JSON result: %s", n, i+1, trimmed)
			}
		}
	}
}
