package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write drops a file next to the main config and returns the dir.
func writeCfg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The whole point: corrallm can write a machine-owned file and have it served,
// without a marshaller round-tripping (and deleting the comments of) the file a
// human maintains.
func TestInclude_MergesModelsFromAnotherFile(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": `
include: [generated/agents.yaml]
servers:
  box1: { pools: { gpu0: 30GB } }
models:
  local:
    cmd: "exec llama-server"
    server: box1
    proxy: 5801
`,
		"generated/agents.yaml": `
models:
  mac-qwen:
    cmd: "exec llama-server"
    server: box1
    proxy: 5802
`,
	})
	c, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"local", "mac-qwen"} {
		if _, ok := c.Models[n]; !ok {
			t.Errorf("model %q missing after include merge", n)
		}
	}
}

// An included model must be a FULL citizen — validated, and eligible for lane
// membership. That is the reason this exists rather than reusing the discovered
// overlay, which ResolveServed can never reach through a lane.
func TestInclude_IncludedModelCanBeALaneMember(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": `
include: [gen.yaml]
servers:
  box1: { pools: { gpu0: 30GB } }
lanes:
  chat:
    members:
      - from-include
`,
		"gen.yaml": `
models:
  from-include:
    cmd: "exec llama-server"
    server: box1
    proxy: 5801
`,
	})
	c, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err != nil {
		t.Fatalf("lane member from an include must validate: %v", err)
	}
	cands, ok := c.ResolveServed("chat")
	if !ok || len(cands) != 1 || cands[0].Name != "from-include" {
		t.Errorf("lane did not resolve to the included model: %v %v", cands, ok)
	}
}

// The operator's own file is the last word. A generated include must never
// quietly redefine something a human wrote down.
func TestInclude_TopLevelWinsOverInclude(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": `
include: [gen.yaml]
servers:
  box1: { pools: { gpu0: 30GB } }
models:
  m:
    cmd: "MINE"
    server: box1
    proxy: 5801
`,
		"gen.yaml": `
models:
  m:
    cmd: "GENERATED"
    server: box1
    proxy: 5809
`,
	})
	c, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models["m"].Cmd; got != "MINE" {
		t.Errorf("cmd = %q, want the top-level file to win", got)
	}
}

// Between includes the LATER one wins — the conventional include-list rule, and
// the one the field doc promises.
func TestInclude_LaterIncludeWinsOverEarlier(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": `
include: [a.yaml, b.yaml]
servers:
  box1: { pools: { gpu0: 30GB } }
`,
		"a.yaml": `
models:
  m: { cmd: "FIRST", server: box1, proxy: 5801 }
`,
		"b.yaml": `
models:
  m: { cmd: "SECOND", server: box1, proxy: 5802 }
`,
	})
	c, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models["m"].Cmd; got != "SECOND" {
		t.Errorf("cmd = %q, want the later include to win", got)
	}
}

// Named by the operator, so its absence is an error — booting with a silently
// smaller config is worse than refusing to boot.
func TestInclude_MissingFileIsAnError(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": "include: [nope.yaml]\n",
	})
	_, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err == nil {
		t.Fatal("want an error for a missing include")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("err = %v, want it to name the missing file", err)
	}
}

func TestInclude_NestedIncludeRejected(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": "include: [a.yaml]\n",
		"a.yaml":        "include: [b.yaml]\n",
		"b.yaml":        "{}\n",
	})
	_, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nested include") {
		t.Fatalf("want a nested-include error, got %v", err)
	}
}

// Dropping a global on the floor is the kind of thing found months later via a
// wrong bill. Say so instead.
func TestInclude_RejectsGlobalSections(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"corrallm.yaml": "include: [a.yaml]\n",
		"a.yaml":        "costPerKwh: 0.42\n",
	})
	_, err := Load(filepath.Join(dir, "corrallm.yaml"))
	if err == nil || !strings.Contains(err.Error(), "costPerKwh") {
		t.Fatalf("want a rejection naming costPerKwh, got %v", err)
	}
}

// Includes resolve against the INCLUDING file's directory, not the process cwd
// — corrallm is launched from several places (bin/run, tests, systemd).
func TestInclude_ResolvesRelativeToTheIncludingFile(t *testing.T) {
	dir := writeCfg(t, map[string]string{
		"conf/corrallm.yaml": `
include: [sub/gen.yaml]
servers:
  box1: { pools: { gpu0: 30GB } }
`,
		"conf/sub/gen.yaml": `
models:
  m: { cmd: "x", server: box1, proxy: 5801 }
`,
	})
	if _, err := Load(filepath.Join(dir, "conf", "corrallm.yaml")); err != nil {
		t.Fatalf("relative include did not resolve against the config's dir: %v", err)
	}
}

// Quality is a float so a tier can be slotted BETWEEN two existing ones without
// renumbering the ladder. The motivating case: an MLX 4-bit port of a 27B that
// is better than the ternary 27B at quality 1 and worse than the original at 2.
//
// The bug this replaces was silent — yaml truncated `quality: 1.5` to 1 and
// validated clean, so the model tied the tier it was meant to beat and nothing
// said a word.
func TestQuality_FractionalTierSurvivesParsing(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
  mac1: { pools: { system: 64GB }, devicePool: system }
models:
  bonsai:      { cmd: x, server: box1, proxy: 5801, quality: 1 }
  mac-4bit:    { cmd: x, server: mac1, proxy: 5810, quality: 1.5 }
  mtp:         { cmd: x, server: box1, proxy: 5800, quality: 2 }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models["mac-4bit"].Quality; got != 1.5 {
		t.Fatalf("quality = %v, want 1.5 (it used to truncate to 1)", got)
	}
	lo, hi := c.Models["bonsai"].Quality, c.Models["mtp"].Quality
	if mid := c.Models["mac-4bit"].Quality; mid <= lo || mid >= hi {
		t.Errorf("%v did not land strictly between %v and %v", mid, lo, hi)
	}
}

// MaxQuality and the degrade gate must both reason in floats, or a fractional
// top tier would round and change which backends a group accepts.
func TestQuality_FractionalTopTierGatesDegrade(t *testing.T) {
	cands := []Candidate{
		{Name: "lo", Model: Model{Quality: 1}},
		{Name: "mid", Model: Model{Quality: 1.5}},
	}
	if top := MaxQuality(cands); top != 1.5 {
		t.Fatalf("MaxQuality = %v, want 1.5", top)
	}

	strict := PriorityGroup{}
	if strict.AcceptsQuality(1, 1.5) {
		t.Error("a non-degrading group must reject a tier below the top, even a fractional one")
	}
	if !strict.AcceptsQuality(1.5, 1.5) {
		t.Error("the top tier is always acceptable")
	}

	degrade := PriorityGroup{AcceptDegrade: true, QualityFloor: 1.25}
	if degrade.AcceptsQuality(1, 1.5) {
		t.Error("1 is below a 1.25 floor and must be rejected")
	}
	if !degrade.AcceptsQuality(1.5, 2) {
		t.Error("1.5 clears a 1.25 floor and must be accepted")
	}
}
