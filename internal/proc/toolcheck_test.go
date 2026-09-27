package proc

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
)

func toolCheckCfg(t *testing.T, yaml string) *Manager {
	t.Helper()
	cfg, err := config.LoadBytesForTest([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	// Stands in for the toolchain registry: the active build's directory.
	m.ExpandCmd = func(_ context.Context, cmd, _ string) (string, error) {
		return strings.ReplaceAll(cmd, "${tool:llama.cpp}", "/fake/bin"), nil
	}
	return m
}

// The model to check with runs THIS tool, on THIS server, and is not running:
// a running one holds the port the trial would bind, and its failure to bind
// would roll back a good build.
func TestToolCheckPicksAnIdleModelThatRunsTheTool(t *testing.T) {
	m := toolCheckCfg(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
  box2: { pools: { gpu0: 30GB } }
models:
  a-running:  { cmd: "${tool:llama.cpp}/llama-server --port 5800", server: box1, proxy: 5800, ramUsage: { gpu0: 1GB } }
  b-idle:     { cmd: "${tool:llama.cpp}/llama-server --port 5801", server: box1, proxy: 5801, ramUsage: { gpu0: 1GB } }
  c-idle:     { cmd: "${tool:llama.cpp}/llama-server --port 5802", server: box1, proxy: 5802, ramUsage: { gpu0: 1GB } }
  a-elsewhere: { cmd: "${tool:llama.cpp}/llama-server --port 5803", server: box2, proxy: 5803, ramUsage: { gpu0: 1GB } }
  a-othertool: { cmd: "${tool:ninfer}/ninfer-serve --port 5804", server: box1, proxy: 5804, ramUsage: { gpu0: 1GB } }
`)
	m.mu.Lock()
	m.procs["a-running"] = &Process{Name: "a-running", key: "a-running", server: "box1",
		state: StateReady, ready: make(chan struct{})}
	m.mu.Unlock()

	name, mdl, ok := m.toolCheckModel("llama.cpp", "box1")
	if !ok || name != "b-idle" {
		t.Fatalf("picked %q (ok=%v), want b-idle", name, ok)
	}
	if mdl.Server != "box1" {
		t.Errorf("server = %q", mdl.Server)
	}
	if _, _, ok := m.toolCheckModel("stable-diffusion.cpp", "box1"); ok {
		t.Error("picked a model for a tool no model uses")
	}
}

// No idle model means the build could not be checked — not that it is broken.
func TestToolCheckWithNothingToRunIsUnchecked(t *testing.T) {
	m := toolCheckCfg(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
models: {}
`)
	ran, err := m.CheckTool(context.Background(), "llama.cpp", "box1", &bytes.Buffer{})
	if ran || err == nil {
		t.Fatalf("ran=%v err=%v; want not ran, with the reason", ran, err)
	}
}

// The outage this exists for: the binary cannot load its libraries and exits
// 127 at once. The check must report a build that was run and failed.
func TestToolCheckReportsABackendThatDiesOnStart(t *testing.T) {
	m := toolCheckCfg(t, `
servers:
  box1: { pools: { gpu0: 30GB } }
models:
  small: { cmd: "echo '${tool:llama.cpp}/llama-server: error while loading shared libraries' >&2; exit 127", server: box1, proxy: 59999, ramUsage: { gpu0: 1GB } }
`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var log bytes.Buffer
	ran, err := m.CheckTool(ctx, "llama.cpp", "box1", &log)
	if !ran || err == nil {
		t.Fatalf("ran=%v err=%v; want ran and failed\n%s", ran, err, log.String())
	}
	if !strings.Contains(log.String(), "/fake/bin/llama-server") {
		t.Errorf("the command was not expanded against the active build, or its output was not logged:\n%s", log.String())
	}
	if m.StateOf(TrialKeyPrefix+"tool-check-small") != StateAbsent {
		t.Error("the check left its trial registered")
	}
}
