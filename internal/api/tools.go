package api

import (
	"context"
	"fmt"

	"github.com/iodesystems/corrallm/internal/toolchain"
)

// The toolchain surface for the dashboard (P25b).
//
// Everything about tools was CLI-only through P25a/c/e, which is defensible for
// a build you supervise and wrong for the question the registry exists to
// answer: "what is installed where, and is it stale". That belongs on a screen
// somebody already has open.
//
// A survey ASKS every host, so it is slow by nature — a fork and an exec per
// tool locally, an HTTP round trip per tool remotely, plus one `git ls-remote`
// each. It is deliberately not folded into an existing page's query: making the
// hosts page wait on a sleeping laptop to render its capacity would be a bad
// trade, and this is the one call that can legitimately take seconds.

// ToolStatesInput asks for the registry's view.
type ToolStatesInput struct {
	Drift bool `query:"drift" doc:"Also ask upstream whether each pin has moved. One git ls-remote per installed tool; skip it for a fast render."`
}

// ToolStateView is one tool on one host, flattened for the UI.
//
// Flattened rather than nested because the table is the point: a row per
// (tool, host) is what an operator scans. The nesting in toolchain.State exists
// to keep "could not ask" distinct from "asked, and the answer is no", and that
// distinction survives here as separate fields rather than an empty string
// meaning two different things.
type ToolStateView struct {
	Tool string `json:"tool"`
	Host string `json:"host"`
	// Declared is false for a host with no entry for this tool. NOT the same as
	// unavailable — "it can never run here" and "nobody has said yet" are
	// different facts, and the UI must not render them alike.
	Declared bool `json:"declared"`
	// Adopted means corrallm does not own this install and will never write to
	// it. The UI uses it to hide Build, which would be refused anyway.
	Adopted bool   `json:"adopted"`
	Present bool   `json:"present"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	// VersionSource is "binary", "stamp", or empty. Empty WITH present=true is
	// the honest "installed, and there is no way to say what it is" — ninfer has
	// no --version at all, so a copy corrallm did not build cannot be named.
	VersionSource string `json:"versionSource,omitempty"`
	Commit        string `json:"commit,omitempty"`
	// Behind is only true when both sides are known. An unknown is not a "no",
	// and rendering it as one would put a permanent out-of-date badge on every
	// tool that cannot identify itself.
	Behind     bool   `json:"behind"`
	RemoteHead string `json:"remoteHead,omitempty"`
	DriftError string `json:"driftError,omitempty"`
	// Error is a failure to ASK — host unreachable, agent too old, recipe
	// crashed. Distinct from a probe that answered "not present".
	Error string `json:"error,omitempty"`
}

// ToolStatesOutput is the whole registry view.
type ToolStatesOutput struct {
	Body struct {
		Tools []ToolStateView `json:"tools"`
	}
}

// ToolStates reports every declared (tool, host) pair.
func (h *Handlers) ToolStates(ctx context.Context, in *ToolStatesInput) (*ToolStatesOutput, error) {
	out := &ToolStatesOutput{}
	out.Body.Tools = []ToolStateView{}
	if h.Tools == nil {
		return out, nil
	}
	for _, s := range h.Tools.SurveyAll(ctx) {
		v := ToolStateView{
			Tool: s.Tool, Host: s.Host,
			Declared: s.Declared, Adopted: s.Adopted, Error: s.Error,
		}
		if s.Probe != nil {
			v.Present = s.Probe.Present
			v.Path = s.Probe.Path
			v.Version = s.Probe.Version
			v.VersionSource = s.Probe.Source
			v.Commit = s.Probe.Commit
		}
		if s.Drift != nil {
			v.Behind = s.Drift.Behind
			v.RemoteHead = s.Drift.RemoteHead
			v.DriftError = s.Drift.Error
		}
		out.Body.Tools = append(out.Body.Tools, v)
	}
	return out, nil
}

// ToolPreflightInput names one tool on one host.
type ToolPreflightInput struct {
	Body struct {
		Tool string `json:"tool"`
		Host string `json:"host"`
	}
}

// ToolPreflightOutput is "could this host build it, and what is missing".
type ToolPreflightOutput struct {
	Body struct {
		OK bool `json:"ok"`
		// Runnable is separate from OK on purpose: nvcc cross-compiles for an
		// absent architecture happily, so a host can be able to build something
		// it cannot run.
		Runnable bool     `json:"runnable"`
		Missing  []string `json:"missing"`
		Commands []string `json:"commands" doc:"Exactly what would be run to fix it. Shown even when installing is not permitted here, so it can be run by hand."`
		Notes    []string `json:"notes"`
	}
}

// ToolPreflight answers whether a host could build a tool. Seconds; compiles
// nothing.
func (h *Handlers) ToolPreflight(ctx context.Context, in *ToolPreflightInput) (*ToolPreflightOutput, error) {
	if h.Tools == nil {
		return nil, fmt.Errorf("no toolchain registry configured")
	}
	pf, err := h.Tools.Preflight(ctx, in.Body.Tool, in.Body.Host)
	if err != nil {
		return nil, err
	}
	out := &ToolPreflightOutput{}
	out.Body.OK = pf.OK
	out.Body.Runnable = pf.Runnable
	out.Body.Missing = orEmpty(pf.Missing)
	out.Body.Commands = orEmpty(pf.Commands)
	out.Body.Notes = orEmpty(pf.Notes)
	return out, nil
}

// ToolResolveInput names a reference to expand.
type ToolResolveInput struct {
	Body struct {
		Tool string `json:"tool"`
		Host string `json:"host"`
	}
}

// ToolResolveOutput is what ${tool:x} becomes on that host.
type ToolResolveOutput struct {
	Body struct {
		Dir string `json:"dir"`
	}
}

// ToolResolve expands a reference without spawning anything, through the same
// code path a spawn uses — so agreement here is agreement there.
func (h *Handlers) ToolResolve(ctx context.Context, in *ToolResolveInput) (*ToolResolveOutput, error) {
	if h.Tools == nil {
		return nil, fmt.Errorf("no toolchain registry configured")
	}
	dir, err := h.Tools.ToolDir(ctx, in.Body.Tool, in.Body.Host)
	if err != nil {
		return nil, err
	}
	out := &ToolResolveOutput{}
	out.Body.Dir = dir
	return out, nil
}

// orEmpty renders a nil slice as [] rather than null.
//
// The UI maps over these, and a null would need a guard at every call site that
// a caller will eventually forget.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// compile-time assurance the registry type is what the handlers expect.
var _ = (*toolchain.Registry)(nil)
