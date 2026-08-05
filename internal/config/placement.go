package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A PLACEMENT is one way of running a model: a box, and the command that runs
// it there.
//
// The model is the routing identity — the name callers ask for, its quality
// tier, its cost class. How that name gets served is a separate thing, and there
// can be several: the same weights on two machines, or the same machine running
// two quantisations. Those are not interchangeable. A Q4 on an M1 Max and a Q6
// on a 5090 have different footprints, different context windows, different
// throughput, and — the part that keeps biting — different CAPABILITIES, since
// whether vision works depends on whether that cmd loaded an mmproj.
//
// corrallm made this mistake twice before arriving here. ramUsage was declared
// per model when footprint is per (model, device), which is why the tune cache
// has always been keyed by device name rather than by model. Modalities are
// declared per model today and have exactly the same shape. Both are properties
// of a placement wearing a model's clothes.
//
// Backwards compatible by construction: a model written the old way — cmd,
// server, proxy and ramUsage directly on the model — normalises to a single
// placement at load, so every existing config means precisely what it did
// before.
type Placement struct {
	// Name identifies this placement stably, because things get FILED under it:
	// the process key, the measured profile, the probed capabilities. Derived
	// from the server when omitted, which is unambiguous until one server hosts
	// the same model twice.
	Name string `yaml:"name,omitempty"`

	// Server is the box. Required — a placement is a way of running something
	// somewhere, and "somewhere" is the half a model cannot supply.
	Server string `yaml:"server,omitempty"`

	// Cmd is what runs there. Two placements on ONE server differ by this, and
	// that difference is the point: a quant, a context size, a flag that turns
	// a projector on.
	Cmd string `yaml:"cmd,omitempty"`

	// Proxy is where that process listens. Per placement because the port is a
	// property of the process, and two boxes routinely use different ones.
	Proxy yaml.Node `yaml:"proxy,omitempty"`

	// RAMUsage is an optional bootstrap hint, superseded by measurement. Per
	// placement because the same weights cost different amounts on different
	// hardware — the whole reason it could never be a model-level number.
	RAMUsage map[string]string `yaml:"ramUsage,omitempty"`

	// MaxConcurrent is admission slots for THIS process. A 5090 may serve four
	// where a laptop serves one.
	MaxConcurrent int `yaml:"maxConcurrent,omitempty"`

	// ContextPerRequest is the window one request may use here. Per placement
	// for the same reason: it is a launch flag, and the flag differs per box.
	ContextPerRequest int `yaml:"contextPerRequest,omitempty"`

	// Swap is the measured load cost of this placement.
	Swap *Swap `yaml:"swap,omitempty"`
}

// PlacementList returns this model's placements, normalising the legacy
// single-server shape into one.
//
// Every caller should go through this rather than reading Model.Server, so that
// a config written either way behaves identically. The legacy fields remain
// populated for the call sites not yet migrated; they describe the FIRST
// placement, which for an old-style config is the only one.
func (m Model) PlacementList() []Placement {
	if len(m.Placements) > 0 {
		out := make([]Placement, len(m.Placements))
		copy(out, m.Placements)
		for i := range out {
			if out[i].Name == "" {
				out[i].Name = defaultPlacementName(out, i)
			}
		}
		return out
	}
	if m.Server == "" && m.Cmd == "" {
		return nil // a pure proxy: served, but not RUN anywhere by us
	}
	return []Placement{{
		Name: m.Server, Server: m.Server, Cmd: m.Cmd, Proxy: m.Proxy,
		RAMUsage: m.RAMUsage, MaxConcurrent: m.MaxConcurrent,
		ContextPerRequest: m.ContextPerRequest, Swap: m.Swap,
	}}
}

