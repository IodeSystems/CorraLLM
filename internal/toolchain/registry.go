package toolchain

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/iodesystems/corrallm/internal/config"
)

// Registry answers "what tool is where, at what version" across every host.
//
// It owns no state about the tools themselves. Every answer is asked of the
// host at the time it is asked, because the alternative — a cache the primary
// keeps — is the thing that made a host's capacity wrong for a whole enrolment:
// remembered facts about someone else's machine go stale silently, and a stale
// version is worse than a slow one here.
type Registry struct {
	// Home is corrallm's home directory; a managed tool installs under
	// <Home>/tools/<name> unless the host's entry overrides it.
	Home string
	// RunnerFor returns how to reach a server. Injected so the primary can hand
	// in agent-backed runners and tests can hand in fakes.
	RunnerFor func(server string) (Runner, error)
	// Cfg reads current config. Called per operation rather than captured, so a
	// config reload is picked up without rebuilding the registry.
	Cfg func() *config.Config
}

// SpecFor builds the recipe input for one tool on one host.
//
// declared=false means this host has no entry for the tool, which is NOT the
// same as the tool being unavailable there. The caller reports the difference;
// nothing here infers one from the other.
func (r *Registry) SpecFor(tool, host string) (spec Spec, declared bool, err error) {
	cfg := r.Cfg()
	if cfg == nil {
		return Spec{}, false, fmt.Errorf("no config loaded")
	}
	t, ok := cfg.Tools[tool]
	if !ok {
		return Spec{}, false, fmt.Errorf("no tool %q declared", tool)
	}
	h, declared := t.Hosts[host]
	if !declared {
		return Spec{}, false, nil
	}
	spec = Spec{
		Name:        tool,
		Recipe:      config.RecipeOf(tool, t),
		URL:         t.URL,
		Ref:         t.Ref,
		Bin:         t.Bin,
		InstalledAt: h.InstalledAt,
	}
	if !h.Adopted() {
		spec.Prefix = h.Prefix
		if spec.Prefix == "" {
			spec.Prefix = filepath.Join(r.Home, "tools", tool)
		}
	}
	return spec, true, nil
}

// Survey reports one tool on one host: what is installed, and how far behind.
//
// Drift is only asked when something is installed. A tool that is absent has no
// local revision to compare, so the ls-remote would cost a network round trip
// to learn nothing.
func (r *Registry) Survey(ctx context.Context, tool, host string) State {
	st := State{Tool: tool, Host: host}
	spec, declared, err := r.SpecFor(tool, host)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Declared = declared
	if !declared {
		return st
	}
	st.Adopted = spec.InstalledAt != ""

	runner, err := r.RunnerFor(host)
	if err != nil {
		st.Error = err.Error()
		return st
	}

	p, err := RunProbe(ctx, runner, spec)
	if err != nil {
		st.Error = err.Error()
		if p != nil {
			st.Probe = p
		}
		return st
	}
	st.Probe = p
	if !p.Present {
		return st
	}

	u, err := RunUpstream(ctx, runner, spec)
	if err != nil {
		// A drift check that could not run does not invalidate the probe. Keep
		// the version — it is the fact that was actually established — and say
		// only the drift is unknown.
		if u != nil && u.Error == "" {
			u.Error = err.Error()
		}
		if u == nil {
			u = &Upstream{Ref: spec.Ref, Error: err.Error()}
		}
	}
	st.Drift = u
	return st
}

// SurveyAll reports every declared (tool, host) pair.
//
// Concurrent across pairs: a survey is dominated by network latency to hosts
// that may be asleep or on the far side of a VPN, and doing them in sequence
// makes the whole listing as slow as the sum of every unreachable machine.
func (r *Registry) SurveyAll(ctx context.Context) []State {
	cfg := r.Cfg()
	if cfg == nil {
		return nil
	}
	type pair struct{ tool, host string }
	var pairs []pair
	for tool, t := range cfg.Tools {
		for host := range t.Hosts {
			pairs = append(pairs, pair{tool, host})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].tool != pairs[j].tool {
			return pairs[i].tool < pairs[j].tool
		}
		return pairs[i].host < pairs[j].host
	})

	out := make([]State, len(pairs))
	var wg sync.WaitGroup
	for i, p := range pairs {
		wg.Add(1)
		go func(i int, p pair) {
			defer wg.Done()
			out[i] = r.Survey(ctx, p.tool, p.host)
		}(i, p)
	}
	wg.Wait()
	return out
}

// Preflight asks whether a host could build a tool.
func (r *Registry) Preflight(ctx context.Context, tool, host string) (*Preflight, error) {
	spec, declared, err := r.SpecFor(tool, host)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, fmt.Errorf("tool %q is not declared on host %q", tool, host)
	}
	runner, err := r.RunnerFor(host)
	if err != nil {
		return nil, err
	}
	return RunPreflight(ctx, runner, spec)
}

// InstallDeps installs what Preflight found missing on a host.
//
// Refused on an ADOPTED entry. Adoption's promise is that corrallm never
// touches an install it does not own, and while installing a system package is
// not writing to that directory, it is still mutating a machine on behalf of a
// tool whose upkeep somebody else owns. If corrallm is going to manage the
// dependencies, it should manage the build.
func (r *Registry) InstallDeps(ctx context.Context, tool, host string) (*InstallDeps, error) {
	spec, declared, err := r.SpecFor(tool, host)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, fmt.Errorf("tool %q is not declared on host %q", tool, host)
	}
	if spec.InstalledAt != "" {
		return nil, fmt.Errorf("tool %q on %q is adopted (installedAt %s) — corrallm does not install dependencies for an install it does not own; drop installedAt to manage it here",
			tool, host, spec.InstalledAt)
	}
	runner, err := r.RunnerFor(host)
	if err != nil {
		return nil, err
	}
	return RunInstallDeps(ctx, runner, spec)
}
