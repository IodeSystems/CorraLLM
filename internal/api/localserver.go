package api

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
	"github.com/iodesystems/corrallm/internal/proc"
)

// A server entry for the machine the daemon runs on.
//
// Every spawned model must name a server, and nothing created one for THIS
// machine: other machines get theirs at enrollment, and this one only ever had
// a hand-written entry. A fresh install therefore had no server to pick, and
// "add a llama-server command" could not be saved at all.

// probeLocal measures this machine and proposes its server. A variable so
// tests can replace it.
var probeLocal = func(h *Handlers) localProposal {
	var own proc.OwnMemory
	if h != nil && h.Mgr != nil {
		own = h.Mgr.LocalMemory()
	}
	return proposeLocal(agent.ProposedServerName(), agent.Probe(), own)
}

// localServerFrom sizes the entry the same way enrollment does, with one
// difference: this machine can enumerate every card, so each gets a pool bound
// to its UUID. Enrollment sees only the first card.
//
// The largest card becomes gpu0, the pool a measured footprint is charged to.
// That is a guess at CUDA0 (CUDA orders fastest first, nvidia-smi by bus), so
// the notes say to check it.
func localServerFrom(c agent.Capacity, devs []gpu.Stats) config.Server {
	pools, reserve, devicePool := sizeFrom(c)
	srv := config.Server{Pools: pools, Reserve: reserve, DevicePool: devicePool,
		NoProcessMemory: !c.PerProcess}
	if !c.Unified && len(devs) > 1 {
		sorted := append([]gpu.Stats(nil), devs...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TotalMiB > sorted[j].TotalMiB })
		srv.Devices = map[string]string{}
		for i, d := range sorted {
			pool := fmt.Sprintf("gpu%d", i)
			srv.Pools[pool] = bytesToGB(int64(d.TotalMiB) * 1024 * 1024)
			if d.UUID != "" {
				srv.Devices[pool] = d.UUID
			}
		}
		if len(srv.Devices) == 0 {
			srv.Devices = nil
		}
	}
	return srv
}

// existingLocal names a server already declared for this machine (no agent
// binding, or an agent on loopback), or "".
func existingLocal(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	var names []string
	for n, s := range cfg.Servers {
		// An agent on loopback is this machine too: box1 runs its models
		// under a same-machine agent so they outlive the primary. Offering
		// to add this machine again would declare its memory twice.
		if s.Agent == nil || loopbackAgent(s.Agent) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func loopbackAgent(a *config.AgentBinding) bool {
	h := a.Host()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func poolSummary(pools map[string]string) string {
	keys := make([]string, 0, len(pools))
	for k := range pools {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+pools[k])
	}
	return strings.Join(parts, ", ")
}

// LocalServerProbeOutput is what adding this machine would write.
type LocalServerProbeOutput struct {
	Body struct {
		Existing        string      `json:"existing" doc:"A server already declared for this machine (no agent binding, or an agent on loopback); empty when there is none."`
		Name            string      `json:"name" doc:"The server name adding this machine would use: the hostname."`
		Pools           string      `json:"pools" doc:"The memory pools adding this machine would declare, e.g. 'gpu0 24GB, system 64GB'."`
		PoolDetail      []LocalPool `json:"poolDetail" doc:"Each pool adding this machine would declare, with its proposed reserve and how it was reached."`
		DevicePool      string      `json:"devicePool" doc:"The pool a measured GPU footprint is charged to."`
		NoProcessMemory bool        `json:"noProcessMemory" doc:"This machine cannot attribute memory to one process."`
	}
}

// LocalServerProbe reports what AddLocalServer would write, without writing it.
func (h *Handlers) LocalServerProbe(ctx context.Context, _ *struct{}) (*LocalServerProbeOutput, error) {
	out := &LocalServerProbeOutput{}
	out.Body.Existing = existingLocal(h.config())
	p := probeLocal(h)
	out.Body.Name, out.Body.Pools = p.Name, poolSummary(p.Server.Pools)
	out.Body.PoolDetail = p.Pools
	out.Body.DevicePool, out.Body.NoProcessMemory = p.Server.DevicePool, p.Server.NoProcessMemory
	return out, nil
}

// AddLocalServerInput is the server to write, as the operator edited the
// probe's proposal. The body is optional, and so is each field: a missing or
// empty body writes the proposal.
type AddLocalServerInput struct {
	Body *LocalServerEdit
}

// LocalServerEdit is the operator's changes to the probe's proposal.
type LocalServerEdit struct {
	Name       string            `json:"name" required:"false" doc:"Server name; empty uses the probe's (the hostname)."`
	Pools      map[string]string `json:"pools" required:"false" doc:"Pool → size, as config writes it (e.g. {\"gpu0\": \"24GB\"}). Empty uses the probe's pools, reserve, devices and devicePool."`
	Reserve    map[string]string `json:"reserve" required:"false" doc:"Pool → memory kept free for other processes. Only read with pools."`
	Devices    map[string]string `json:"devices" required:"false" doc:"Pool → card UUID it budgets. Only read with pools."`
	DevicePool string            `json:"devicePool" required:"false" doc:"The pool a measured GPU footprint is charged to. Only read with pools; empty uses the probe's when it is one of them."`
	ECC        map[string]bool   `json:"ecc" required:"false" doc:"Pool → ECC, only for a pool where detection is wrong. Absent pools keep detection."`
}

// AddLocalServerOutput names the server that now describes this machine.
type AddLocalServerOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Server  string `json:"server" doc:"The server to put spawned models on."`
	}
}

