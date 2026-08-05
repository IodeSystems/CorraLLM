package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"strings"
)

// TrialModelInput is a command the operator wants to SEE run before committing
// to it.
//
// Deliberately the same shape as ModelSpec's spawn-related half, so the
// dashboard can hand the form's current contents straight here and hand the
// result straight back — the point is to iterate on a cmd string without the
// round trip through config that authoring one has required until now.
type TrialModelInput struct {
	Body struct {
		Name     string            `json:"name" required:"false" doc:"Label for the run. Generated when empty; a trial is never written to config, so this names nothing permanent."`
		Cmd      string            `json:"cmd" doc:"The spawn command to try."`
		Server   string            `json:"server" doc:"Which machine to run it on."`
		Proxy    string            `json:"proxy" doc:"Where it will listen: a port (5800), host:port, or a URL."`
		RAMUsage map[string]string `json:"ramUsage" required:"false" doc:"Declared footprint. Required on a host that cannot measure per process; elsewhere the trial measures it for you."`
	}
}

// TrialModelOutput is the whole transcript plus what it learned.
type TrialModelOutput struct {
	Body struct {
		OK     bool              `json:"ok"`
		Events []proc.TrialEvent `json:"events" doc:"Every stage and log line, in the order they happened."`
		Result proc.TrialResult  `json:"result" doc:"What the run learned, for prefilling the model form."`
		Error  string            `json:"error,omitempty"`
	}
}

// TrialModel spawns an uncommitted command, reports what happened, and tears it
// down. It writes nothing to config, whatever the outcome.
//
// The transcript returns as one document rather than a stream, which is the
// honest shape for a first cut: the operator waits for a cold load either way,
// and the ordered stages-and-logs they need to diagnose a bad command are all
// present when it lands. Streaming is a UI improvement on top of this, not a
// different mechanism.
func (h *Handlers) TrialModel(ctx context.Context, in *TrialModelInput) (*TrialModelOutput, error) {
	if h.Mgr == nil {
		return nil, huma.Error503ServiceUnavailable("no process manager: trials unavailable")
	}
	// specToModel is the same translation the model form uses, so a trial and
	// the save that follows it interpret `proxy: 5800` identically. A trial that
	// resolved a port differently from the model it becomes would be worse than
	// no trial at all.
	mdl, err := specToModel(ModelSpec{
		Cmd: in.Body.Cmd, Server: in.Body.Server,
		Proxy: in.Body.Proxy, RAMUsage: in.Body.RAMUsage,
	})
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	id := in.Body.Name
	if id == "" {
		id = "trial-" + randomID()
	}

	// Collected under a lock: emit is documented as same-goroutine, but the
	// cost of being wrong is a torn slice, and this is not a hot path.
	var mu sync.Mutex
	events := []proc.TrialEvent{}
	res, runErr := h.Mgr.Trial(ctx, id, mdl, func(e proc.TrialEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	out := &TrialModelOutput{}
	out.Body.Events = events
	out.Body.Result = res
	out.Body.OK = runErr == nil
	if runErr != nil {
		// A 200 carrying a failed transcript, not a 4xx/5xx. The command failing
		// is the ANSWER the operator asked for — it is not an API error, and
		// returning it as one would throw away the logs that say why.
		out.Body.Error = runErr.Error()
	}
	return out, nil
}

func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}

// ProbeModelInput asks about a model that already exists.
type ProbeModelInput struct {
	Name  string `path:"name" doc:"Served model name."`
	Apply bool   `query:"apply" doc:"Write what was discovered back onto the model: modalities, slots, context. Off by default — a probe reads unless told to write."`
}

// ProbeModel interrogates a CONFIGURED model, as opposed to TrialModel, which
// runs a command that exists nowhere.
//
// Same report, different contract. A trial is an experiment on something not
// yet written down: free memory only, never evicts, always torn down. A probe
// asks about a declared model through the ordinary load path and leaves it
// exactly as it found it — warm if it was warm, and resident under its own
// rules if this call loaded it. Asking a model what it can do should not be
// able to unload it.
func (h *Handlers) ProbeModel(ctx context.Context, in *ProbeModelInput) (*TrialModelOutput, error) {
	if h.Mgr == nil {
		return nil, huma.Error503ServiceUnavailable("no process manager: probes unavailable")
	}
	var mu sync.Mutex
	events := []proc.TrialEvent{}
	res, runErr := h.Mgr.Probe(ctx, in.Name, func(e proc.TrialEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	out := &TrialModelOutput{}
	out.Body.Events = events
	out.Body.Result = res
	out.Body.OK = runErr == nil
	if runErr != nil {
		// A 200 with the transcript, for the same reason a trial does it: the
		// failure is the answer that was asked for, and an error status would
		// discard the stages and logs that say why.
		out.Body.Error = runErr.Error()
		return out, nil
	}
	if in.Apply {
		if err := h.applyProbe(in.Name, res); err != nil {
			out.Body.Error = "probed, but could not record it: " + err.Error()
		} else {
			out.Body.Events = append(out.Body.Events, proc.TrialEvent{
				Stage: "apply", OK: true,
				Msg: "recorded against this placement; upstream written to the model"})
		}
	}
	return out, nil
}

// applyProbe writes what the backend said about itself onto the model.
//
// The point is to stop capabilities being a thing an operator asserts. A model
// that did not DECLARE vision was treated as not having it — so the capability
// routing skipped it and llm-bench would not run a vision probe against it,
// which meant the one mechanism that could have proved the capability was gated
// on someone having already claimed it. The backend knows; asking it settles it.
//
// Deliberately does NOT write ramUsage. Footprint is measured continuously and
// the peak governs; writing a declaration would re-introduce the stale
// hand-typed number this all exists to remove.
func (h *Handlers) applyProbe(name string, res proc.TrialResult) error {
	return h.mutateConfig(func(c *config.Config) error {
		m, ok := c.Models[name]
		if !ok {
			return huma.Error404NotFound("no such model")
		}
		// Modalities, slots and context are NOT written here any more: the probe
		// records them against the placement it probed (proc.recordCapabilities),
		// which is the only place they are true. Writing them onto the model
		// would flatten two placements into one claim — the exact error this
		// whole change removes.
		//
		// Upstream stays, because it is a property of the NAME rather than of
		// the placement: it is what callers' requests get rewritten to, and it
		// is the same wherever the model runs.
		if res.Upstream != "" && !strings.HasPrefix(res.Upstream, "/") {
			m.Upstream = res.Upstream
			c.Models[name] = m
		}
		return nil
	})
}