// defaultPlacementName derives a stable name when none was written.
//
// The server alone is the obvious choice and is right until one box hosts the
// same model twice — two quantisations, say — at which point it would collide
// and two placements would share a process key, a profile and a capability
// record. Disambiguated by ordinal, which is stable as long as the list order
// is; anyone reordering placements should name them.
func defaultPlacementName(all []Placement, i int) string {
	srv := all[i].Server
	if srv == "" {
		srv = "unbound"
	}
	n := 0
	for j := range all {
		if all[j].Server == all[i].Server {
			if j == i {
				break
			}
			n++
		}
	}
	dup := false
	for j := range all {
		if j != i && all[j].Server == all[i].Server {
			dup = true
			break
		}
	}
	if !dup {
		return srv
	}
	return fmt.Sprintf("%s-%d", srv, n+1)
}

// PlacementOn returns the placement for a server, if the model has one there.
func (m Model) PlacementOn(server string) (Placement, bool) {
	for _, p := range m.PlacementList() {
		if p.Server == server {
			return p, true
		}
	}
	return Placement{}, false
}

// PlacementNamed returns a placement by its stable name.
func (m Model) PlacementNamed(name string) (Placement, bool) {
	for _, p := range m.PlacementList() {
		if p.Name == name {
			return p, true
		}
	}
	return Placement{}, false
}

// ProcKey is the identity of the process backing ONE placement.
//
// Distinct from Model.ProcKey, which keys by served name and therefore cannot
// tell two placements of the same model apart — they would share a process
// slot, so loading one would appear to load the other and unloading either
// would free a reservation the other still held.
//
// An extension still wins: its models share a process by definition, and that
// grouping is what makes them load and unload together.
func (m Model) PlacementProcKey(served string, p Placement) string {
	if m.Extension != "" && m.ExtensionHosted {
		return "extension:" + m.Extension
	}
	if p.Name == "" || len(m.Placements) == 0 {
		// Legacy single-placement model: keep the historical key exactly, so
		// nothing that persisted it (residency, reconciliation on a running
		// agent) is orphaned by the upgrade.
		return served
	}
	return served + "@" + p.Name
}

// ValidatePlacements checks the structural rules a placement list must satisfy.
func (m Model) ValidatePlacements(served string) error {
	ps := m.PlacementList()
	if len(ps) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Cmd != "" && p.Server == "" {
			return fmt.Errorf("model %q placement %q: a spawn command needs a server to run on",
				served, p.Name)
		}
		if seen[p.Name] {
			// Two placements sharing a name would share a process key, a
			// measured profile and a capability record — silently, and with the
			// second overwriting the first.
			return fmt.Errorf("model %q: two placements named %q; name them distinctly",
				served, p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// PlacementServers lists the boxes this model can run on, sorted.
func (m Model) PlacementServers() []string {
	var out []string
	for _, p := range m.PlacementList() {
		if p.Server != "" && !contains(out, p.Server) {
			out = append(out, p.Server)
		}
	}
	sort.Strings(out)
	return out
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// ForPlacement returns this model as it is when served BY that placement.
//
// The alternative was threading a Placement through three dozen call sites that
// already read Model.Server/Cmd/Proxy — admission, residency, target
// resolution, tuning, reconciliation. Resolving once at the entry point makes
// every one of those reads mean "the placement we chose" with no change to any
// of them, which is both less churn and less opportunity to migrate one site
// and forget its neighbour.
//
// The returned Model is a COPY. Config values are shared across goroutines and
// requests, so mutating in place would rewrite the model every other caller
// sees.
func (m Model) ForPlacement(p Placement) Model {
	m.Server = p.Server
	m.Cmd = p.Cmd
	if !p.Proxy.IsZero() {
		m.Proxy = p.Proxy
	}
	if len(p.RAMUsage) > 0 {
		m.RAMUsage = p.RAMUsage
	}
	if p.MaxConcurrent > 0 {
		m.MaxConcurrent = p.MaxConcurrent
	}
	if p.ContextPerRequest > 0 {
		m.ContextPerRequest = p.ContextPerRequest
	}
	if p.Swap != nil {
		m.Swap = p.Swap
	}
	return m
}
