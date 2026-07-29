package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config that came from Load is NOT the same shape as one that goes into it:
// resolveExtensions expands every extension's `provides` into Models. Writing
// that back out emits the extension AND its expansion, and the next Load fails
// with "collides with a declared model" — a file the daemon cannot start from.
func TestSave_DoesNotEmitDerivedExtensionModels(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(src, []byte(`
servers:
  box1: { pools: { system: 8GB } }
extensions:
  oidio:
    cmd: "exec oidio"
    server: box1
    proxy: 5806
    ramUsage: { system: 1GB }
    provides:
      stt: { type: stt }
      tts: { type: tts }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Models["oidio-stt"]; !ok {
		t.Fatal("precondition: Load should have expanded the extension")
	}

	out := filepath.Join(dir, "managed.yml")
	if err := SaveValidated(out, c); err != nil {
		t.Fatalf("SaveValidated: %v", err)
	}
	// The real assertion: what we wrote must load again.
	back, err := Load(out)
	if err != nil {
		t.Fatalf("the written config does not load — the daemon could not restart: %v", err)
	}
	if _, ok := back.Models["oidio-stt"]; !ok {
		t.Error("the extension's models did not come back on reload")
	}
}

// A managed file is rewritten by corrallm, so it must say so — the failure mode
// otherwise is someone hand-editing it and losing the edit without warning.
func TestSave_CarriesTheManagedWarning(t *testing.T) {
	out := filepath.Join(t.TempDir(), "managed.yml")
	if err := Save(out, &Config{Servers: map[string]Server{"box1": {Pools: map[string]string{"system": "8GB"}}}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "MANAGED CONFIG") || !strings.Contains(string(b), "notes") {
		t.Errorf("written file lacks the managed warning:\n%s", b)
	}
}

// An in-memory config that serialises to something Load rejects must fail at
// the write, not leave a daemon that cannot restart.
func TestSaveValidated_RefusesAnUnloadableConfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "managed.yml")
	bad := &Config{
		Servers: map[string]Server{"box1": {Pools: map[string]string{"gpu0": "8GB"}}},
		Lanes:   map[string]Lane{"chat": {Members: []LaneMember{{Model: "does-not-exist"}}}},
	}
	err := SaveValidated(out, bad)
	if err == nil {
		t.Fatal("want a refusal for a lane naming a missing model")
	}
	if !strings.Contains(err.Error(), "refusing to write") {
		t.Errorf("err = %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("an invalid config was written to disk anyway")
	}
}

// The migration's whole point: the knowledge in comments must survive, and
// most of it is written on NESTED fields, not above the entity name.
func TestImportComments_CapturesNestedCommentary(t *testing.T) {
	src := []byte(`
servers:
  # box1 is the only host today.
  box1: { pools: { gpu0: 30GB } }
models:
  # Heavy chat model.
  big:
    cmd: "exec llama-server"
    server: box1
    proxy: 5800
    # 180k, not 220k. At 220k this measured 29130 MiB against 31654 usable,
    # which left no headroom for the mmproj.
    contextPerRequest: 180000
`)
	c, err := LoadBytesForTest(src)
	if err != nil {
		t.Fatal(err)
	}
	orphaned, err := ImportComments(src, c)
	if err != nil {
		t.Fatal(err)
	}
	notes := c.Models["big"].Notes
	if !strings.Contains(notes, "Heavy chat model") {
		t.Error("the entity's own comment was not carried")
	}
	if !strings.Contains(notes, "29130 MiB") {
		t.Errorf("the NESTED measurement note was lost — that is where the value lives:\n%s", notes)
	}
	if !strings.Contains(notes, "contextPerRequest") {
		t.Error("a nested note should say which field it was about")
	}
	if !strings.Contains(c.Servers["box1"].Notes, "only host") {
		t.Error("server comment not carried")
	}
	_ = orphaned
}

// A comment attached to a whole section belongs to no entry. It must be
// REPORTED rather than silently dropped, so the operator can rehome it.
func TestImportComments_ReportsOrphans(t *testing.T) {
	src := []byte(`
# A banner explaining the whole models section.
models:
  m: { proxy: 5800, type: chat }
`)
	c, err := LoadBytesForTest(src)
	if err != nil {
		t.Fatal(err)
	}
	orphaned, err := ImportComments(src, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) == 0 {
		t.Fatal("a section-level comment was dropped silently")
	}
	if !strings.Contains(strings.Join(orphaned, "\n"), "banner") {
		t.Errorf("orphans = %v", orphaned)
	}
}
