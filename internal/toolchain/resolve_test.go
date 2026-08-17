package toolchain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func TestHasToolRef(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{"${tool:llama.cpp}/llama-server -m x.gguf", true},
		{"prefix ${tool:ninfer}/ninfer-serve", true},
		{"/abs/path/llama-server -m x.gguf", false},
		{"", false},
		// Not a tool reference. An ordinary shell variable must pass through
		// untouched, or every cmd using $HOME becomes our problem.
		{"${HOME}/bin/llama-server", false},
		{"$tool:llama.cpp", false},
	} {
		if got := HasToolRef(c.cmd); got != c.want {
			t.Errorf("HasToolRef(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// An adopted entry resolves with NO round trip: its directory is stated in
// config, so there is nothing to ask the host.
func TestExpandAdoptedNeedsNoProbe(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{
		"box1": {InstalledAt: "/opt/ml-kit/bin/llama.cpp"},
	}), f)

	got, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server -m x.gguf", "box1")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != "/opt/ml-kit/bin/llama.cpp/llama-server -m x.gguf" {
		t.Errorf("expanded to %q", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("an adopted entry should resolve from config alone, but ran %v", f.calls)
	}
}

// A managed entry is asked of the HOST, because only the host knows where its
// own install landed — the primary's home is not the agent's on a machine whose
// home is /Users rather than /home.
func TestExpandManagedAsksTheHost(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe: Probe{Present: true, Path: "/var/lib/corrallm/tools/llama.cpp/bin/llama-server"},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	got, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != "/var/lib/corrallm/tools/llama.cpp/bin/llama-server" {
		t.Errorf("expanded to %q — should use the directory the HOST reported", got)
	}
}

// The refusal that keeps this honest. Falling back to PATH would run whichever
// llama-server the machine happens to have, silently, at load time — exactly the
// ambiguity the whole phase exists to remove.
func TestExpandRefusesWhenNotBuilt(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe: Probe{Present: false, Path: "/home/x/.corrallm/tools/llama.cpp/bin/llama-server"},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	_, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1")
	if err == nil {
		t.Fatal("expanded a tool that is not installed")
	}
	if !strings.Contains(err.Error(), "not built yet") || !strings.Contains(err.Error(), "corrallm tools build") {
		t.Errorf("refusal should say what is wrong and how to fix it: %v", err)
	}
}

func TestExpandRefusesUndeclaredToolAndHost(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	if _, err := reg.ExpandTools(context.Background(), "${tool:nope}/x", "box1"); err == nil {
		t.Error("expanded a tool that is not declared at all")
	}
	// Declared tool, wrong host: the message must name the host, since this is
	// how a model placed on the Mac referencing a box1-only tool presents.
	_, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/x", "othermachine")
	if err == nil {
		t.Fatal("expanded a tool not declared on that host")
	}
	if !strings.Contains(err.Error(), "othermachine") {
		t.Errorf("error should name the host: %v", err)
	}
}

// A cmd with no reference must not touch the registry at all — every existing
// absolute-path cmd keeps working and pays nothing.
func TestExpandLeavesPlainCommandsAlone(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	in := "/home/nthalk/ml-kit/local/bin/llama.cpp/llama-server -m x.gguf --port 5801"
	got, err := reg.ExpandTools(context.Background(), in, "box1")
	if err != nil || got != in {
		t.Errorf("got (%q, %v), want the input unchanged", got, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a plain cmd should ask the host nothing, but ran %v", f.calls)
	}
}

// Resolution is cached per (tool, host) so a spawn does not pay a probe every
// time — but a build must drop it, since that is when an absent tool becomes
// present.
func TestResolutionIsCachedAndInvalidatedByBuild(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe: Probe{Present: true, Path: "/x/tools/llama.cpp/bin/llama-server"},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	for range 3 {
		if _, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1"); err != nil {
			t.Fatal(err)
		}
	}
	probes := 0
	for _, c := range f.calls {
		if c == VerbProbe {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("probed %d times for 3 expansions; the answer should be cached", probes)
	}

	reg.InvalidateResolved("llama.cpp", "box1")
	if _, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1"); err != nil {
		t.Fatal(err)
	}
	probes = 0
	for _, c := range f.calls {
		if c == VerbProbe {
			probes++
		}
	}
	if probes != 2 {
		t.Errorf("invalidation did not force a re-probe (probes=%d)", probes)
	}
}

// A failed lookup must NOT be cached, or a tool stays unusable for the life of
// the daemon after it is built.
func TestFailureIsNotCached(t *testing.T) {
	f := &fakeRunner{answers: map[Verb]any{
		VerbProbe: Probe{Present: false, Path: "/x/bin/llama-server"},
	}}
	reg := testRegistry(cfgWith(map[string]config.ToolHost{"box1": {}}), f)

	if _, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1"); err == nil {
		t.Fatal("expected a refusal")
	}
	// Now it exists — as it would after a build.
	b, _ := json.Marshal(Probe{Present: true, Path: "/x/bin/llama-server"})
	f.answers[VerbProbe] = json.RawMessage(b)

	got, err := reg.ExpandTools(context.Background(), "${tool:llama.cpp}/llama-server", "box1")
	if err != nil {
		t.Fatalf("a tool that became present must resolve, got %v", err)
	}
	if got != "/x/bin/llama-server" {
		t.Errorf("expanded to %q", got)
	}
}
