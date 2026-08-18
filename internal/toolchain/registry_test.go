package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// fakeRunner answers verbs from a canned table, recording what it was asked.
// It exists so the refusals and the ordering can be tested without a compiler.
type fakeRunner struct {
	answers map[Verb]any
	calls   []Verb
	force   bool
	// block, when set, holds the build verb open until closed — so a test can
	// observe the single-slot refusal while a build is genuinely in flight.
	block chan struct{}
}

func (f *fakeRunner) Where() string   { return "fake" }
func (f *fakeRunner) SetForce(v bool) { f.force = v }
func (f *fakeRunner) Run(_ context.Context, _ Spec, v Verb) (*Raw, error) {
	f.calls = append(f.calls, v)
	if v == VerbBuild && f.block != nil {
		<-f.block
	}
	a, ok := f.answers[v]
	if !ok {
		// Stands in for a host that could not answer this verb — an unreachable
		// agent, a recipe that died. Distinct from a verb that answered "no".
		return nil, errors.New("host could not answer " + string(v))
	}
	b, _ := json.Marshal(a)
	return &Raw{JSON: b}, nil
}

func testRegistry(cfg *config.Config, r Runner) *Registry {
	return &Registry{
		Home:      "/home/test/.corrallm",
		Cfg:       func() *config.Config { return cfg },
		RunnerFor: func(string) (Runner, error) { return r, nil },
	}
}

func cfgWith(hosts map[string]config.ToolHost) *config.Config {
	return &config.Config{
		Servers: map[string]config.Server{"box1": {}},
		Tools: map[string]config.Tool{"llama.cpp": {
			URL: "https://example.invalid/llama.cpp.git", Ref: "master",
			Bin: "llama-server", Hosts: hosts,
		}},
	}
}

// A managed entry installs under corrallm's home; an adopted one reports the
// foreign path and NO prefix, which is what keeps a build from ever targeting it.
func TestSpecForManagedVsAdopted(t *testing.T) {
	managed := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), nil)
	spec, declared, err := managed.SpecFor("llama.cpp", "box1")
	if err != nil || !declared {
		t.Fatalf("declared=%v err=%v", declared, err)
	}
	// An empty prefix is correct: it means "the host's own default", which the
	// recipe derives where the install actually happens. The primary's home is
	// not the agent's on a machine whose home is /Users rather than /home.
	if spec.Prefix != "" {
		t.Errorf("managed prefix = %q, want empty so the host decides", spec.Prefix)
	}
	if spec.InstalledAt != "" {
		t.Errorf("managed entry must not carry installedAt, got %q", spec.InstalledAt)
	}

	adopted := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {InstalledAt: "/opt/llama"}}), nil)
	spec, _, _ = adopted.SpecFor("llama.cpp", "box1")
	if spec.InstalledAt != "/opt/llama" {
		t.Errorf("adopted installedAt = %q", spec.InstalledAt)
	}
	if spec.Prefix != "" {
		t.Errorf("an adopted entry must have NO prefix — a prefix is where a build would write, got %q", spec.Prefix)
	}
}

// An undeclared host is not an unavailable one. Nothing may infer one from the
// other, and neither is an error.
func TestSpecForUndeclaredHostIsNotAnError(t *testing.T) {
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), nil)
	_, declared, err := reg.SpecFor("llama.cpp", "someotherbox")
	if err != nil {
		t.Errorf("an undeclared host is a fact, not an error: %v", err)
	}
	if declared {
		t.Error("declared true for a host with no entry")
	}
}

// The refusal that matters most: a build begins with `git clean -xdf`, so
// pointing one at an adopted tree would destroy a human's uncommitted work and
// rebuild the binary production is spawning, in one step.
func TestBuildRefusesAdopted(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {InstalledAt: "/home/me/ml-kit/local/bin/llama.cpp"}}), f)

	_, err := reg.Build(context.Background(), "llama.cpp", "box1", false, nil)
	if err == nil {
		t.Fatal("built into an adopted tree")
	}
	if !strings.Contains(err.Error(), "adopted") || !strings.Contains(err.Error(), "installedAt") {
		t.Errorf("refusal should name the cause and the cure: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("refused build still ran %v on the host", f.calls)
	}
}

func TestInstallDepsRefusesAdopted(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {InstalledAt: "/opt/llama"}}), f)
	if _, err := reg.InstallDeps(context.Background(), "llama.cpp", "box1"); err == nil {
		t.Fatal("installed dependencies for an adopted install")
	}
	if len(f.calls) != 0 {
		t.Errorf("refused install still ran %v", f.calls)
	}
}

// Preflight gates the build. Twelve minutes of nvcc followed by "you were
// missing a package" is the failure this is here to prevent.
func TestBuildRunsPreflightFirstAndStopsOnBlocked(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbPreflight: Preflight{OK: false, Missing: []string{"ffmpeg dev libs"}, Commands: []string{"apt-get install -y libavformat-dev"}},
		VerbBuild:     Build{OK: true},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	_, err := reg.Build(context.Background(), "llama.cpp", "box1", false, nil)
	if err == nil {
		t.Fatal("built despite a blocked preflight")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("error should carry what is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "apt-get") {
		t.Errorf("error should carry the fix: %v", err)
	}
	for _, c := range f.calls {
		if c == VerbBuild {
			t.Fatal("build ran after preflight said no")
		}
	}
}

func TestBuildProceedsWhenPreflightPasses(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbPreflight: Preflight{OK: true, Runnable: true},
		VerbBuild:     Build{OK: true, Head: "abc123", Version: "10380 (abc123)", Seconds: 700},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	res, err := reg.Build(context.Background(), "llama.cpp", "box1", true, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !res.OK || res.Version != "10380 (abc123)" {
		t.Errorf("result = %+v", res)
	}
	if !f.force {
		t.Error("--force did not reach the runner")
	}
	if len(f.calls) != 2 || f.calls[0] != VerbPreflight || f.calls[1] != VerbBuild {
		t.Errorf("call order = %v, want [preflight build]", f.calls)
	}
}

// Drift is not asked about a tool that is not installed: there is no local
// revision to compare, so the round trip would learn nothing.
func TestSurveySkipsDriftWhenAbsent(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe:    Probe{Present: false},
		VerbUpstream: Upstream{RemoteHead: "deadbeef"},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	st := reg.Survey(context.Background(), "llama.cpp", "box1")
	if st.Drift != nil {
		t.Error("asked upstream about a tool that is not installed")
	}
	for _, c := range f.calls {
		if c == VerbUpstream {
			t.Fatal("upstream was called for an absent tool")
		}
	}
}

// A failed drift check must not discard the version. The probe established a
// fact; the network failing afterwards does not unestablish it.
func TestSurveyKeepsProbeWhenDriftFails(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe: Probe{Present: true, Version: "10380 (abc)", Source: "binary"},
		// no VerbUpstream answer → nil JSON → error path
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	st := reg.Survey(context.Background(), "llama.cpp", "box1")
	if st.Probe == nil || st.Probe.Version != "10380 (abc)" {
		t.Errorf("probe lost when the drift check failed: %+v", st.Probe)
	}
}
