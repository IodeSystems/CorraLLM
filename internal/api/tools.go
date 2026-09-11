package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
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
	// StartedBy names the models and extensions whose cmd references this tool.
	//
	// It is what makes `behind` actionable. Drift alone cannot tell an operator
	// whether to act: a tool nothing runs looks exactly like one holding six
	// models on an old build, and the control beside it offers "minutes of
	// full-machine compile" either way — so the safe reading is to leave it,
	// which was right by luck rather than by knowing (Lenny run 9, on ninfer,
	// which nothing on this box references).
	//
	// Computed per request from the config in memory: 13.8µs on a config larger
	// than this box runs (BenchmarkUsersOf), against a survey that makes network
	// probes to every agent. Not worth a cache it could go stale against.
	StartedBy []string `json:"startedBy" doc:"Models and extensions that start from this tool. Empty means nothing does."`
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
	// Ref is what the tool TRACKS — a branch or tag — and Pin, when set, is the
	// commit it is HELD at instead.
	//
	// Both are on the row because they answer different questions and the UI
	// shows them together: "master, held at 0b1bad14f" is the whole state of a
	// tool somebody deliberately stopped moving, and either half alone reads as
	// something else.
	Ref string `json:"ref,omitempty"`
	Pin string `json:"pin,omitempty"`
	// Ahead means the tracked ref has moved past the pin. Only ever true while
	// pinned — unpinned, Behind already says it.
	Ahead bool `json:"ahead,omitempty"`
	// PinUnsupported means this host's agent predates pins: its drift answer
	// was computed against the tracked ref and says nothing about the pin.
	// Behind is cleared when it is set, so the row must say why rather than
	// leaving a pinned tool looking quietly fine.
	PinUnsupported bool `json:"pinUnsupported,omitempty"`
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
	users := toolchain.UsersOf(h.config())
	for _, s := range h.Tools.SurveyAll(ctx) {
		v := ToolStateView{
			Tool: s.Tool, Host: s.Host,
			Declared: s.Declared, Adopted: s.Adopted, Error: s.Error,
			StartedBy: users[s.Tool],
		}
		if v.StartedBy == nil {
			v.StartedBy = []string{}
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
			v.Ahead = s.Drift.Ahead
			v.PinUnsupported = s.Drift.PinUnsupported
		}
		// From config, not the survey: a pin is a fact about configuration and
		// must show even on a host that could not be asked anything. A row
		// reading "cannot ask" with no pin beside it would send somebody to
		// re-pin a tool that is already pinned.
		if cfg := h.config(); cfg != nil {
			if t, ok := cfg.Tools[s.Tool]; ok {
				v.Ref, v.Pin = t.Ref, config.NormalizePin(t.Pin)
			}
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

// Pinning, and rolling back to a build already on the host.
//
// These are the two halves of "hold this tool still", and they are deliberately
// different operations because they act on different things:
//
//   - A PIN is configuration. It says which commit the tool should be at, on
//     every host, and it survives a rebuild — including a scheduled one, which
//     is the failure it exists to prevent: `rebuild: true` plus a regression
//     upstream means the bad build comes back every six hours.
//   - ACTIVATE is local. It repoints one host's bin/ at a build already sitting
//     in builds/, which takes a second and needs no compiler. It is what you
//     reach for when a build you already have is known-good.
//
// The pair is what makes holding a tool back cheap: activate to get serving
// again now, pin so nothing walks it forward later.

// ToolPinInput sets or clears a tool's pin.
type ToolPinInput struct {
	Body struct {
		Tool string `json:"tool"`
		// Pin is a full 40-character commit sha, or empty to un-pin.
		//
		// Full only, and validated here rather than at build time: an
		// abbreviated hash cannot be requested from a remote, so it would
		// resolve on a host that already has a clone and fail on one that does
		// not — a pin that works until the machine you need it on.
		Pin string `json:"pin"`
	}
}

// ToolPinOutput reports what the tool is now pinned to.
type ToolPinOutput struct {
	Body struct {
		Tool string `json:"tool"`
		Ref  string `json:"ref" doc:"What the tool tracks. Unchanged by pinning — a pin holds, it does not retarget."`
		Pin  string `json:"pin" doc:"The commit it is held at. Empty means it tracks ref freely again."`
		// Message says what happens next, because the effect is NOT immediate:
		// a pin changes what the next build produces, and processes already
		// running keep the binary they started with.
		Message string `json:"message"`
	}
}

// ToolPin holds a tool at one commit, or lets it go again.
//
// It does not build and it does not verify the commit exists upstream. Neither
// is free — one is twenty minutes of nvcc, the other a clone or a remote that
// may not serve an arbitrary sha — and both would turn "hold this still" into
// an operation that can fail for reasons unrelated to the decision being
// recorded. A pin that names a commit no branch reaches fails at the next
// build, loudly, naming the sha.
func (h *Handlers) ToolPin(ctx context.Context, in *ToolPinInput) (*ToolPinOutput, error) {
	name := strings.TrimSpace(in.Body.Tool)
	if name == "" {
		return nil, huma.Error400BadRequest("a pin needs a tool")
	}
	pin := config.NormalizePin(in.Body.Pin)
	if err := config.ValidatePin(pin); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	var ref string
	err := h.mutateConfig(ctx, "tool "+in.Body.Tool+" pinned", func(c *config.Config) error {
		t, ok := c.Tools[name]
		if !ok {
			return huma.Error404NotFound(fmt.Sprintf("no tool %q declared", name))
		}
		t.Pin = pin
		c.Tools[name] = t
		ref = t.Ref
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &ToolPinOutput{}
	out.Body.Tool, out.Body.Ref, out.Body.Pin = name, ref, pin
	if pin == "" {
		out.Body.Message = fmt.Sprintf("%s tracks %s again — the next build takes whatever is at its head", name, ref)
	} else {
		out.Body.Message = fmt.Sprintf("%s is held at %s; the next build on each host checks that out, and nothing that is already running changes until then", name, pin)
	}
	return out, nil
}

// ToolInstalledBuildsInput names one tool on one host.
type ToolInstalledBuildsInput struct {
	Tool string `query:"tool" doc:"Tool name."`
	Host string `query:"host" doc:"Server the builds live on."`
}

// InstalledBuildView is one build sitting in the host's builds/ directory.
type InstalledBuildView struct {
	ID string `json:"id" doc:"The build directory: a UTC timestamp and the short commit."`
	// Head is the commit it was built from, which is what a pin would name.
	Head   string `json:"head,omitempty"`
	Stamp  string `json:"stamp,omitempty"`
	At     int64  `json:"at" doc:"Unix seconds when it was installed."`
	Active bool   `json:"active" doc:"bin/ currently points here."`
}

// ToolInstalledBuildsOutput is what a host can roll back to.
type ToolInstalledBuildsOutput struct {
	Body struct {
		Builds []InstalledBuildView `json:"builds"`
		Active string               `json:"active"`
		// Versioned is false on a prefix that predates versioned installs.
		// Reported rather than inferred from an empty list: "nothing to roll
		// back to yet" and "this layout does not track builds" are different
		// answers and only one of them is fixed by building again.
		Versioned bool `json:"versioned"`
		Keep      int  `json:"keep" doc:"How many the host retains. The active build is never pruned."`
	}
}

// ToolInstalledBuilds lists the builds a host could activate.
//
// As cheap as a probe — it reads a directory — and it deliberately does not
// mutate the tree it describes, so a listing is safe to open on a host mid-build.
func (h *Handlers) ToolInstalledBuilds(ctx context.Context, in *ToolInstalledBuildsInput) (*ToolInstalledBuildsOutput, error) {
	if h.Tools == nil {
		return nil, fmt.Errorf("no toolchain registry configured")
	}
	bl, err := h.Tools.Builds(ctx, in.Tool, in.Host)
	if err != nil {
		return nil, err
	}
	if bl.Error != "" {
		return nil, fmt.Errorf("%s", bl.Error)
	}
	out := &ToolInstalledBuildsOutput{}
	out.Body.Builds = []InstalledBuildView{}
	for _, b := range bl.Builds {
		out.Body.Builds = append(out.Body.Builds, InstalledBuildView{
			ID: b.ID, Head: b.Head, Stamp: b.Stamp, At: b.At, Active: b.Active,
		})
	}
	out.Body.Active, out.Body.Versioned, out.Body.Keep = bl.Active, bl.Versioned, bl.Keep
	return out, nil
}

// ToolActivateInput names the build to make current.
type ToolActivateInput struct {
	Body struct {
		Tool string `json:"tool"`
		Host string `json:"host"`
		ID   string `json:"id" doc:"Build id from toolInstalledBuilds."`
	}
}

// ToolActivateOutput reports the swap.
type ToolActivateOutput struct {
	Body struct {
		Active string `json:"active"`
		// Previous is what was current before, so a rollback taken by mistake
		// can be undone without listing again.
		Previous string `json:"previous"`
		Message  string `json:"message"`
	}
}

// ToolActivate repoints a host's bin/ at a build it already has.
//
// Seconds, not minutes: this is a symlink rename, which is the entire reason
// versioned builds exist. Processes already running keep the binary they
// started with — they hold the inode — so it takes effect on the next spawn,
// exactly as a build does.
func (h *Handlers) ToolActivate(ctx context.Context, in *ToolActivateInput) (*ToolActivateOutput, error) {
	if h.Tools == nil {
		return nil, fmt.Errorf("no toolchain registry configured")
	}
	res, err := h.Tools.Activate(ctx, in.Body.Tool, in.Body.Host, in.Body.ID)
	if err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	out := &ToolActivateOutput{}
	out.Body.Active, out.Body.Previous = res.Active, res.Previous
	out.Body.Message = fmt.Sprintf(
		"%s on %s now serves %s (was %s). Models already loaded keep the binary they started with; reload one to pick this up.",
		in.Body.Tool, in.Body.Host, res.Active, orDash(res.Previous))
	return out, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "nothing"
	}
	return s
}
