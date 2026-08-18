package api

import (
	"context"
	"fmt"
	"time"

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

// --- builds (P25b) ---
//
// A build is minutes long, so it cannot be a request/response: the browser
// would hold a connection for a quarter of an hour and lose everything on a
// reload. Start returns immediately with a job id; the modal polls status and
// pulls the log incrementally.

// ToolBuildStartInput names what to build.
type ToolBuildStartInput struct {
	Body struct {
		Tool  string `json:"tool"`
		Host  string `json:"host"`
		Force bool   `json:"force" required:"false" doc:"Build even when the stamp already matches (same HEAD, same patches, same CUDA archs)."`
	}
}

// ToolJobView is one build, running or finished.
type ToolJobView struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Host   string `json:"host"`
	Status string `json:"status" doc:"running | ok | failed"`
	// StartedAt/FinishedAt are RFC3339. ElapsedSeconds is computed server-side
	// so a running job's timer does not depend on the client's clock agreeing
	// with the daemon's.
	StartedAt      string `json:"startedAt"`
	FinishedAt     string `json:"finishedAt,omitempty"`
	ElapsedSeconds int    `json:"elapsedSeconds"`
	// Skipped means the stamp matched and nothing compiled — which is why a
	// "build" can finish in two seconds.
	Skipped bool   `json:"skipped"`
	Version string `json:"version,omitempty"`
	Stamp   string `json:"stamp,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolBuildStartOutput hands back the job that was started.
type ToolBuildStartOutput struct {
	Body struct {
		Job ToolJobView `json:"job"`
	}
}

// ToolBuildStart begins a build. Refused when one is already running: a build
// takes every core on the box, so two at once finish later than two in sequence.
func (h *Handlers) ToolBuildStart(_ context.Context, in *ToolBuildStartInput) (*ToolBuildStartOutput, error) {
	if h.Builds == nil {
		return nil, fmt.Errorf("no toolchain builder configured")
	}
	j, err := h.Builds.Start(in.Body.Tool, in.Body.Host, in.Body.Force)
	if err != nil {
		return nil, err
	}
	out := &ToolBuildStartOutput{}
	out.Body.Job = jobView(j)
	return out, nil
}

// ToolBuildStatusInput asks for the current or last build, and the log after a
// point the caller has already seen.
type ToolBuildStatusInput struct {
	LogFrom int `query:"logFrom" doc:"Absolute line index to read the log from. Send the previous response's logTotal to get only what is new."`
}

// ToolBuildStatusOutput is the whole modal's state in one call.
type ToolBuildStatusOutput struct {
	Body struct {
		// Current is the running build, absent when nothing is running.
		Current *ToolJobView `json:"current,omitempty"`
		// Last is the most recent finished build, kept because "did that work?"
		// is asked minutes later by somebody who closed the modal.
		Last     *ToolJobView `json:"last,omitempty"`
		Log      []string     `json:"log"`
		LogTotal int          `json:"logTotal" doc:"Total lines ever emitted. Pass back as logFrom; a gap means the ring trimmed what you missed."`
	}
}

// ToolBuildStatus reports the running build if there is one, else the last.
func (h *Handlers) ToolBuildStatus(_ context.Context, in *ToolBuildStatusInput) (*ToolBuildStatusOutput, error) {
	out := &ToolBuildStatusOutput{}
	out.Body.Log = []string{}
	if h.Builds == nil {
		return out, nil
	}
	cur, last := h.Builds.State()
	if cur != nil {
		v := jobView(cur)
		out.Body.Current = &v
	}
	if last != nil {
		v := jobView(last)
		out.Body.Last = &v
	}
	// The log follows whichever job the modal is showing: the running one when
	// there is one, otherwise the last. Same rule the UI uses to pick a title,
	// so the two never disagree about which build's output is on screen.
	show := cur
	if show == nil {
		show = last
	}
	if show != nil {
		lines, total := show.LogFrom(in.LogFrom)
		out.Body.Log = lines
		out.Body.LogTotal = total
	}
	return out, nil
}

func jobView(j *toolchain.Job) ToolJobView {
	s := j.Snapshot()
	v := ToolJobView{
		ID: s.ID, Tool: s.Tool, Host: s.Host, Status: s.Status,
		StartedAt:      s.StartedAt.Format(time.RFC3339),
		ElapsedSeconds: int(j.Elapsed().Seconds()),
		Skipped:        s.Skipped, Version: s.Version, Stamp: s.Stamp, Error: s.Error,
	}
	if !s.FinishedAt.IsZero() {
		v.FinishedAt = s.FinishedAt.Format(time.RFC3339)
	}
	return v
}

// ToolBuildHistoryInput bounds the listing.
type ToolBuildHistoryInput struct {
	Tool  string `query:"tool" doc:"Scope to one tool. Empty means any."`
	Host  string `query:"host" doc:"Scope to one host. Empty means any."`
	Limit int    `query:"limit" doc:"Newest first. Default 20."`
}

// ToolBuildRecord is one persisted build.
//
// No log field: a listing of twenty builds would otherwise carry twenty logs,
// which is megabytes to render a list of dates. Fetch one with toolBuildLog.
type ToolBuildRecord struct {
	ID   int64  `json:"id"`
	Tool string `json:"tool"`
	Host string `json:"host"`
	// Status is running | ok | failed | interrupted. "interrupted" means the
	// daemon restarted while it ran, which kills it — a build is a child of
	// this process.
	Status         string `json:"status"`
	StartedAt      string `json:"startedAt"`
	FinishedAt     string `json:"finishedAt,omitempty"`
	ElapsedSeconds int    `json:"elapsedSeconds"`
	Skipped        bool   `json:"skipped"`
	Version        string `json:"version,omitempty"`
	Stamp          string `json:"stamp,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ToolBuildHistoryOutput is the persisted list.
type ToolBuildHistoryOutput struct {
	Body struct {
		Builds []ToolBuildRecord `json:"builds"`
	}
}

// ToolBuildHistory lists builds that outlived the process that ran them.
//
// The Builder's in-memory current/last is right for a live modal and empty
// after any restart — and this daemon restarts on every deploy, so "did that
// build work?" had no answer an hour later. This does.
func (h *Handlers) ToolBuildHistory(ctx context.Context, in *ToolBuildHistoryInput) (*ToolBuildHistoryOutput, error) {
	out := &ToolBuildHistoryOutput{}
	out.Body.Builds = []ToolBuildRecord{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.RecentToolBuilds(ctx, in.Tool, in.Host, in.Limit)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		rec := ToolBuildRecord{
			ID: r.ID, Tool: r.Tool, Host: r.Host, Status: r.Status,
			StartedAt: r.StartedAt.Format(time.RFC3339),
			Skipped:   r.Skipped, Version: r.Version, Stamp: r.Stamp, Error: r.Error,
		}
		if !r.FinishedAt.IsZero() {
			rec.FinishedAt = r.FinishedAt.Format(time.RFC3339)
			rec.ElapsedSeconds = int(r.FinishedAt.Sub(r.StartedAt).Seconds())
		}
		out.Body.Builds = append(out.Body.Builds, rec)
	}
	return out, nil
}

// ToolBuildLogInput names one build.
type ToolBuildLogInput struct {
	ID int64 `path:"id"`
}

// ToolBuildLogOutput is that build's captured output.
type ToolBuildLogOutput struct {
	Body struct {
		Log string `json:"log"`
	}
}

// ToolBuildLog returns one past build's log — the reason anybody opens an old
// build at all.
func (h *Handlers) ToolBuildLog(ctx context.Context, in *ToolBuildLogInput) (*ToolBuildLogOutput, error) {
	out := &ToolBuildLogOutput{}
	if h.Store == nil {
		return out, nil
	}
	log, err := h.Store.ToolBuildLog(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	out.Body.Log = log
	return out, nil
}
