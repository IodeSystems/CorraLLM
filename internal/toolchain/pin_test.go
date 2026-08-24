package toolchain

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/toolchain/recipes"
)

// Pinning: holding a tool at one commit while upstream keeps moving.
//
// The case these exist for is a real one that cost weeks — llama.cpp ships
// several builds a day, one of them regresses a model, and the only lever was
// to stop rebuilding by hand and remember why. A pin makes that a decision the
// daemon knows about, and the property that has to hold is narrow: while
// pinned, "behind" must mean behind the PIN. If it kept meaning "behind
// upstream", a pinned tool would report drift forever and — with
// `rebuild: true` — get rebuilt straight past the hold every six hours, which
// is the exact regression a pin is set to prevent.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// gitRepo builds a throwaway repo on branch master with n commits and returns
// its path and every sha, oldest first.
//
// A real repo rather than a stub, because the thing under test is what
// `git ls-remote` answers — and ls-remote against a fake is a test of the fake.
func gitRepo(t *testing.T, n int) (dir string, shas []string) {
	t.Helper()
	requireGit(t)
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Identity and hooks come from the environment otherwise, and a
		// developer's global config (commit.gpgsign, init.templateDir) would
		// make this pass or fail per machine.
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	// Not `init -b`: that is git 2.28+, and the default branch name is exactly
	// the kind of thing that differs between the machine writing the test and
	// the machine running it.
	run("symbolic-ref", "HEAD", "refs/heads/master")
	for i := range n {
		run("commit", "-q", "--allow-empty", "-m", "c"+string(rune('a'+i)))
		shas = append(shas, run("rev-parse", "HEAD"))
	}
	return dir, shas
}

// installed lays down a prefix whose active build reports `head` as its commit,
// the way a llama-server built from that commit would.
func installed(t *testing.T, head string) string {
	t.Helper()
	prefix := t.TempDir()
	fakeBuild(t, prefix, "20260101-000000-"+head[:8], head, time.Time{})
	link(t, prefix, "20260101-000000-"+head[:8])
	return prefix
}

func upstreamOf(t *testing.T, spec Spec) *Upstream {
	t.Helper()
	u, err := RunUpstream(context.Background(), localRunner(t), spec)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	return u
}

// The core promise: sitting exactly on the pin is CURRENT, however far master
// has run ahead. Without this a pinned tool wears a permanent "behind" badge
// and a scheduled rebuild walks it forward — undoing the pin on a timer.
func TestPinnedAtTheInstalledCommitIsNotBehind(t *testing.T) {
	requireBash(t)
	repo, shas := gitRepo(t, 3)
	held := shas[0]

	spec := managed(installed(t, held))
	spec.URL, spec.Ref, spec.Pin = repo, "master", held

	u := upstreamOf(t, spec)
	if u.Behind {
		t.Fatalf("pinned at the installed commit reported behind: %+v", u)
	}
	if u.Pin != held {
		t.Fatalf("pin not reported back: %+v", u)
	}
	// Ahead is the half a pinned operator actually wants: what is being held
	// back FROM. Reporting the hold without it makes "pinned" look like the
	// whole story when master is 200 commits along.
	if !u.Ahead {
		t.Fatalf("master moved two commits past the pin and ahead is false: %+v", u)
	}
	if u.RemoteHead != shas[2] {
		t.Fatalf("remoteHead should still track %s, got %q", spec.Ref, u.RemoteHead)
	}
}

// Behind, while pinned, means "not yet at the pin" — a build is owed. This is
// what makes a scheduled rebuild CONVERGE on a pin instead of ignoring it.
func TestPinnedElsewhereIsBehindThePinNotUpstream(t *testing.T) {
	requireBash(t)
	repo, shas := gitRepo(t, 3)

	// Installed at the newest commit, pinned back to the oldest: upstream would
	// call this current, the pin calls it behind, and the pin is right.
	spec := managed(installed(t, shas[2]))
	spec.URL, spec.Ref, spec.Pin = repo, "master", shas[0]

	u := upstreamOf(t, spec)
	if !u.Behind {
		t.Fatalf("installed commit is not the pin and behind is false: %+v", u)
	}
	if u.Local != shas[2] {
		t.Fatalf("local should be what is installed, got %q", u.Local)
	}
}

// Unpinned, nothing changes: behind still means behind the tracked branch.
func TestUnpinnedStillTracksTheBranch(t *testing.T) {
	requireBash(t)
	repo, shas := gitRepo(t, 2)

	spec := managed(installed(t, shas[0]))
	spec.URL, spec.Ref = repo, "master"

	u := upstreamOf(t, spec)
	if !u.Behind {
		t.Fatalf("a commit behind master and behind is false: %+v", u)
	}
	if u.Ahead {
		t.Fatalf("ahead is only meaningful while pinned: %+v", u)
	}
	if u.Pin != "" {
		t.Fatalf("no pin was set, got %q", u.Pin)
	}
}

