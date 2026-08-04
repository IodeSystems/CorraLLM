package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
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

	// Adopt the addresses the agent just reported, when they differ from what is
	// stored. This is what keeps a machine reachable after it moves — the agent
	// is the only party that can see its own interfaces, and a stored list from
	// enrollment goes stale the first time a laptop changes network.
	//
	// Only on a real change: this runs on every beat from every agent, and
	// rewriting the config file ten times a second per host would be absurd.
	h.adoptEndpoints(in.Body.Server, srv, in.Body.Endpoints)

	out := &AgentHeartbeatOutput{}
	out.Body.OK = true
	out.Body.IntervalSeconds = int(agent.HeartbeatInterval / time.Second)
	// The version of the BINARIES we serve, not this process's own — they are
	// built by separate targets and routinely differ, and the agent compares
	// against what it would actually install.
	out.Body.Version = h.AgentVersion()
	// Tell the agent where ITS platform's binary is, using the OS/arch it just
	// reported. The agent needs no knowledge of the primary's layout, and a
	// mismatched build can never be handed to the wrong machine.
	if hb := in.Body.Hello; hb.OS != "" && hb.Arch != "" {
		out.Body.UpdateURL = fmt.Sprintf("/install/corrallm-%s-%s", hb.OS, hb.Arch)
		// The identity of the binary at that URL, which is what self-update
		// actually compares. Without it two untagged "dev" builds look
		// identical and an agent never picks up a change — the state that
		// matters most while iterating on agent code.
		if h.AgentDist != nil {
			out.Body.BuildID = h.AgentDist.BuildID(hb.OS, hb.Arch)
		}
	}
	return out, nil
}

func bearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return h
}

// AgentVersion is the version stamped into the agent binaries this primary
// serves, falling back to this process's version when none were built.
func (h *Handlers) AgentVersion() string {
	if h.AgentDist != nil {
		return h.AgentDist.ServedVersion()
	}
	return h.Version
}

// adoptEndpoints persists a changed endpoint list and drops the cached client
// built from the old one.
//
// Silent on failure by design: a heartbeat is liveness, and refusing to record
// "this agent is alive" because the config file happens to be hand-written
// would turn a cosmetic limitation into an outage. The agent stays reachable at
// whatever endpoints already worked; only the refresh is skipped.
func (h *Handlers) adoptEndpoints(server string, srv config.Server, reported []string) {
	if len(reported) == 0 || srv.Agent == nil {
		return // an older agent says nothing; leave what we have
	}
	if sameEndpoints(srv.Agent.Endpoints, reported) {
		return
	}
	if h.ConfigPath == "" || requireManaged(h.ConfigPath) != nil {
		return
	}
	err := h.mutateConfig(func(c *config.Config) error {
		cur, ok := c.Servers[server]
		if !ok || cur.Agent == nil {
			return nil
		}
		next := *cur.Agent
		next.Endpoints = append([]string(nil), reported...)
		cur.Agent = &next
		c.Servers[server] = cur
		return nil
	})
	if err != nil {
		slog.Warn("could not record an agent's new endpoints; it stays reachable at the old ones",
			"server", server, "err", err)
		return
	}
	slog.Info("agent endpoints changed", "server", server,
		"was", srv.Agent.Endpoints, "now", reported)
	// The cached client holds the OLD list, so without this the update would be
	// recorded and then ignored until a restart.
	if h.Mgr != nil {
		h.Mgr.InvalidateHost(server)
	}
}

// sameEndpoints compares two address lists ignoring order, since the agent
// enumerates interfaces and their order is not stable across beats — comparing
// as ordered lists would rewrite the config every time they shuffled.
func sameEndpoints(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}
