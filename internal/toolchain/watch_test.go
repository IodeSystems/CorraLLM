package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

// watchRunner answers verbs from a canned table and records what it was asked.
//
// Its own type rather than fakeRunner's: the watcher surveys hosts CONCURRENTLY
// and starts builds on a background goroutine, so the call log has to be
// mutex-guarded or the race detector fails the test for the harness's sake
// rather than the code's.
type watchRunner struct {
	answers map[Verb]any

	mu    sync.Mutex
	calls []Verb
	built chan struct{} // closed on the first build
}

func newWatchRunner(answers map[Verb]any) *watchRunner {
	return &watchRunner{answers: answers, built: make(chan struct{})}
}

func (f *watchRunner) Where() string { return "fake" }
func (f *watchRunner) SetForce(bool) {}
func (f *watchRunner) Run(_ context.Context, _ Spec, v Verb) (*Raw, error) {
	f.mu.Lock()
	f.calls = append(f.calls, v)
	if v == VerbBuild {
		select {
		case <-f.built:
		default:
			close(f.built)
		}
	}
	f.mu.Unlock()

	a, ok := f.answers[v]
	if !ok {
		return nil, errors.New("host could not answer " + string(v))
	}
	b, _ := json.Marshal(a)
	return &Raw{JSON: b}, nil
}

func (f *watchRunner) saw(v Verb) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == v {
			n++
		}
	}
	return n
}

// driftingAnswers is a host whose tool is installed and demonstrably behind.
func driftingAnswers() map[Verb]any {
	return map[Verb]any{
		VerbProbe: Probe{Present: true, Path: "/opt/llama.cpp", Version: "10380 (0b1bad14f)",
			Commit: "0b1bad14f", Source: "binary"},
		VerbUpstream: Upstream{Ref: "master", RemoteHead: "34af94cd9", Local: "0b1bad14f", Behind: true},
		VerbBuild:    Build{OK: true, Head: "34af94cd9", Version: "10999", Stamp: "head=34af94cd9"},
	}
}

func watchCfg(check string, rebuild bool, host config.ToolHost) *config.Config {
	return &config.Config{
		Servers: map[string]config.Server{"box1": {}},
		Tools: map[string]config.Tool{"llama.cpp": {
			URL: "https://example.invalid/llama.cpp.git", Ref: "master", Bin: "llama-server",
			Check: check, Rebuild: rebuild,
			Hosts: map[string]config.ToolHost{"box1": host},
		}},
	}
}

func newWatcher(cfg *config.Config, r Runner) (*Watcher, *Builder) {
	reg := testRegistry(cfg, r)
	b := &Builder{Reg: reg}
	return &Watcher{Reg: reg, Builder: b}, b
}

