package recipes

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// box1, 2026-10-07 22:51: a build pruned the build it had just replaced, and
// the daemon's benchmark gate then had no baseline to run. Installing then
// pruning must keep the previously active build even past the retention count.
func TestPruneKeepsTheBuildThisInstallReplaced(t *testing.T) {
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
	// "old" is serving, and is the OLDEST of six builds.
	for i, id := range []string{"old", "b2", "b3", "b4", "b5", "b6"} {
		src := buildTree(t, prefix, "$ORIGIN")
		if out, err := installBuild(t, recipes, prefix, src, id); err != nil {
			t.Fatalf("install %s: %v\n%s", id, err, out)
		}
		ts := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.UTC)
		_ = os.Chtimes(filepath.Join(prefix, "builds", id), ts, ts)
	}
	if err := os.Remove(filepath.Join(prefix, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("builds/old", filepath.Join(prefix, "bin")); err != nil {
		t.Fatal(err)
	}

	src := buildTree(t, prefix, "$ORIGIN")
	cmd := exec.Command("bash", "-c", `source "$1/common.sh"; install_build "$2" new; prune_builds`, "_", recipes, src)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix, "BIN_NAME=llama-server", "TOOL_KEEP_BUILDS=5")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install+prune: %v\n%s", err, b)
	}
	if _, err := os.Stat(filepath.Join(prefix, "builds", "old")); err != nil {
		t.Error("the build this install replaced was pruned — the gate has no baseline")
	}
	if _, err := os.Stat(filepath.Join(prefix, "builds", "b2")); err == nil {
		t.Error("nothing was pruned: the retention count did not apply")
	}
}

// patchCycle runs one rebuild's patch steps — unapply, then apply — the way
// build() does, with <prefix>/patches as the patch set.
func patchCycle(t *testing.T, recipes, prefix, src string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", `source "$1/common.sh"; unapply_patches "$2" && apply_patches "$2"`, "_", recipes, src)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("patch cycle: %v\n%s", err, b)
	}
	return string(b)
}

// 2026-11-05: upstream merged #30137, which corrallm carried as a patch. The
// recipe logged it "already" — correct — but the next unapply reverted every
// patch that reverse-checks clean, and an upstreamed patch does: it would have
// reverted upstream's own code. Only what apply_patches applied is reverted.
func TestUnapplyNeverRevertsAPatchUpstreamMerged(t *testing.T) {
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	recipes := t.TempDir()
	if err := Extract(recipes); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	src := filepath.Join(prefix, "src")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	write("a.txt", "a1\na2\na3\n")
	write("b.txt", "b1\nb2\nb3\n")
	git("add", ".")
	git("commit", "-qm", "base")

	// Two corrallm patches, made from the tree and then undone.
	pd := filepath.Join(prefix, "patches")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	write("a.txt", "a1\nUPSTREAMED\na3\n")
	if err := os.WriteFile(filepath.Join(pd, "0001-upstreamed.patch"), []byte(git("diff")), 0o644); err != nil {
		t.Fatal(err)
	}
	git("checkout", "--", ".")
	write("b.txt", "b1\nOURS\nb3\n")
	if err := os.WriteFile(filepath.Join(pd, "0002-ours.patch"), []byte(git("diff")), 0o644); err != nil {
		t.Fatal(err)
	}
	git("checkout", "--", ".")

	if out := patchCycle(t, recipes, prefix, src); !strings.Contains(out, "applied  0001") || !strings.Contains(out, "applied  0002") {
		t.Fatalf("first build did not apply both:\n%s", out)
	}

	// Upstream merges 0001: what align_tree does is unapply, move, apply.
	cmd := exec.Command("bash", "-c", `source "$1/common.sh"; unapply_patches "$2"`, "_", recipes, src)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unapply: %v\n%s", err, b)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("unapply left the tree dirty:\n%s", st)
	}
	write("a.txt", "a1\nUPSTREAMED\na3\n")
	git("commit", "-qam", "upstream merges 0001")

	if out := patchCycle(t, recipes, prefix, src); !strings.Contains(out, "already  0001") {
		t.Fatalf("second build did not see 0001 as upstream:\n%s", out)
	}
	// The rebuild after that is the one that used to revert upstream.
	patchCycle(t, recipes, prefix, src)
	cmd = exec.Command("bash", "-c", `source "$1/common.sh"; unapply_patches "$2"`, "_", recipes, src)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unapply: %v\n%s", err, b)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("unapply reverted upstream's merged code (or left ours):\n%s", st)
	}

	// A patch retired from patches/ while applied is still reverted: the
	// record holds its content, not just its name.
	patchCycle(t, recipes, prefix, src)
	if err := os.Rename(filepath.Join(pd, "0002-ours.patch"), filepath.Join(prefix, "0002-ours.retired")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", `source "$1/common.sh"; unapply_patches "$2"`, "_", recipes, src)
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unapply: %v\n%s", err, b)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("a retired patch stayed in the tree:\n%s", st)
	}
}

// A patch added, edited or retired since the installed build was stamped makes
// the tool Behind even at the same commit; the same patch set does not
// (2026-11-05: a patch-only change waited two hours for upstream to move).
func TestUpstreamReportsAChangedPatchSetAsBehind(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	recipes := t.TempDir()
	if err := Extract(recipes); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	const head = "0b1bad14f0b1bad14f0b1bad14f0b1bad14f0b1b"
	bin := filepath.Join(prefix, "bin")
	pd := filepath.Join(prefix, "patches")
	for _, d := range []string{bin, pd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pd, "0001-a.patch"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upstream := func() string {
		t.Helper()
		// Pinned at the installed commit, so no network is needed and the
		// commit alone says "not behind".
		cmd := exec.Command("bash", "-c", `source "$1/common.sh"; emit_upstream "$2"`, "_", recipes, head)
		cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix,
			"TOOL_URL=https://example.invalid/x.git", "TOOL_REF=master", "TOOL_PIN="+head)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("emit_upstream: %v\n%s", err, b)
		}
		return string(b)
	}
	hash := func() string {
		t.Helper()
		cmd := exec.Command("bash", "-c", `source "$1/common.sh"; patch_set_hash`, "_", recipes)
		cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix)
		b, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	stamp := func(patches string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, ".corrallm-stamp"),
			[]byte("head="+head+" patches="+patches+" archs=86"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stamp(hash())
	if out := upstream(); !strings.Contains(out, `"behind":false`) || !strings.Contains(out, `"patchesChanged":false`) {
		t.Fatalf("same commit, same patch set: want not behind\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(pd, "0002-b.patch"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := upstream(); !strings.Contains(out, `"behind":true`) || !strings.Contains(out, `"patchesChanged":true`) {
		t.Fatalf("a patch was added after the build: want behind + patchesChanged\n%s", out)
	}
	if err := os.Remove(filepath.Join(bin, ".corrallm-stamp")); err != nil {
		t.Fatal(err)
	}
	if out := upstream(); !strings.Contains(out, `"behind":false`) {
		t.Fatalf("no stamp (adopted install): a patch change must not count\n%s", out)
	}
}
