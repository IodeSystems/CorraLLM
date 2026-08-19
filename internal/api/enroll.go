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

	pools, reserve, devicePool := sizeFrom(in.Body.Capacity)
	srv := config.Server{
		Pools:      pools,
		Reserve:    reserve,
		DevicePool: devicePool,
		// Recorded at enrollment because the agent is the only one who knows,
		// and the consequence of getting it wrong is silent single-tenancy.
		NoProcessMemory: !in.Body.Capacity.PerProcess,
		Agent:           &config.AgentBinding{Endpoints: in.Body.Endpoints, Token: tok},
		Notes: fmt.Sprintf("Enrolled %s from %s (%s/%s), corrallm %s. Pools sized from the agent's own capacity probe.",
			time.Now().Format(time.RFC3339), in.Body.Hello.Hostname,
			in.Body.Hello.OS, in.Body.Hello.Arch, in.Body.Hello.Version),
	}

	// Through the same funnel as every other edit. Enrollment used to read the
	// config, add its server and save — with the read outside the write, so a
	// machine attaching while somebody edited a model in the dashboard could
	// erase that edit, or be erased by it. The merge happens INSIDE the
	// transaction now, against whatever the config actually is at that moment.
	if err := h.applyEdit(func(next *config.Config) error {
		if next.Servers == nil {
			next.Servers = map[string]config.Server{}
		}
		s := srv
		if prev, ok := next.Servers[name]; ok {
			s = mergeEnrollment(prev, s)
		}
		next.Servers[name] = s
		return nil
	}); err != nil {
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

// sizeFrom turns a machine's own measurements into a capacity declaration: the
// pools it offers, the headroom kept off-limits, and which pool a measured
// device footprint is charged against.
//
// Deriving this beats asking the operator to type it, which is where the wrong
// number usually comes from.
//
// One function rather than three because the three answers are ONE decision,
// and when they were separate they disagreed. Pools were declared from both
// measurements while the device pool was picked from "is there a GPU" — so on
// Apple silicon, where the GPU probe reports a slice of the same RAM sysmem
// reports, a 64 GiB Mac enrolled as `{system: 68GB, gpu0: 51GB}`: 119 GB across
// two ledgers backed by 68 GB. fitsLocked checks each pool independently and
// nothing relates them, so the overcommit is undetectable until a spawn OOMs —
// on the one class of host that also cannot measure per-process memory, and so
// cannot produce the profile that would have shown the drift.
func sizeFrom(c agent.Capacity) (pools, reserve map[string]string, devicePool string) {
	host, gpuMem := int64(0), int64(0)
	if c.Host != nil {
		host = c.Host.TotalBytes
	}
	if c.GPU != nil {
		gpuMem = c.GPU.TotalBytes
	}

	if !c.Unified {
		pools = map[string]string{}
		if host > 0 {
			pools["system"] = bytesToGB(host)
		}
		if gpuMem > 0 {
			pools["gpu0"] = bytesToGB(gpuMem)
			devicePool = "gpu0"
		}
		if len(pools) == 0 {
			// Nothing measured. Leave it unsized rather than inventing a
			// number: an invented pool is a budget the scheduler will admit
			// against, and being wrong there means OOM.
			pools["system"] = "0"
		}
		if devicePool == "" {
			devicePool = "system" // no discrete device to charge against
		}
		return pools, nil, devicePool
	}

	// Unified memory: ONE pool, because there is one. Prefer the host figure —
	// hw.memsize is the machine, while the GPU reading is a ceiling within it.
	total := host
	if total <= 0 {
		total = gpuMem // host probe failed; the wired limit is all we know
	}
	if total <= 0 {
		return map[string]string{"system": "0"}, nil, "system"
	}

	// The wired limit is not a second pool; it is a CEILING on the one pool, so
	// it becomes reserve. Budget (total − reserve) then lands at or just under
	// the wired limit, and the memory macOS will not let a backend wire is
	// described as what it is: headroom, visible and adjustable, rather than a
	// phantom budget the scheduler would happily fill.
	//
	// The floor covers an operator who raised iogpu.wired_limit_mb to (or past)
	// physical memory. Their machine still needs to run macOS, and a reserve of
	// zero would let corrallm admit against every byte the box has.
	res := total - gpuMem
	if floor := total / 8; res < floor {
		res = floor
	}
	// Asymmetric rounding, deliberately: a pool floors so it never claims more
	// memory than exists, and reserve ceils so headroom is never understated.
	// The two compound, so the budget can sit up to 2 GB below the wired limit
	// — both errors are in the safe direction, and readable GB in a file the
	// operator edits by hand is worth more than that last gigabyte.
	return map[string]string{"system": bytesToGB(total)},
		map[string]string{"system": bytesToGBUp(res)},
		"system"
}

// mergeEnrollment settles a fresh enrollment against an existing server entry.
//
// RE-ENROLLING RE-DERIVES THE SIZING. Pools, reserve and devicePool come from
// the measurement the agent just sent, overwriting whatever was there.
//
// This used to preserve them, which sounds protective and is not: sizing is
// DERIVED, so preserving it pinned a server to whatever its FIRST enrollment
// computed — including, for every Mac enrolled before sizeFrom was fixed, a
// pool shape that counted unified memory twice. Nothing could correct that short
// of hand-editing YAML, while the operator who re-ran the install command
// reasonably expected that to be the fix. Re-measuring a machine is the entire
// purpose of enrollment; declining to apply the new measurement made the
// operation a no-op precisely when it was needed.
//
// Safe because re-enrolling cannot happen by accident. It requires a one-time
// token the operator just minted and a command they just ran on the machine —
// a deliberate "measure this box again", and the only path to here.
//
// What survives is what the agent CANNOT know: notes, and maxConcurrent, which
// is operator policy about how hard to drive the host rather than a fact about
// it. A resize is appended to the notes so an operator who HAD hand-tuned a pool
// can see that it changed, and to what.
func mergeEnrollment(prev, fresh config.Server) config.Server {
	fresh.MaxConcurrent = prev.MaxConcurrent
	if prev.Notes != "" {
		fresh.Notes = prev.Notes + "\n\n" + fresh.Notes
	}
	if len(prev.Pools) > 0 && !sameSizes(prev.Pools, fresh.Pools) {
		fresh.Notes += fmt.Sprintf("\nRe-enrollment resized this server: pools %v → %v, devicePool %q → %q.",
			prev.Pools, fresh.Pools, prev.DevicePool, fresh.DevicePool)
	}
	return fresh
}

// sameSizes compares two pool declarations by VALUE, so "68GB" and "68GB"
// written by two different enrollments are the same and do not produce a note
// about a change that did not happen.
func sameSizes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

const gb = 1000 * 1000 * 1000

func bytesToGB(b int64) string   { return fmt.Sprintf("%dGB", b/gb) }
func bytesToGBUp(b int64) string { return fmt.Sprintf("%dGB", (b+gb-1)/gb) }

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
	Body          struct {
		Server string `json:"server" doc:"Server name this token may claim. Empty lets the enrolling agent propose one."`
		Note   string `json:"note" doc:"Free text, e.g. what machine this is for."`
		// Base is where the ATTACHING machine should reach this daemon.
		//
		// The dashboard sends its own origin, which is the most reliable answer
		// available: it is literally the address that just worked, with the
		// right scheme and port and whatever proxy sits in front. The server
		// cannot derive this as well — Go moves the Host header onto r.Host and
		// out of r.Header, so binding it yields nothing, and a configured
		// --public-base goes stale the moment the daemon is reached another way.
		Base       string `json:"base" required:"false" doc:"Base URL the attaching machine should use. The dashboard sends its own origin; falls back to the daemon's --public-base."`
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