// A pin makes the remote round trip optional rather than load-bearing: the
// target is known locally. An unreachable remote costs the "how far ahead" half
// and must NOT come back as an error, or every pinned tool goes red the first
// time the network is down.
func TestAnUnreachableRemoteIsNotAnErrorWhilePinned(t *testing.T) {
	requireBash(t)
	_, shas := gitRepo(t, 1)
	held := shas[0]

	spec := managed(installed(t, held))
	spec.URL, spec.Ref, spec.Pin = filepath.Join(t.TempDir(), "nope.git"), "master", held

	u := upstreamOf(t, spec)
	if u.Error != "" {
		t.Fatalf("a pinned tool with an unreachable remote reported an error: %q", u.Error)
	}
	if u.Behind {
		t.Fatalf("pinned at the installed commit reported behind: %+v", u)
	}

	// Unpinned, the same unreachable remote IS the answer failing — there is
	// nothing else to compare against, and silence would read as "current".
	// RunUpstream surfaces that as an error, which is why this half does not go
	// through the helper.
	spec.Pin = ""
	u2, err := RunUpstream(context.Background(), localRunner(t), spec)
	if err == nil && (u2 == nil || u2.Error == "") {
		t.Fatalf("an unreachable remote with no pin should report why: %+v", u2)
	}
}

