package api

import (
	"context"
	"sort"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// LaneRefView is one lane a model is approved into, and its position there.
type LaneRefView struct {
	Lane  string `json:"lane" doc:"Lane name."`
	Order int    `json:"order" doc:"Position in that lane's ladder; lower is tried first."`
}

// ApprovalView is one decision, as the dashboard renders it.
type ApprovalView struct {
	Provider   string        `json:"provider" doc:"Provider whose catalogue offered it."`
	Credential string        `json:"credential" doc:"Account that saw it — catalogues differ by key."`
	Model      string        `json:"model" doc:"Served model name."`
	State      string        `json:"state" doc:"pending | approved | rejected."`
	Lanes      []LaneRefView `json:"lanes" doc:"Lanes it was approved into, with position."`
	Quality    float64       `json:"quality" doc:"Operator's rank, replacing the discovery template's uniform guess (0 = keep the template's)."`
	Note       string        `json:"note" doc:"Free text kept with the decision."`
	AtMS       int64         `json:"atMs" doc:"When the decision was recorded."`
	Upstream   string        `json:"upstream" doc:"Provider id for a model picked off a catalogue by hand. Empty for one a discovery filter found."`
	Picked     bool          `json:"picked" doc:"Chosen by hand rather than admitted by a filter — so this decision is what makes the model exist at all."`
}

type ApprovalsOutput struct {
	Body struct {
		Approvals []ApprovalView `json:"approvals" doc:"Every recorded decision, plus every discovered model still awaiting one."`
	}
}

// ListApprovals returns recorded decisions AND the discovered models that have
// none yet, so the dashboard can render one queue rather than diffing two lists
// client-side — the pending set is the whole point of the view.
func (h *Handlers) ListApprovals(_ context.Context, _ *struct{}) (*ApprovalsOutput, error) {
	out := &ApprovalsOutput{}
	out.Body.Approvals = []ApprovalView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.LoadApprovals()
	if err != nil {
		return nil, err
	}
	decided := map[string]bool{}
	for _, r := range rows {
		lanes := make([]LaneRefView, 0, len(r.Lanes))
		for _, l := range r.Lanes {
			lanes = append(lanes, LaneRefView{Lane: l.Lane, Order: l.Order})
		}
		decided[config.ApprovalKey(r.Provider, r.Credential, r.Model)] = true
		out.Body.Approvals = append(out.Body.Approvals, ApprovalView{
			Provider: r.Provider, Credential: r.Credential, Model: r.Model,
			State: r.State, Lanes: lanes, Quality: r.Quality, Note: r.Note,
			AtMS: r.At.UnixMilli(), Upstream: r.Upstream, Picked: r.Upstream != "",
		})
	}
	// Synthesise a pending row per (credential that can serve it, discovered
	// model) with no decision. Only credentials REQUIRING approval are listed:
	// elsewhere the model already serves, so presenting it as awaiting a
	// decision would imply a gate that is not there.
	cfg := h.Cfg
	if cfg != nil {
		for name, m := range cfg.Discovered() {
			ext, ok := cfg.Extensions[m.Extension]
			if !ok {
				continue
			}
			pv, ok := ext.Providers[m.ProviderName]
			if !ok {
				continue
			}
			for _, cr := range pv.CredentialList() {
				if !cr.ApprovalRequired || !cfg.DiscoveredServableBy(name, cr.Name) {
					continue
				}
				if decided[config.ApprovalKey(m.ProviderName, cr.Name, name)] {
					continue
				}
				out.Body.Approvals = append(out.Body.Approvals, ApprovalView{
					Provider: m.ProviderName, Credential: cr.Name, Model: name,
					State: store.ApprovalPending, Lanes: []LaneRefView{},
				})
			}
		}
	}
	sort.Slice(out.Body.Approvals, func(i, j int) bool {
		a, b := out.Body.Approvals[i], out.Body.Approvals[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Credential != b.Credential {
			return a.Credential < b.Credential
		}
		return a.Model < b.Model
	})
	return out, nil
}

type DecideApprovalInput struct {
	Body struct {
		Provider   string        `json:"provider" doc:"Provider whose catalogue offered it."`
		Credential string        `json:"credential" doc:"Account the decision applies to."`
		Model      string        `json:"model" doc:"Served model name."`
		State      string        `json:"state" doc:"approved | rejected | pending (pending clears the decision)."`
		Lanes      []LaneRefView `json:"lanes,omitempty" doc:"Lanes to place it in, with position."`
		Quality    float64       `json:"quality,omitempty" doc:"Rank to use instead of the discovery template's guess."`
		Note       string        `json:"note,omitempty"`
		// Upstream turns this from a verdict into an ENROLMENT. Sent when the
		// model was picked off a catalogue rather than found by a filter: it is
		// the provider's own id, and without it nothing downstream could
		// address a model discovery never saw.
		Upstream string `json:"upstream,omitempty" doc:"Provider model id, for a model chosen off a catalogue instead of found by the discover filter."`
	}
}

// DecideApproval records a decision and applies it to routing immediately.
func (h *Handlers) DecideApproval(_ context.Context, in *DecideApprovalInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	if h.Store == nil {
		out.Body.Message = "no store: approvals cannot be recorded"
		return out, nil
	}
	b := in.Body
	switch b.State {
	case store.ApprovalApproved, store.ApprovalRejected:
		lanes := make([]store.LaneRef, 0, len(b.Lanes))
		for _, l := range b.Lanes {
			lanes = append(lanes, store.LaneRef{Lane: l.Lane, Order: l.Order})
		}
		if err := h.Store.SaveApproval(store.ModelApproval{
			Provider: b.Provider, Credential: b.Credential, Model: b.Model,
			State: b.State, Lanes: lanes, Quality: b.Quality, Note: b.Note,
			Upstream: b.Upstream,
		}); err != nil {
			return nil, err
		}
	case store.ApprovalPending, "":
		// Clearing returns the model to the queue, which is how a mis-click is
		// undone. Rejections are otherwise permanent by design.
		if err := h.Store.DeleteApproval(b.Provider, b.Credential, b.Model); err != nil {
			return nil, err
		}
	default:
		out.Body.Message = "state must be approved, rejected or pending"
		return out, nil
	}
	if err := h.reloadApprovals(); err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "recorded " + b.State + " for " + b.Model + " on " + b.Credential
	return out, nil
}

// reloadApprovals re-reads every decision and installs it, so a change takes
// effect on the next request rather than the next restart.
func (h *Handlers) reloadApprovals() error {
	rows, err := h.Store.LoadApprovals()
	if err != nil {
		return err
	}
	InstallApprovals(h.config(), rows)
	return nil
}

// InstallApprovals applies a decision set to a live config: the gate that says
// which discovered models may serve, and the manual enrolments that say which
// models exist in the first place.
//
// Exported and shared with startup because the two used to be separate copies
// of the same loop, and a copy that installs the gate but forgets the picks
// would drop every hand-chosen model on restart — silently, and only on
// restart, which is the worst way to find out.
func InstallApprovals(cfg *config.Config, rows []store.ModelApproval) {
	if cfg == nil {
		return
	}
	view := make(map[string]config.ApprovalView, len(rows))
	var picks []config.Pick
	for _, r := range rows {
		lanes := make([]config.LanePlacement, 0, len(r.Lanes))
		for _, l := range r.Lanes {
			lanes = append(lanes, config.LanePlacement{Lane: l.Lane, Order: l.Order})
		}
		view[config.ApprovalKey(r.Provider, r.Credential, r.Model)] = config.ApprovalView{
			State: r.State, Lanes: lanes, Quality: r.Quality,
		}
		// Only an APPROVED pick enrols. A pending or rejected one keeps its row
		// — that is what stops the queue asking again — but contributes no
		// model, so "reject" genuinely removes it rather than merely un-gating.
		if r.Upstream != "" && r.State == store.ApprovalApproved {
			picks = append(picks, config.Pick{
				Provider: r.Provider, Credential: r.Credential,
				Model: r.Model, Upstream: r.Upstream, Quality: r.Quality,
			})
		}
	}
	cfg.SetApprovals(view)
	cfg.SetPicks(picks)
}
