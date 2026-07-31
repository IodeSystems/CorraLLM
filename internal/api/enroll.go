package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
)

// AgentEnrollInput is a machine attaching itself with a one-time token.
type AgentEnrollInput struct {
	Authorization string `header:"Authorization" doc:"Bearer <enrollment token>, minted by the operator."`
	Body          agent.EnrollRequest
}

// AgentEnrollOutput returns the long-lived per-server credential.
type AgentEnrollOutput struct {
	Body agent.EnrollResponse
}

// AgentEnroll attaches a machine, creating its server entry from what the
// machine reports about itself.
//
// This is the inversion that makes attaching a host one command. Before it, the
// heartbeat authenticated against a token in a server entry the operator had
// already written, so a machine could not join anything that did not already
// describe it — you wrote the pools, the endpoints and the token by hand, then
// went to the machine. Now the agent brings its own measurements and the
// primary writes the entry.
//
// Not gated by the admin token: an enrolling machine has no operator
// credential, which is the whole point. It presents a one-time enrollment token
// instead, and that token is consumed atomically so two machines racing it
// cannot both attach as the same server.
func (h *Handlers) AgentEnroll(_ context.Context, in *AgentEnrollInput) (*AgentEnrollOutput, error) {
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no store: enrollment unavailable")
	}
	if h.ConfigPath == "" {
		return nil, huma.Error503ServiceUnavailable("this daemon has no writable config; enrollment cannot record the server")
	}
	// Refuse to rewrite a config corrallm does not own. A hand-written file is
	// mostly commentary, and a marshaller would delete all of it — the operator
	// migrates first (corrallm config import) and knows they did.
	if err := requireManaged(h.ConfigPath); err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}

	who := in.Body.Hello.Hostname
	if who == "" {
		who = in.Body.Server
	}
	// PEEK, do not claim yet. Everything that can reject the request runs
	// first, because the token is single-use: consuming it and then failing
	// means a typo costs a credential and the operator has to mint another.
	claim, err := h.Store.PeekEnrollmentToken(bearer(in.Authorization))
	if err != nil {
		return nil, huma.Error401Unauthorized(err.Error())
	}

	name := strings.TrimSpace(claim.Server)
	if name == "" {
		name = strings.TrimSpace(in.Body.Server)
	}
	if name == "" {
		// The agent proposes its hostname when nothing else names it, so this
		// should be unreachable; keep it as a clear message rather than
		// creating a server called "".
		return nil, huma.Error400BadRequest(
			"no server name: mint the token with a server, pass --server, or set `server:` in agent.yml")
	}

	cfg := h.config()
	if cfg == nil {
		return nil, huma.Error503ServiceUnavailable("config unavailable")
	}
	if existing, ok := cfg.Servers[name]; ok && existing.Agent == nil {
		// Refuse to convert a LOCAL server into a remote one. Doing so would
		// silently move every model declared on it to another machine.
		return nil, huma.Error409Conflict(fmt.Sprintf(
			"server %q already exists and is local; enrolling would move its models to another machine", name))
	}

	tok, err := newAgentToken()
	if err != nil {
		return nil, huma.Error500InternalServerError("could not mint an agent token", err)
	}

	srv := config.Server{
		Pools:      poolsFrom(in.Body.Capacity),
		DevicePool: devicePoolFrom(in.Body.Capacity),
		// Recorded at enrollment because the agent is the only one who knows,
		// and the consequence of getting it wrong is silent single-tenancy.
		NoProcessMemory: !in.Body.Capacity.PerProcess,
		Agent:      &config.AgentBinding{Endpoints: in.Body.Endpoints, Token: tok},
		Notes: fmt.Sprintf("Enrolled %s from %s (%s/%s), corrallm %s. Pools sized from the agent's own capacity probe.",
			time.Now().Format(time.RFC3339), in.Body.Hello.Hostname,
			in.Body.Hello.OS, in.Body.Hello.Arch, in.Body.Hello.Version),
	}

	next := shallowCopyConfig(cfg)
	if next.Servers == nil {
		next.Servers = map[string]config.Server{}
	}
	// Preserve anything the operator already set on this server (a hand-tuned
	// pool, a note) — enrollment fills in what is missing, it does not reset.
	if prev, ok := next.Servers[name]; ok {
		if len(prev.Pools) > 0 {
			srv.Pools = prev.Pools
		}
		if prev.DevicePool != "" {
			srv.DevicePool = prev.DevicePool
		}
		if prev.Notes != "" {
			srv.Notes = prev.Notes + "\n\n" + srv.Notes
		}
		srv.MaxConcurrent = prev.MaxConcurrent
		srv.Reserve = prev.Reserve
	}
	next.Servers[name] = srv

	if err := config.SaveValidated(h.ConfigPath, next); err != nil {
		return nil, huma.Error400BadRequest("enrollment would produce an invalid config", err)
	}
	// Claim LAST: everything that could reject this has passed, so the
	// single-use token is spent only on an enrollment that actually happened.
	// The conditional update still settles a race between two agents.
	if _, err := h.Store.ClaimEnrollmentToken(bearer(in.Authorization), who); err != nil {
		return nil, huma.Error401Unauthorized(err.Error())
	}
	if h.Reload != nil {
		if err := h.Reload(); err != nil {
			slog.Error("enrolled, but the reload failed — restart to pick it up", "server", name, "err", err)
		}
	}
	slog.Info("agent enrolled", "server", name, "host", in.Body.Hello.Hostname,
		"os", in.Body.Hello.OS, "arch", in.Body.Hello.Arch, "pools", srv.Pools)

	out := &AgentEnrollOutput{}
	out.Body = agent.EnrollResponse{Server: name, Token: tok, Pools: srv.Pools}
	return out, nil
}