// Aligning a checkout to a PIN, which is the half a build depends on.
//
// `git fetch origin <sha>` is not a thing you can rely on: upload-pack's
// allowReachableSHA1InWant is OFF by default and only some hosts turn it on, so
// a pin that fetched fine against GitHub could fail against a mirror — at the
// start of a twenty-minute build, on a machine nobody is watching. The recipe
// therefore falls back to fetching the branches and finding the commit in what
// came back, and this exercises whichever path the local git takes.
func TestAlignTreeChecksOutAPinnedCommit(t *testing.T) {
	requireBash(t)
	repo, shas := gitRepo(t, 3)
	held := shas[0] // deliberately not the tip

	dir := t.TempDir()
	recipeDir := filepath.Join(dir, "recipes")
	if err := recipes.Extract(recipeDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	src := filepath.Join(dir, "src")

	cmd := exec.Command("bash", "-c",
		`source "$1/common.sh"; align_tree "$2" "$3" "$4"`, "_", recipeDir, src, held, repo)
	cmd.Env = append(cmd.Environ(),
		"TOOL_NAME=t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("align to a pinned sha failed: %v\n%s", err, out)
	}

	head := exec.Command("git", "-C", src, "rev-parse", "HEAD")
	got, err := head.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if strings.TrimSpace(string(got)) != held {
		t.Fatalf("checkout is at %s, want the pinned %s", strings.TrimSpace(string(got)), held)
	}
}

// A pin naming a commit no branch reaches must fail with the sha in the
// message. Force-pushes strand commits and a sha copied from a fork is not in
// this remote at all — both are pins that look perfectly valid.
func TestAlignTreeRefusesAnUnreachableCommit(t *testing.T) {
	requireBash(t)
	repo, _ := gitRepo(t, 1)
	const ghost = "1234567812345678123456781234567812345678"

	dir := t.TempDir()
	recipeDir := filepath.Join(dir, "recipes")
	if err := recipes.Extract(recipeDir); err != nil {
		t.Fatalf("extract: %v", err)
	}

	cmd := exec.Command("bash", "-c",
		`source "$1/common.sh"; align_tree "$2" "$3" "$4"`, "_",
		recipeDir, filepath.Join(dir, "src"), ghost, repo)
	cmd.Env = append(cmd.Environ(),
		"TOOL_NAME=t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a commit reachable from nothing was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), ghost) {
		t.Fatalf("the refusal should name the sha, got:\n%s", out)
	}
}

// The FALLBACK path, forced.
//
// Whether `git fetch origin <sha>` works depends on the remote, and the local
// file transport this test suite uses happens to allow it — so the fallback
// would otherwise never run here, and would break unnoticed until somebody
// pinned a tool against a remote that refuses. A git shim that rejects exactly
// that one command makes the branch deterministic.
func TestAlignTreeFallsBackWhenTheRemoteRefusesAShaFetch(t *testing.T) {
	requireBash(t)
	repo, shas := gitRepo(t, 3)
	held := shas[0]

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	shimDir := filepath.Join(dir, "shim")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/usr/bin/env bash\n" +
		"if [ \"$1\" = fetch ] && [ \"$3\" = origin ] && [[ \"$4\" =~ ^[0-9a-f]{40}$ ]]; then\n" +
		"  echo 'fatal: remote refused to serve that object (simulated)' >&2; exit 128\n" +
		"fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	recipeDir := filepath.Join(dir, "recipes")
	if err := recipes.Extract(recipeDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	src := filepath.Join(dir, "src")

	cmd := exec.Command("bash", "-c",
		`source "$1/common.sh"; align_tree "$2" "$3" "$4"`, "_", recipeDir, src, held, repo)
	cmd.Env = append(cmd.Environ(),
		"PATH="+shimDir+":"+os.Getenv("PATH"),
		"TOOL_NAME=t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the fallback did not recover a refused sha fetch: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "would not serve") {
		t.Fatalf("the fallback ran without saying so — an operator reading this log\n"+
			"needs to know why it fetched everything:\n%s", out)
	}

	head, err := exec.Command(realGit, "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if strings.TrimSpace(string(head)) != held {
		t.Fatalf("checkout is at %s, want the pinned %s", strings.TrimSpace(string(head)), held)
	}
}

// staleRunner answers upstream the way a recipe that PREDATES pins does: drift
// computed against the tracked ref, and no pin echoed back.
//
// This is not a hypothetical. Agents carry their own embedded recipes and
// self-update on a build-id mismatch, so the first fleet-wide pin after a
// primary deploy hit exactly this: the Mac sat on the pinned commit and
// reported BEHIND, because its recipe was comparing against master and had
// never heard of TOOL_PIN.
type staleRunner struct{ local, remoteHead string }

func (staleRunner) Where() string { return "stalehost" }

func (s staleRunner) Run(_ context.Context, _ Spec, verb Verb) (*Raw, error) {
	switch verb {
	case VerbProbe:
		return &Raw{JSON: []byte(`{"present":true,"path":"/x/llama-server","version":"` +
			s.local + `","commit":"` + s.local + `","source":"stamp","stamp":""}`)}, nil
	case VerbUpstream:
		// The old shape, verbatim: no "pin", no "ahead".
		return &Raw{JSON: []byte(`{"ref":"master","remoteHead":"` + s.remoteHead +
			`","local":"` + s.local + `","behind":true,"error":""}`)}, nil
	}
	return nil, errors.New("unexpected verb")
}

func stalePinnedRegistry(local, remoteHead, pin string) *Registry {
	return &Registry{
		Cfg: func() *config.Config {
			return &config.Config{
				Servers: map[string]config.Server{"stalehost": {}},
				Tools: map[string]config.Tool{"llama.cpp": {
					URL: "https://example.invalid/llama.cpp.git", Ref: "master", Pin: pin,
					Bin: "llama-server", Hosts: map[string]config.ToolHost{"stalehost": {}},
				}},
			}
		},
		RunnerFor: func(string) (Runner, error) {
			return staleRunner{local: local, remoteHead: remoteHead}, nil
		},
	}
}

// A host that cannot see the pin must not report drift, because its "behind" is
// an answer to a different question. Behind is what the watcher acts on, so
// leaving it set would start a rebuild that the same old recipe aligns to
// master — the pin walked past by the machinery meant to honour it.
func TestAStaleAgentsDriftAnswerIsNotTrustedWhilePinned(t *testing.T) {
	const pin = "c060ca974c773c7c3d17fd1b66dc9d312bc292c0"
	// Installed EXACTLY at the pin, and the old recipe still says behind
	// because master has moved.
	r := stalePinnedRegistry(pin, "f280b26983ad0fdb705a0d9ebf0503e76f2899b0", pin)

	st := r.Survey(context.Background(), "llama.cpp", "stalehost")
	if st.Drift == nil {
		t.Fatal("no drift answer at all")
	}
	if st.Drift.Behind {
		t.Fatal("a pre-pin agent's 'behind' was taken at face value — the watcher would rebuild past the pin")
	}
	if !st.Drift.PinUnsupported {
		t.Fatal("the row does not say the host cannot see the pin; it would read as quietly fine")
	}
	if st.Drift.Error == "" {
		t.Error("nothing explains the missing answer")
	}
}

// And the explicit build is refused for the same reason, naming both commits.
// A build here is not merely uninformative — it INSTALLS the wrong one.
func TestAPinnedBuildIsRefusedOnAnAgentThatPredatesPins(t *testing.T) {
	const pin = "c060ca974c773c7c3d17fd1b66dc9d312bc292c0"
	r := stalePinnedRegistry(pin, "f280b26983ad0fdb705a0d9ebf0503e76f2899b0", pin)

	_, err := r.Build(context.Background(), "llama.cpp", "stalehost", false, nil)
	if err == nil {
		t.Fatal("built on a host that would ignore the pin")
	}
	if !strings.Contains(err.Error(), pin) || !strings.Contains(err.Error(), "master") {
		t.Fatalf("the refusal should name the pin and what would be built instead, got: %v", err)
	}
}

// Unpinned, a pre-pin agent is simply an agent: nothing to detect, nothing to
// suppress, and drift keeps working as it always did. The guard must not turn
// every not-yet-updated host into a warning.
func TestAStaleAgentIsUnaffectedWhenNothingIsPinned(t *testing.T) {
	r := stalePinnedRegistry("c060ca974", "f280b26983ad0fdb705a0d9ebf0503e76f2899b0", "")

	st := r.Survey(context.Background(), "llama.cpp", "stalehost")
	if st.Drift == nil || !st.Drift.Behind {
		t.Fatalf("ordinary drift stopped working: %+v", st.Drift)
	}
	if st.Drift.PinUnsupported {
		t.Error("flagged a host that is not being asked anything about a pin")
	}
}
