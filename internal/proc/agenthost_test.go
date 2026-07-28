package proc

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/host"
)

// The consequence of getting this wrong is not a failed request, it is the
// WRONG MACHINE doing work: a model declared for the Mac spawned on the
// primary, taking the GPU it was configured to stay off, running a command
// whose binary and weights may not exist here.
func TestHostFor_AgentBoundServerRefusesToSpawnLocally(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  box1:
    pools: { gpu0: 30GB }
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.42:6503"] }
models:
  mac-qwen:
    cmd: "exec rapid-mlx serve"
    server: mac1
    ramUsage: { system: 20GB }
    proxy: 5810
`))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)

	// The durable invariant: an agent-bound server must NEVER resolve to a
	// local host. Which remote implementation backs it may change; falling
	// through to Local must not.
	if _, isLocal := m.hostFor("mac1").(*host.Local); isLocal {
		t.Fatal("hostFor(mac1) resolved to *host.Local — a Mac model would be spawned on the primary")
	}
	// A server with no agent is still local, unchanged.
	if _, ok := m.hostFor("box1").(*host.Local); !ok {
		t.Errorf("hostFor(box1) = %T, want *host.Local", m.hostFor("box1"))
	}

	// And the refusal reaches the caller as a spawn failure that says why,
	// rather than a mystery.
	_, _, _, err = m.EnsureReady(context.Background(), "mac-qwen", cfg.Models["mac-qwen"], nil)
	if err == nil {
		t.Fatal("want an error spawning onto an agent-bound server")
	}
	// The endpoint is unreachable in this test, so the failure must say so
	// rather than surfacing as a mystery — and must not have spawned anything.
	if !strings.Contains(err.Error(), "agent") && !strings.Contains(err.Error(), "192.168.1.42") {
		t.Errorf("err = %v, want it to name the agent or its endpoint", err)
	}
}

// Routing, not just spawning. A model on an agent-bound server must never have
// its traffic sent to the PRIMARY's loopback — the port it names belongs to the
// other machine, and whatever holds that port here is an unrelated process.
func TestEnsureReady_AgentModelTargetIsNotThePrimarysLoopback(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  mac1:
    pools: { system: 64GB }
    devicePool: system
    agent: { endpoints: ["http://192.168.1.42:6503"] }
models:
  mac-qwen:
    cmd: "exec rapid-mlx serve"
    server: mac1
    ramUsage: { system: 20GB }
    proxy: 5810
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.TargetFor("mac-qwen", cfg.Models["mac-qwen"])
	if err != nil {
		t.Fatal(err)
	}
	if config.IsLocalHost(got.URL.Hostname()) {
		t.Fatalf("target = %s — traffic for another machine's model would go to this one", got.URL)
	}
	if want := "http://192.168.1.42:5810"; got.URL.String() != want {
		t.Errorf("target = %s, want %s", got.URL, want)
	}
}
