package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/gpu"
)

// A server entry for the machine the daemon runs on.
//
// Every spawned model must name a server, and nothing created one for THIS
// machine: other machines get theirs at enrollment, and this one only ever had
// a hand-written entry. A fresh install therefore had no server to pick, and
// "add a llama-server command" could not be saved at all.

// probeLocal measures this machine. A variable so tests can replace it.
var probeLocal = func() (name string, srv config.Server) {
	return agent.ProposedServerName(), localServerFrom(agent.Probe(), probeAllGPUs())
}

func probeAllGPUs() []gpu.Stats {
	devs, err := gpu.ProbeAll()
	if err != nil {
		return nil
	}
	return devs
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
// binding), or "".
func existingLocal(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	var names []string
	for n, s := range cfg.Servers {
		if s.Agent == nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
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
		Existing string `json:"existing" doc:"A server already declared for this machine (no agent binding); empty when there is none."`
		Name     string `json:"name" doc:"The server name adding this machine would use: the hostname."`
		Pools    string `json:"pools" doc:"The memory pools adding this machine would declare, e.g. 'gpu0 24GB, system 64GB'."`
	}
}

// LocalServerProbe reports what AddLocalServer would write, without writing it.
func (h *Handlers) LocalServerProbe(ctx context.Context, _ *struct{}) (*LocalServerProbeOutput, error) {
	out := &LocalServerProbeOutput{}
	out.Body.Existing = existingLocal(h.config())
	name, srv := probeLocal()
	out.Body.Name, out.Body.Pools = name, poolSummary(srv.Pools)
	return out, nil
}

// AddLocalServerOutput names the server that now describes this machine.
type AddLocalServerOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Server  string `json:"server" doc:"The server to put spawned models on."`
	}
}

// AddLocalServer declares this machine as a server, sized from its own probe.
//
// Idempotent: when a server for this machine already exists it is returned
// unchanged, so a second click (or two dialogs) cannot write a duplicate.
func (h *Handlers) AddLocalServer(ctx context.Context, _ *struct{}) (*AddLocalServerOutput, error) {
	out := &AddLocalServerOutput{}
	if n := existingLocal(h.config()); n != "" {
		out.Body.OK, out.Body.Server = true, n
		out.Body.Message = "server " + n + " already describes this machine"
		return out, nil
	}
	name, srv := probeLocal()
	srv.Notes = fmt.Sprintf("Added %s from the dashboard, sized from this machine's own probe. "+
		"With more than one GPU, check that gpu0 is the card llama.cpp calls CUDA0.",
		time.Now().Format(time.RFC3339))
	what := "machine " + name + " added (this machine)"
	err := h.mutateConfig(ctx, what, func(c *config.Config) error {
		if c.Servers == nil {
			c.Servers = map[string]config.Server{}
		}
		if _, ok := c.Servers[name]; ok {
			// A server of this name exists and is an agent (existingLocal saw no
			// local one). Writing over it would move its models here.
			return huma.Error409Conflict(fmt.Sprintf(
				"server %q exists and is another machine (an agent); rename it or add this machine by hand", name))
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
