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
func TestModelOnTwoBoxesKeepsThemDistinct(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 64GB }
    devicePool: system
models:
  qwen:
    type: chat
    quality: 2
    placements:
      - server: box1
        cmd: "exec llama-server -m qwen-q6.gguf --port 5800"
        proxy: 5800
        ramUsage: { gpu0: 20GB }
        maxConcurrent: 4
      - server: mac1
        cmd: "exec llama-server -m qwen-q4.gguf --port 5810"
        proxy: 5810
        maxConcurrent: 1
`)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models["qwen"]
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
	k1 := m.PlacementProcKey("qwen", ps[0])
	k2 := m.PlacementProcKey("qwen", ps[1])
	if k1 == k2 {
		t.Errorf("both placements key to %q — they would share a process slot", k1)
	}
	// Per-placement sizing is preserved, which is the reason these cannot be
	// one entry: a 5090 serves four where a laptop serves one.
	byServer := map[string]Placement{}
	for _, p := range ps {
		byServer[p.Server] = p
	}
	if byServer["box1"].MaxConcurrent != 4 || byServer["mac1"].MaxConcurrent != 1 {
		t.Errorf("per-placement slots collapsed: %+v", byServer)
	}
	if byServer["mac1"].RAMUsage != nil {
		t.Error("mac1 declared no ramUsage and must not inherit box1's")
	}
}

// The same box twice — two quantisations — is legal and must not collide.
func TestTwoPlacementsOnOneBoxGetDistinctNames(t *testing.T) {
	c, err := loadYAML(t, `
servers:
  box1:
    pools: { gpu0: 30GB }
models:
  qwen:
    type: chat
    placements:
      - server: box1
        cmd: "exec llama-server -m qwen-q4.gguf --port 5800"
        proxy: 5800
      - server: box1
        cmd: "exec llama-server -m qwen-q6.gguf --port 5801"
        proxy: 5801
`)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models["qwen"]
	ps := m.PlacementList()
	if ps[0].Name == ps[1].Name {
		t.Fatalf("both placements named %q — profile, capabilities and process key would collide", ps[0].Name)
	}
	if m.PlacementProcKey("qwen", ps[0]) == m.PlacementProcKey("qwen", ps[1]) {
		t.Error("same-box placements share a process key")
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