// requireManaged refuses to write a config corrallm did not author.
func requireManaged(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the config at %s: %v", path, err)
	}
	if !strings.Contains(string(b), "MANAGED CONFIG") {
		return fmt.Errorf("%s is hand-written; corrallm will not rewrite it and lose its comments. "+
			"Run `corrallm config import %s` first, then point the daemon at the managed file", path, path)
	}
	return nil
}

func newAgentToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "agt_" + hex.EncodeToString(raw), nil
}

// poolsFrom sizes a server from what the machine measured about itself.
//
// A unified-memory host reports one pool because it HAS one; a discrete-GPU box
// reports its card separately from host RAM. Deriving this beats asking the
// operator to type it, which is where the wrong number usually comes from.
func poolsFrom(c agent.Capacity) map[string]string {
	pools := map[string]string{}
	if c.Host != nil && c.Host.TotalBytes > 0 {
		pools["system"] = bytesToGB(c.Host.TotalBytes)
	}
	if c.GPU != nil && c.GPU.TotalBytes > 0 {
		pools["gpu0"] = bytesToGB(c.GPU.TotalBytes)
	}
	if len(pools) == 0 {
		// The agent could not measure anything (macOS today). Leave it unsized
		// rather than inventing a number: an invented pool is a budget the
		// scheduler will admit against, and being wrong there means OOM.
		pools["system"] = "0"
	}
	return pools
}

// devicePoolFrom names the pool a measured footprint is charged against.
func devicePoolFrom(c agent.Capacity) string {
	if c.GPU != nil && c.GPU.TotalBytes > 0 {
		return "gpu0"
	}
	return "system" // unified memory, or no discrete device
}

func bytesToGB(b int64) string {
	const gb = 1000 * 1000 * 1000
	return fmt.Sprintf("%dGB", b/gb)
}

// shallowCopyConfig copies the maps that enrollment touches, so a failed
// validation never leaves the LIVE config mutated.
func shallowCopyConfig(c *config.Config) *config.Config {
	out := *c
	out.Servers = make(map[string]config.Server, len(c.Servers)+1)
	for k, v := range c.Servers {
		out.Servers[k] = v
	}
	return &out
}