// waitForBuild waits for a build to actually reach the runner. Builder.Start
// returns before its goroutine runs, so asserting immediately after it would
// pass whether or not anything was built.
func waitForBuild(t *testing.T, r *watchRunner) bool {
	t.Helper()
	select {
	case <-r.built:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// `check: off` must mean the host is never contacted at all. Surveying anyway
// and merely discarding the answer would keep waking sleeping laptops, which is
// the cost the setting exists to avoid.
func TestCheckOffNeverSurveys(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	w, _ := newWatcher(watchCfg("off", false, config.ToolHost{}), r)

	w.CheckDue(context.Background())

	if n := r.saw(VerbProbe); n != 0 {
		t.Errorf("check is off, so nothing should have been asked; probe ran %d times", n)
	}
	if len(w.LastChecked()) != 0 {
		t.Errorf("a tool that is never checked should not be stamped: %v", w.LastChecked())
	}
}

// The cadence has to actually gate. Without this, the once-a-minute tick would
// run every tool's check every minute regardless of `check:`, turning a 6h
// ls-remote into 360 of them.
func TestCadenceGatesRepeatChecks(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	w, _ := newWatcher(watchCfg("6h", false, config.ToolHost{}), r)

	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	w.Now = func() time.Time { return now }

	w.CheckDue(context.Background()) // never checked -> due
	if n := r.saw(VerbProbe); n != 1 {
		t.Fatalf("first pass should check once, got %d", n)
	}

	now = now.Add(5*time.Hour + 59*time.Minute)
	w.CheckDue(context.Background())
	if n := r.saw(VerbProbe); n != 1 {
		t.Errorf("not due yet at 5h59m, but it checked again (%d total)", n)
	}

	now = now.Add(2 * time.Minute) // 6h01m
	w.CheckDue(context.Background())
	if n := r.saw(VerbProbe); n != 2 {
		t.Errorf("due at 6h01m, want 2 checks, got %d", n)
	}
}

// Drift alone must NOT build. Building is opt-in per tool because it is minutes
// of pegged GPU that replaces the binary resident models are running on.
func TestDriftWithoutRebuildReportsOnly(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	w, _ := newWatcher(watchCfg("6h", false, config.ToolHost{}), r)

	w.CheckDue(context.Background())

	if r.saw(VerbUpstream) != 1 {
		t.Error("drift should still have been checked")
	}
	// WAITED FOR, not sampled. Builder.Start returns before its goroutine has
	// run, so reading the call log straight after CheckDue passes whether or not
	// the opt-in guard exists — which is exactly how this test was wrong first
	// time: deleting `if !t.Rebuild` left it green.
	select {
	case <-r.built:
		t.Fatal("rebuild is off; a scheduled check must never build")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDriftWithRebuildStartsABuild(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	w, _ := newWatcher(watchCfg("6h", true, config.ToolHost{}), r)

	w.CheckDue(context.Background())

	if !waitForBuild(t, r) {
		t.Fatal("rebuild is on and the tool is behind: a build should have started")
	}
}

// The safety property. An adopted entry points at a tree corrallm does not own
// — ml-kit's checkout, a Homebrew prefix — and a managed build runs
// `git clean -xdf`. Registry.Build refuses this too; the watcher refusing FIRST
// is what keeps a scheduled check from filing a guaranteed-to-fail build every
// six hours forever.
func TestAdoptedIsNeverRebuiltOnASchedule(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	w, b := newWatcher(watchCfg("6h", true, config.ToolHost{InstalledAt: "/opt/ml-kit/llama.cpp"}), r)

	w.CheckDue(context.Background())

	select {
	case <-r.built:
		t.Fatal("an ADOPTED install was rebuilt on a schedule; corrallm must never write to a tree it does not own")
	case <-time.After(300 * time.Millisecond):
	}
	if cur, last := b.State(); cur != nil || last != nil {
		t.Errorf("no build job should exist for an adopted tool: current=%v last=%v", cur, last)
	}
	// Drift must still have been REPORTED — that is the whole value of adoption.
	if r.saw(VerbUpstream) != 1 {
		t.Error("an adopted tool's drift should still be checked and reported")
	}
}

// A tool that is up to date must not build even when rebuild is on.
func TestUpToDateDoesNotBuild(t *testing.T) {
	a := driftingAnswers()
	a[VerbUpstream] = Upstream{Ref: "master", RemoteHead: "0b1bad14f", Local: "0b1bad14f", Behind: false}
	r := newWatchRunner(a)
	w, _ := newWatcher(watchCfg("6h", true, config.ToolHost{}), r)

	w.CheckDue(context.Background())

	select {
	case <-r.built:
		t.Fatal("nothing drifted; a build should not have started")
	case <-time.After(300 * time.Millisecond):
	}
}

// An unreachable host must not be mistaken for drift, and must not wedge the
// cadence: it is stamped as checked so it retries on its interval.
func TestUnreachableHostIsNotDrift(t *testing.T) {
	r := newWatchRunner(map[Verb]any{}) // answers nothing
	w, _ := newWatcher(watchCfg("6h", true, config.ToolHost{}), r)

	w.CheckDue(context.Background())

	select {
	case <-r.built:
		t.Fatal("a host that could not be reached must never trigger a build")
	case <-time.After(300 * time.Millisecond):
	}
	if _, ok := w.LastChecked()["llama.cpp"]; !ok {
		t.Error("an unreachable host should still stamp the cadence, or it retries every tick")
	}
}

// fakeHistory is a store that answers LastBuild with a canned record.
type fakeHistory struct{ last *BuildRecord }

func (h fakeHistory) Start(context.Context, string, string, time.Time) (int64, error) { return 1, nil }
func (h fakeHistory) Finish(context.Context, int64, string, time.Time, bool, string, string, string, string) error {
	return nil
}
func (h fakeHistory) LastBuild(context.Context, string, string) (BuildRecord, bool, error) {
	if h.last == nil {
		return BuildRecord{}, false, nil
	}
	return *h.last, true, nil
}

func noBuild(t *testing.T, r *watchRunner, why string) {
	t.Helper()
	select {
	case <-r.built:
		t.Fatal(why)
	case <-time.After(300 * time.Millisecond):
	}
}

// rebuildAt: drift found before the slot waits; the check at the slot builds.
func TestRebuildAtWaitsForItsDailySlot(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	cfg := watchCfg("6h", true, config.ToolHost{})
	tl := cfg.Tools["llama.cpp"]
	tl.RebuildAt = "06:00"
	cfg.Tools["llama.cpp"] = tl
	w, _ := newWatcher(cfg, r)
	loc := time.FixedZone("PST", -8*3600)
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, loc)
	w.Now = func() time.Time { return now }

	w.CheckDue(context.Background())
	noBuild(t, r, "05:00 is outside the 06:00 slot (yesterday's slot ended at 07:00); nothing should build")

	now = time.Date(2026, 11, 6, 6, 1, 0, 0, loc) // 1h01m after the last check, under 6h
	w.CheckDue(context.Background())
	if r.saw(VerbUpstream) != 2 {
		t.Fatalf("the slot should make the check due even though check: 6h has not elapsed; upstream asked %d times", r.saw(VerbUpstream))
	}
	if !waitForBuild(t, r) {
		t.Fatal("drift at 06:01 with rebuildAt 06:00: the daily build should have started")
	}
}

// One churn a day: a second check inside the same slot does not build again.
func TestRebuildAtBuildsOncePerSlot(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	cfg := watchCfg("1m", true, config.ToolHost{})
	tl := cfg.Tools["llama.cpp"]
	tl.RebuildAt = "06:00"
	cfg.Tools["llama.cpp"] = tl
	w, b := newWatcher(cfg, r)
	now := time.Date(2026, 11, 6, 6, 1, 0, 0, time.UTC)
	w.Now = func() time.Time { return now }

	w.CheckDue(context.Background())
	if !waitForBuild(t, r) {
		t.Fatal("first check in the slot should build")
	}
	for i := 0; i < 100; i++ { // let the job finish so the slot is free of "already running"
		if cur, _ := b.State(); cur == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	now = now.Add(30 * time.Minute)
	w.CheckDue(context.Background())
	time.Sleep(300 * time.Millisecond)
	if n := r.saw(VerbBuild); n != 1 {
		t.Fatalf("still in the same slot: want 1 build, got %d", n)
	}

	now = now.Add(24 * time.Hour) // next day's slot
	w.CheckDue(context.Background())
	time.Sleep(300 * time.Millisecond)
	if n := r.saw(VerbBuild); n != 2 {
		t.Fatalf("next day's slot should build again: want 2 builds, got %d", n)
	}
}

// The store's record of today's build counts too, so a restart inside the
// window does not churn a second time.
func TestRebuildAtHonoursABuildTheStoreRemembers(t *testing.T) {
	r := newWatchRunner(driftingAnswers())
	cfg := watchCfg("6h", true, config.ToolHost{})
	tl := cfg.Tools["llama.cpp"]
	tl.RebuildAt = "06:00"
	cfg.Tools["llama.cpp"] = tl
	w, b := newWatcher(cfg, r)
	now := time.Date(2026, 11, 6, 6, 40, 0, 0, time.UTC)
	w.Now = func() time.Time { return now }
	b.History = fakeHistory{last: &BuildRecord{StartedAt: now.Add(-35 * time.Minute), Status: "ok", Stamp: "head=aaaa"}}

	w.CheckDue(context.Background())
	noBuild(t, r, "the store says today's slot already built; a restarted daemon must not build again")
}

// A commit whose build was rolled back is held until upstream moves.
func TestRolledBackCommitIsHeldUntilUpstreamMoves(t *testing.T) {
	r := newWatchRunner(driftingAnswers()) // upstream at 34af94cd9
	w, b := newWatcher(watchCfg("6h", true, config.ToolHost{}), r)
	b.History = fakeHistory{last: &BuildRecord{StartedAt: time.Now().Add(-time.Hour), Status: "failed",
		Stamp: "head=34af94cd9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa patches=x archs=86"}}

	w.CheckDue(context.Background())
	noBuild(t, r, "upstream is still at the rolled-back commit; it must be held")

	r2 := newWatchRunner(driftingAnswers())
	a := driftingAnswers()
	a[VerbUpstream] = Upstream{Ref: "master", RemoteHead: "5555beef0", Local: "0b1bad14f", Behind: true}
	r2.answers = a
	w2, b2 := newWatcher(watchCfg("6h", true, config.ToolHost{}), r2)
	b2.History = b.History
	w2.CheckDue(context.Background())
	if !waitForBuild(t, r2) {
		t.Fatal("upstream moved past the rolled-back commit; the hold should lift")
	}
}

func TestStampHead(t *testing.T) {
	if got := StampHead("head=abc patches=none archs=86"); got != "abc" {
		t.Errorf("StampHead = %q", got)
	}
	if got := StampHead(""); got != "" {
		t.Errorf("StampHead(\"\") = %q", got)
	}
	if !sameCommit("34af94cd9", "34af94cd9aaaa") || sameCommit("", "x") || sameCommit("abc", "abd") {
		t.Error("sameCommit wrong")
	}
}
