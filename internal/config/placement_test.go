package config

import (
	"strings"
	"testing"
)

// The migration has to be exact. Every config in existence is written the old
// way, so if normalisation changes ANY of what a model means, it changes
// production behaviour on upgrade rather than at a moment anyone chose.
func TestLegacyModelNormalisesToOneIdenticalPlacement(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  qwen:
    cmd: "exec llama-server --port 5800"
    server: box1
    proxy: 5800
    ramUsage: { gpu0: 20GB }
    maxConcurrent: 2
    contextPerRequest: 180000
    type: chat
    quality: 2
`)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models["qwen"]
	ps := m.PlacementList()
	if len(ps) != 1 {
		t.Fatalf("legacy model produced %d placements, want exactly 1", len(ps))
	}
	p := ps[0]
	if p.Server != "box1" || p.Cmd != m.Cmd {
		t.Errorf("placement lost the model's cmd/server: %+v", p)
	}
	if p.RAMUsage["gpu0"] != "20GB" || p.MaxConcurrent != 2 || p.ContextPerRequest != 180000 {
		t.Errorf("placement lost sizing fields: %+v", p)
	}
	// The process key MUST be unchanged for a legacy model, or residency and
	// agent reconciliation are orphaned the moment the daemon restarts.
	if got := m.PlacementProcKey("qwen", p); got != "qwen" {
		t.Errorf("legacy proc key = %q, want %q — anything else strands running backends", got, "qwen")
	}
}

// The case the whole change exists for: one model, two boxes, different cmds.
//
// Built directly rather than loaded, because Validate deliberately refuses more
// than one placement until the runtime can schedule them (see
// TestMultiPlacementIsRefusedUntilTheRuntimeCanServeIt). The SCHEMA is what is
// under test here.
func TestModelOnTwoBoxesKeepsThemDistinct(t *testing.T) {
	m := Model{Type: "chat", Quality: 2, Placements: []Placement{
		{Server: "box1", Cmd: "exec llama-server -m qwen-q6.gguf --port 5800",
			RAMUsage: map[string]string{"gpu0": "20GB"}, MaxConcurrent: 4},
		{Server: "mac1", Cmd: "exec llama-server -m qwen-q4.gguf --port 5810",
			MaxConcurrent: 1},
	}}
	ps := m.PlacementList()
	if len(ps) != 2 {
		t.Fatalf("got %d placements, want 2", len(ps))
	}
	if got := m.PlacementServers(); len(got) != 2 {
		t.Errorf("PlacementServers = %v, want both boxes", got)
	}
	// Distinct process keys. Sharing one would make loading the Mac's copy look
	// like loading box1's, and unloading either would free a reservation the
	// other still held.
	if k1, k2 := m.PlacementProcKey("qwen", ps[0]), m.PlacementProcKey("qwen", ps[1]); k1 == k2 {
		t.Errorf("both placements key to %q — they would share a process slot", k1)
	}
	byServer := map[string]Placement{}
	for _, p := range ps {
		byServer[p.Server] = p
	}
	// Per-placement sizing is the reason these cannot be one entry: a 5090
	// serves four where a laptop serves one.
	if byServer["box1"].MaxConcurrent != 4 || byServer["mac1"].MaxConcurrent != 1 {
		t.Errorf("per-placement slots collapsed: %+v", byServer)
	}
	if byServer["mac1"].RAMUsage != nil {
		t.Error("mac1 declared no ramUsage and must not inherit box1's")
	}
}

// The same box twice — two quantisations — must not collide.
func TestTwoPlacementsOnOneBoxGetDistinctNames(t *testing.T) {
	m := Model{Type: "chat", Placements: []Placement{
		{Server: "box1", Cmd: "exec llama-server -m qwen-q4.gguf --port 5800"},
		{Server: "box1", Cmd: "exec llama-server -m qwen-q6.gguf --port 5801"},
	}}
	ps := m.PlacementList()
	if ps[0].Name == ps[1].Name {
		t.Fatalf("both named %q — profile, capabilities and process key would collide", ps[0].Name)
	}
	if m.PlacementProcKey("qwen", ps[0]) == m.PlacementProcKey("qwen", ps[1]) {
		t.Error("same-box placements share a process key")
	}
}

// The schema runs ahead of the runtime, and that gap must fail LOUDLY.
// Admission, residency and target resolution still key by one server, so a
// second placement would be accepted and never served — a config that says two
// boxes and means one.
func TestMultiPlacementIsRefusedUntilTheRuntimeCanServeIt(t *testing.T) {
	_, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 64GB }
    devicePool: system
models:
  qwen:
    type: chat
    placements:
      - server: box1
        cmd: "exec a --port 5800"
        proxy: 5800
      - server: mac1
        cmd: "exec b --port 5810"
        proxy: 5810
`)
	if err == nil {
		t.Fatal("two placements loaded, but nothing can schedule the second")
	}
	if !strings.Contains(err.Error(), "serves one per model") {
		t.Errorf("error should say why, got: %v", err)
	}
}

// A model written the NEW way with one placement must serve exactly as the old
// shape did — that is what makes the syntax usable before the runtime catches
// up.
func TestSinglePlacementProjectsOntoTheRuntimeFields(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  qwen:
    type: chat
    placements:
      - server: box1
        cmd: "exec llama-server --port 5800"
        proxy: 5800
        ramUsage: { gpu0: 20GB }
        maxConcurrent: 4
`)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models["qwen"]
	if m.Server != "box1" || m.Cmd == "" {
		t.Errorf("placement did not project onto the runtime fields: server=%q cmd=%q", m.Server, m.Cmd)
	}
	if m.RAMUsage["gpu0"] != "20GB" || m.MaxConcurrent != 4 {
		t.Errorf("sizing did not project: %+v %d", m.RAMUsage, m.MaxConcurrent)
	}
	if tgt, err := m.ProxyTarget(); err != nil || tgt == nil {
		t.Errorf("proxy did not project, so nothing could route to it: %v", err)
	}
}

// A hand-written duplicate name is the one case auto-naming cannot save, and it
// silently makes two placements one. Refuse it at load.
func TestDuplicatePlacementNamesAreRefused(t *testing.T) {
	_, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  qwen:
    type: chat
    placements:
      - name: same
        server: box1
        cmd: "exec a --port 5800"
        proxy: 5800
      - name: same
        server: box1
        cmd: "exec b --port 5801"
        proxy: 5801
`)
	if err == nil {
		t.Fatal("duplicate placement names were accepted")
	}
	if !strings.Contains(err.Error(), "two placements named") {
		t.Errorf("error should name the collision, got: %v", err)
	}
}

// A placement naming a server that does not exist is a typo that would
// otherwise surface as a model that never schedules.
func TestPlacementOnUnknownServerIsRefused(t *testing.T) {
	_, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  qwen:
    type: chat
    placements:
      - server: nosuchbox
        cmd: "exec a --port 5800"
        proxy: 5800
`)
	if err == nil || !strings.Contains(err.Error(), "unknown server") {
		t.Errorf("want an unknown-server error, got: %v", err)
	}
}

// A pure proxy is served but not run by us, so it has no placements at all.
func TestPureProxyHasNoPlacements(t *testing.T) {
	c, err := loadYAML(t, `
models:
  remote:
    proxy: { host: api.example.com, port: 443 }
    type: chat
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models["remote"].PlacementList(); len(got) != 0 {
		t.Errorf("pure proxy has %d placements, want 0", len(got))
	}
}