// MintEnrollmentTokenInput asks for a new one-time token.
type MintEnrollmentTokenInput struct {
	// ForwardedHost is how the caller reached this daemon, when something in
	// front says so. A fallback only.
	//
	// Note it is NOT `header:"Host"`: Go moves the Host header out of
	// r.Header and onto r.Host, so binding it that way silently yields an empty
	// string and an install command with no address in it. --public-base is the
	// reliable answer; this covers a reverse proxy that sets the header.
	ForwardedHost string `header:"X-Forwarded-Host" required:"false"`
	Body struct {
		Server     string `json:"server" doc:"Server name this token may claim. Empty lets the enrolling agent propose one."`
		Note       string `json:"note" doc:"Free text, e.g. what machine this is for."`
		// Base is where the ATTACHING machine should reach this daemon.
		//
		// The dashboard sends its own origin, which is the most reliable answer
		// available: it is literally the address that just worked, with the
		// right scheme and port and whatever proxy sits in front. The server
		// cannot derive this as well — Go moves the Host header onto r.Host and
		// out of r.Header, so binding it yields nothing, and a configured
		// --public-base goes stale the moment the daemon is reached another way.
		Base string `json:"base" required:"false" doc:"Base URL the attaching machine should use. The dashboard sends its own origin; falls back to the daemon's --public-base."`
		TTLMinutes int    `json:"ttlMinutes" doc:"Validity window; default 60."`
	}
}

// MintEnrollmentTokenOutput carries the plaintext ONCE.
type MintEnrollmentTokenOutput struct {
	Body struct {
		Token   string `json:"token" doc:"The enrollment token. Shown once — only its hash is stored."`
		Server  string `json:"server" doc:"Server it may claim, if fixed."`
		Expires int64  `json:"expires" doc:"Unix millis after which it stops working."`
		Command string `json:"command" doc:"The full install command to run on the machine being attached."`
	}
}

// MintEnrollmentToken issues a one-time credential for attaching a machine.
func (h *Handlers) MintEnrollmentToken(_ context.Context, in *MintEnrollmentTokenInput) (*MintEnrollmentTokenOutput, error) {
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no store: enrollment unavailable")
	}
	ttl := time.Duration(in.Body.TTLMinutes) * time.Minute
	if ttl <= 0 {
		ttl = time.Hour
	}
	tok, err := h.Store.NewEnrollmentToken(in.Body.Server, in.Body.Note, ttl)
	if err != nil {
		return nil, huma.Error500InternalServerError("could not mint a token", err)
	}
	out := &MintEnrollmentTokenOutput{}
	out.Body.Token = tok
	out.Body.Server = in.Body.Server
	out.Body.Expires = time.Now().Add(ttl).UnixMilli()
	// Caller-supplied first: the browser knows how it reached us better than we
	// do. Then a configured base, then a proxy's forwarded host.
	base := strings.TrimRight(strings.TrimSpace(in.Body.Base), "/")
	if base == "" {
		base = h.PublicBase
	}
	if base == "" && in.ForwardedHost != "" {
		base = "http://" + in.ForwardedHost
	}
	if base == "" {
		// Better a visible placeholder than a command that silently curls
		// nothing: the operator sees what to fix.
		base = "<set --public-base on the daemon>"
	}
	srvArg := ""
	if in.Body.Server != "" {
		srvArg = " --server " + in.Body.Server
	}
	// The whole point is that this is copy-pasteable onto the machine.
	// `sh`, not `bash`: the script is POSIX and the machine being attached may
	// not have bash at all (or may simply not want it).
	out.Body.Command = fmt.Sprintf("curl -fsSL %s/install.sh | sh -s --%s --token %s", base, srvArg, tok)
	return out, nil
}
