package toolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain/recipes"
)

// A prefix laid out the way a build leaves it: one directory per build, and
// bin/ a symlink at the active one.
func fakeBuild(t *testing.T, prefix, id, head string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(prefix, "builds", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A binary that answers --version the way llama-server does: on STDERR.
	script := "#!/usr/bin/env bash\nprintf 'version: 999 (" + head + ")\\n' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "llama-server"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := "head=" + head + " patches=none archs=cpu"
	if err := os.WriteFile(filepath.Join(dir, ".corrallm-stamp"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	if !at.IsZero() {
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func link(t *testing.T, prefix, id string) {
	t.Helper()
	if err := os.Symlink(filepath.Join("builds", id), filepath.Join(prefix, "bin")); err != nil {
		t.Fatal(err)
	}
}

func localRunner(t *testing.T) *Local {
	t.Helper()
	requireBash(t)
	return &Local{Dir: filepath.Join(t.TempDir(), "recipes"), Server: "test"}
}

func managed(prefix string) Spec {
	return Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", Prefix: prefix}
}

// A prefix nothing has built yet is not an error and not an empty versioned
// layout — it is honestly "no versions here", which is what the operator needs
// to know before looking for a rollback that does not exist.
func TestBuildsOnAFreshPrefixIsAnAnswerNotAnError(t *testing.T) {
	l := localRunner(t)
	res, err := RunBuilds(context.Background(), l, managed(t.TempDir()))
	if err != nil {
		t.Fatalf("builds: %v", err)
	}
	if res.Versioned {
		t.Error("reported a versioned layout for a prefix with no bin/ at all")
	}
	if len(res.Builds) != 0 {
		t.Errorf("reported %d builds in an empty prefix", len(res.Builds))
	}
	if res.Keep < 1 {
		t.Errorf("keep = %d; a retention count of zero would prune everything", res.Keep)
	}
}

func TestBuildsListsNewestFirstAndMarksTheActiveOne(t *testing.T) {
	l := localRunner(t)
	prefix := t.TempDir()
	now := time.Now()
	fakeBuild(t, prefix, "20260101-000000-aaaaaaaa", "aaaaaaaa", now.Add(-2*time.Hour))
	fakeBuild(t, prefix, "20260102-000000-bbbbbbbb", "bbbbbbbb", now.Add(-time.Hour))
	link(t, prefix, "20260101-000000-aaaaaaaa")

	res, err := RunBuilds(context.Background(), l, managed(prefix))
	if err != nil {
		t.Fatalf("builds: %v", err)
	}
	if !res.Versioned {
		t.Error("a prefix whose bin/ is a symlink must report as versioned")
	}
	if len(res.Builds) != 2 {
		t.Fatalf("got %d builds, want 2: %+v", len(res.Builds), res.Builds)
	}
	if res.Builds[0].ID != "20260102-000000-bbbbbbbb" {
		t.Errorf("newest first is the whole point of the ordering; got %s", res.Builds[0].ID)
	}
	if res.Active != "20260101-000000-aaaaaaaa" {
		t.Errorf("active = %q, want the build bin/ points at", res.Active)
	}
	if res.Builds[0].Active || !res.Builds[1].Active {
		t.Error("the active flag does not match the symlink")
	}
	if res.Builds[1].Head != "aaaaaaaa" {
		t.Errorf("head %q not read from the build's stamp", res.Builds[1].Head)
	}
	if res.Builds[0].At <= 0 {
		t.Error("no install time reported; a rollback list without dates is hard to choose from")
	}
}

// The point of the whole layout: putting a previous build back is a rename, and
// what a probe reports follows the link with no config change.
func TestActivateSwapsWhatProbeSees(t *testing.T) {
	l := localRunner(t)
	prefix := t.TempDir()
	fakeBuild(t, prefix, "old", "aaaaaaaa", time.Time{})
	fakeBuild(t, prefix, "new", "bbbbbbbb", time.Time{})
	link(t, prefix, "new")

	spec := managed(prefix)
	spec.SelectBuild = "old"
	act, err := RunActivate(context.Background(), l, spec)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !act.OK || act.Active != "old" {
		t.Fatalf("activate reported %+v", act)
	}
	if act.Previous != "new" {
		t.Errorf("previous = %q; an operator who rolled back by mistake needs the way forward", act.Previous)
	}

	p, err := RunProbe(context.Background(), l, managed(prefix))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(p.Version, "aaaaaaaa") {
		t.Errorf("probe reports %q — it is not reading through the symlink", p.Version)
	}
	if !strings.Contains(p.Stamp, "head=aaaaaaaa") {
		t.Errorf("stamp %q comes from the wrong build directory", p.Stamp)
	}
}

func TestActivateRefusesABuildThatIsNotThere(t *testing.T) {
	l := localRunner(t)
	prefix := t.TempDir()
	fakeBuild(t, prefix, "only", "aaaaaaaa", time.Time{})
	link(t, prefix, "only")

	spec := managed(prefix)
	spec.SelectBuild = "pruned-last-week"
	_, err := RunActivate(context.Background(), l, spec)
	if err == nil {
		t.Fatal("activating a build that does not exist must fail, not silently do nothing")
	}
	if !strings.Contains(err.Error(), "pruned-last-week") {
		t.Errorf("error %q does not name the build that was asked for", err)
	}
	// And it must not have broken the working install on the way out.
	p, err := RunProbe(context.Background(), l, managed(prefix))
	if err != nil || !p.Present {
		t.Errorf("a failed activate left the tool unusable: present=%v err=%v", p.Present, err)
	}
}

// Adoption's promise is that corrallm never writes to a tree it does not own.
// Both halves of the rollback pair have to keep it.
func TestVersionVerbsRefuseAnAdoptedInstall(t *testing.T) {
	l := localRunner(t)
	adopted := Spec{Name: "llama.cpp", Recipe: "llama.cpp", Bin: "llama-server", InstalledAt: t.TempDir()}

	if _, err := RunBuilds(context.Background(), l, adopted); err == nil {
		t.Error("builds must refuse an adopted install rather than report an empty history for it")
	}
	adopted.SelectBuild = "whatever"
	if _, err := RunActivate(context.Background(), l, adopted); err == nil {
		t.Error("activate must refuse to repoint an install corrallm does not own")
	}
}

// Everything installed before versioned builds existed has bin/ as a real
// directory. That install is the one a first rollback would want, so it is
// moved into builds/ rather than replaced.
func TestALegacyInstallBecomesARollbackTarget(t *testing.T) {
	l := localRunner(t)
	prefix := t.TempDir()

	// The pre-versioning layout: bin/ is a directory, not a link.
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "llama-server"),
		[]byte("#!/usr/bin/env bash\nprintf 'version: 1 (deadbeef)\\n' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, ".corrallm-stamp"),
		[]byte("head=deadbeef patches=none archs=cpu"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBuild(t, prefix, "fresh", "bbbbbbbb", time.Time{})

	spec := managed(prefix)
	spec.SelectBuild = "fresh"
	if _, err := RunActivate(context.Background(), l, spec); err != nil {
		t.Fatalf("activate over a legacy layout: %v", err)
	}

	res, err := RunBuilds(context.Background(), l, managed(prefix))
	if err != nil {
		t.Fatalf("builds: %v", err)
	}
	if res.Active != "fresh" {
		t.Errorf("active = %q, want fresh", res.Active)
	}
	var legacy string
	for _, b := range res.Builds {
		if strings.HasPrefix(b.ID, "legacy-") {
			legacy = b.ID
		}
	}
	if legacy == "" {
		t.Fatalf("the pre-versioning install was not kept: %+v", res.Builds)
	}
	// And it is a real one: rolling back to it must work.
	spec.SelectBuild = legacy
	if _, err := RunActivate(context.Background(), l, spec); err != nil {
		t.Fatalf("activate the migrated install: %v", err)
	}
	p, err := RunProbe(context.Background(), l, managed(prefix))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Version, "deadbeef") {
		t.Errorf("rolled back to the migrated install and got %q", p.Version)
	}
}

// Retention: the newest N survive, and the ACTIVE build survives whatever its
// age. Pruning what is currently serving to satisfy a count would be an outage
// caused by housekeeping.
func TestPruneKeepsTheNewestAndNeverTheActive(t *testing.T) {
	requireBash(t)
	dir := filepath.Join(t.TempDir(), "recipes")
	if err := recipes.Extract(dir); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	base := time.Now().Add(-24 * time.Hour)
	var ids []string
	for i := range 8 {
		id := string(rune('a'+i)) + "-build"
		fakeBuild(t, prefix, id, "head", base.Add(time.Duration(i)*time.Hour))
		ids = append(ids, id)
	}
	// The OLDEST is the one in service — the case that makes "keep 5" wrong.
	link(t, prefix, ids[0])

	cmd := exec.Command("bash", "-c",
		"source '"+filepath.Join(dir, "common.sh")+"'; prune_builds")
	cmd.Env = append(os.Environ(), "TOOL_NAME=llama.cpp", "TOOL_PREFIX="+prefix, "TOOL_KEEP_BUILDS=5")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prune_builds: %v\n%s", err, out)
	}

	left := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(prefix, "builds"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left[e.Name()] = true
	}
	if !left[ids[0]] {
		t.Error("the ACTIVE build was pruned — retention deleted what was serving")
	}
	for _, id := range ids[3:] {
		if !left[id] {
			t.Errorf("build %s is within the newest 5 and was pruned", id)
		}
	}
	for _, id := range ids[1:3] {
		if left[id] {
			t.Errorf("build %s is older than the newest 5 and is not active; it should be gone", id)
		}
	}
	// Still usable afterwards.
	if _, err := os.Stat(filepath.Join(prefix, "bin", "llama-server")); err != nil {
		t.Errorf("bin/ no longer resolves after a prune: %v", err)
	}
}
