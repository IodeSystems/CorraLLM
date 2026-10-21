package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

// A mismatched agent is refused, but still told where its update is. Agents
// now self-update ONLY on a protocol difference, so an error reply here would
// leave exactly the agents that need an update unable to find one.
func TestAgentHeartbeat_ProtocolMismatchStillCarriesTheUpdate(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
servers:
  mac1:
    pools: { system: 8GB }
    agent: { endpoints: ["http://192.168.1.42:6503"], token: tok }
`))
	if err != nil {
		t.Fatal(err)
	}
	live := agent.NewLiveness()
	h := &Handlers{Cfg: cfg, Liveness: live}
	in := &AgentHeartbeatInput{Authorization: "Bearer tok"}
	in.Body.Server = "mac1"
	in.Body.Hello = agent.Hello{Protocol: agent.Protocol + 1, OS: "darwin", Arch: "arm64"}

	out, err := h.AgentHeartbeat(context.Background(), in)
	if err != nil {
		t.Fatalf("mismatch answered with an error (%v); the agent could never learn where its update is", err)
	}
	if out.Body.OK || out.Body.Protocol != agent.Protocol || out.Body.UpdateURL == "" {
		t.Errorf("ack = %+v; want ok=false, our protocol, and an update URL", out.Body)
	}
	if _, seen := live.LastSeen("mac1"); seen {
		t.Error("a mismatched agent was recorded as alive; nothing can be spawned on it")
	}

	in.Body.Hello.Protocol = agent.Protocol
	out, err = h.AgentHeartbeat(context.Background(), in)
	if err != nil || !out.Body.OK || out.Body.Protocol != agent.Protocol {
		t.Errorf("matching beat: ack=%+v err=%v", out, err)
	}
}