var (
	serverNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	poolNameRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// localServerEdit applies the operator's edits to the probe's proposal, or
// says what is wrong with them.
func localServerEdit(p localProposal, in *AddLocalServerInput) (string, config.Server, error) {
	name, srv := p.Name, p.Server
	if in == nil || in.Body == nil {
		return name, srv, nil
	}
	b := *in.Body
	if n := strings.TrimSpace(b.Name); n != "" {
		name = n
	}
	if !serverNameRe.MatchString(name) {
		return "", srv, fmt.Errorf("server name %q: use letters, digits, '.', '_' and '-'", name)
	}
	if len(b.Pools) > 0 {
		probeDevicePool := srv.DevicePool
		srv.Pools, srv.Reserve, srv.Devices, srv.DevicePool = b.Pools, b.Reserve, b.Devices, b.DevicePool
		if len(srv.Reserve) == 0 {
			srv.Reserve = nil
		}
		if len(srv.Devices) == 0 {
			srv.Devices = nil
		}
		if srv.DevicePool == "" {
			if _, ok := srv.Pools[probeDevicePool]; ok {
				srv.DevicePool = probeDevicePool
			}
		}
	}
	if len(b.ECC) > 0 {
		srv.ECC = b.ECC
	}

	sizes := map[string]int64{}
	for _, pool := range sortedKeys(srv.Pools) {
		if !poolNameRe.MatchString(pool) {
			return "", srv, fmt.Errorf("pool name %q: use letters, digits, '_' and '-'", pool)
		}
		n, err := config.ParseSize(srv.Pools[pool])
		if err != nil {
			return "", srv, fmt.Errorf("pool %s: %w", pool, err)
		}
		if n <= 0 {
			return "", srv, fmt.Errorf("pool %s: size must be more than 0 GB", pool)
		}
		sizes[pool] = n
	}
	for _, pool := range sortedKeys(srv.Reserve) {
		total, ok := sizes[pool]
		if !ok {
			return "", srv, fmt.Errorf("reserve %s: not one of the pools", pool)
		}
		n, err := config.ParseSize(srv.Reserve[pool])
		if err != nil {
			return "", srv, fmt.Errorf("reserve %s: %w", pool, err)
		}
		if n < 0 {
			return "", srv, fmt.Errorf("reserve %s: must not be negative", pool)
		}
		if n > total {
			return "", srv, fmt.Errorf("reserve %s (%s) is more than the pool (%s)", pool, srv.Reserve[pool], srv.Pools[pool])
		}
	}
	return name, srv, nil
}

// AddLocalServer declares this machine as a server: the probe's proposal, as
// the operator edited it.
//
// Idempotent: when a server for this machine already exists it is returned
// unchanged, so a second click (or two dialogs) cannot write a duplicate.
func (h *Handlers) AddLocalServer(ctx context.Context, in *AddLocalServerInput) (*AddLocalServerOutput, error) {
	out := &AddLocalServerOutput{}
	if n := existingLocal(h.config()); n != "" {
		out.Body.OK, out.Body.Server = true, n
		out.Body.Message = "server " + n + " already describes this machine"
		return out, nil
	}
	name, srv, err := localServerEdit(probeLocal(h), in)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	// Checked here as well as inside the edit: mutateConfig reports every
	// error from the edit as 400, and this one is a conflict.
	if cfg := h.config(); cfg != nil && cfg.Servers[name].Agent != nil {
		return nil, agentNameConflict(name)
	}
	srv.Notes = fmt.Sprintf("Added %s from the dashboard, sized from this machine's own probe. "+
		"With more than one GPU, check that gpu0 is the card llama.cpp calls CUDA0.",
		time.Now().Format(time.RFC3339))
	what := "machine " + name + " added (this machine)"
	err = h.mutateConfig(ctx, what, func(c *config.Config) error {
		if c.Servers == nil {
			c.Servers = map[string]config.Server{}
		}
		if _, ok := c.Servers[name]; ok {
			// A server of this name exists and is an agent (existingLocal saw no
			// local one). Writing over it would move its models here.
			return agentNameConflict(name)
		}
		c.Servers[name] = srv
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Body.OK, out.Body.Server, out.Body.Message = true, name, what+": "+poolSummary(srv.Pools)
	return out, nil
}

func agentNameConflict(name string) error {
	return huma.Error409Conflict(fmt.Sprintf(
		"server %q exists and is another machine (an agent); choose another name", name))
}
