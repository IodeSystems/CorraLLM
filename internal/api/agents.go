package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
)

// AgentHeartbeatInput is an agent reporting in.
type AgentHeartbeatInput struct {
	// Authorization carries the agent's token. Authenticated against the token
	// configured for THAT server, which makes the token the membership
	// credential: revoke it in config and the agent stops being accepted, while
	// a network outage merely marks it down and changes nothing about the
	// declaration.
	Authorization string `header:"Authorization" doc:"Bearer <the server's configured agent token>."`
	Body          agent.Heartbeat
}

// AgentHeartbeatOutput acknowledges and sets the cadence.
type AgentHeartbeatOutput struct {
	Body agent.HeartbeatAck
}

// AgentHeartbeat records that an agent-backed server is alive.
//
// Deliberately NOT gated by the admin token: an agent is not an operator, and
// handing every machine the credential that can unload models and start bench
// runs to let it say "I am here" would be a bad trade. It presents its own
// per-server token instead, so the blast radius of a compromised agent is that
// one server's liveness.
func (h *Handlers) AgentHeartbeat(_ context.Context, in *AgentHeartbeatInput) (*AgentHeartbeatOutput, error) {
	cfg := h.config()
	if cfg == nil {
		return nil, huma.Error503ServiceUnavailable("config unavailable")
	}
	srv, ok := cfg.Servers[in.Body.Server]
	if !ok || srv.Agent == nil {
		// Naming an unknown server is a misconfiguration on the agent, not an
		// auth failure — say which so it is fixable from the agent's log.
		return nil, huma.Error404NotFound(fmt.Sprintf("no server %q with an agent binding", in.Body.Server))
	}
	want := srv.Agent.ExpandedToken()
	got := bearer(in.Authorization)
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return nil, huma.Error401Unauthorized("agent token rejected")
	}
	if in.Body.Hello.Protocol != 0 && in.Body.Hello.Protocol != agent.Protocol {
		return nil, huma.Error400BadRequest(fmt.Sprintf(
			"agent speaks protocol %d, this daemon speaks %d", in.Body.Hello.Protocol, agent.Protocol))
	}

	h.Liveness.Beat(in.Body.Server, time.Now())

	// Reconcile on every beat. The heartbeat already carries what the agent is
	// running, so this costs nothing extra and is a no-op in the steady state;
	// the case it exists for is the beat right after a reconnect, where the two
	// sides may disagree about what is alive.
	if h.Mgr != nil {
		if a, r, v := h.Mgr.ReconcileAgent(context.Background(), in.Body.Server, in.Body.Backends); r > 0 || v > 0 {
			slog.Warn("agent reconciliation corrected state",
				"server", in.Body.Server, "adopted", a, "reaped", r, "vanished", v)
		}
	}

	out := &AgentHeartbeatOutput{}
	out.Body.OK = true
	out.Body.IntervalSeconds = int(agent.HeartbeatInterval / time.Second)
	return out, nil
}

func bearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return h
}
