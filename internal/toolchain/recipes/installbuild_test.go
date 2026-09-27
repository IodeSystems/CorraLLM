package recipes

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildTree compiles, into <prefix>/src/build/bin, a shared library and a
// binary that needs it — the shape llama.cpp's build leaves. rpath is the
// binary's RUNPATH: the absolute build dir is what cmake used to embed.
func buildTree(t *testing.T, prefix, rpath string) string {
	t.Helper()
	out := filepath.Join(prefix, "src", "build", "bin")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(out, "lib.c")
	main := filepath.Join(out, "main.c")
	if err := os.WriteFile(lib, []byte("int answer(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("int answer(void);\nint main(void){return answer();}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if b, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
			t.Fatalf("cc %v: %v\n%s", args, err, b)
		}
	}
	run("-shared", "-fPIC", "-o", filepath.Join(out, "libanswer.so"), lib)
	run("-o", filepath.Join(out, "llama-server"), main, "-L"+out, "-lanswer", "-Wl,--enable-new-dtags,-rpath,"+rpath)
	_ = os.Remove(lib)
	_ = os.Remove(main)
	return out
}

// installBuild runs common.sh's install_build the way a recipe does.
func installBuild(t *testing.T, recipes, prefix, src, id string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", `source "$1/common.sh"; install_build "$2" "$3"`, "_", recipes, src, id)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix, "BIN_NAME=llama-server")
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// The outage of 2026-09-26: the installed llama-server loaded its libraries
// from src/build/bin, which the next rebuild deletes first. A copy that depends
// on the build tree must be refused BEFORE the switch, leaving the build that
// was serving active; a copy that loads from beside itself must be activated.
func TestInstallBuildRefusesACopyThatLoadsFromTheBuildTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the library check uses ldd")
	}
	for _, tool := range []string{"bash", "cc", "ldd"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	recipes := t.TempDir()
	if err := Extract(recipes); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()

	src := buildTree(t, prefix, "$ORIGIN")
	if out, err := installBuild(t, recipes, prefix, src, "good"); err != nil {
		t.Fatalf("a self-contained build was refused: %v\n%s", err, out)
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin")); got != "builds/good" {
		t.Fatalf("bin -> %q, want builds/good", got)
	}

	src = buildTree(t, prefix, filepath.Join(prefix, "src", "build", "bin"))
	out, err := installBuild(t, recipes, prefix, src, "bad")
	if err == nil {
		t.Fatalf("a build that loads from src/build/bin was activated:\n%s", out)
	}
	if !strings.Contains(out, "libanswer.so") {
		t.Errorf("the refusal does not name the library that resolved outside the build:\n%s", out)
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin")); got != "builds/good" {
		t.Errorf("bin -> %q after a refused build; the serving build must stay active", got)
	}
	if _, err := os.Stat(filepath.Join(prefix, "builds", "bad")); !os.IsNotExist(err) {
		t.Errorf("the refused build was left in builds/, where activate could select it (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "builds", "good", "libanswer.so")); err != nil {
		t.Errorf("the serving build was disturbed: %v", err)
	}
}
