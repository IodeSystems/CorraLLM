package api

import (
	"context"
	"sort"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// Model selections: what an operator chose off a provider's directory, and
// where they put it.
//
// This replaced an approvals API. The difference is not naming: approvals had
// three states and a QUEUE — every discovered model on a gated credential
// showed up as a question owed — and against a provider listing hundreds of
// models a queue is a chore, not a control. Selections have no states. A model
// is assigned or it is not, and unassigning is a DELETE.

// LaneRefView is one lane a model is assigned to, and its position there.
type LaneRefView struct {
	Lane  string `json:"lane" doc:"Lane name."`
	Order int    `json:"order" doc:"Priority in that lane's ladder; lower is tried first."`
}

// SelectionView is one assignment, as the dashboard renders it.
type SelectionView struct {
	Provider   string        `json:"provider" doc:"Provider whose directory offered it."`
	Credential string        `json:"credential" doc:"Account it is served on — directories differ by key."`
	Model      string        `json:"model" doc:"Served model name."`
	Upstream   string        `json:"upstream" doc:"The provider's own id. Empty when this row only places a model a discover filter already contributes."`
	Lanes      []LaneRefView `json:"lanes" doc:"Lanes it was placed in, with priority."`
	Quality    float64       `json:"quality" doc:"Operator's rank, replacing the discovery template's uniform guess (0 = keep the template's)."`
	Note       string        `json:"note" doc:"Free text kept with the selection."`
	AtMS       int64         `json:"atMs" doc:"When it was assigned."`
	// Serving reports whether the model is actually in the served registry
	// right now. A selection whose provider or upstream id has gone away stays
	// on record but stops serving, and a list that could not say so would be
	// quietly lying about what is reachable.
	Serving bool `json:"serving" doc:"Currently in the served registry."`
}

type SelectionsOutput struct {
	Body struct {
		Selections []SelectionView `json:"selections" doc:"Every model assigned off a provider's directory."`
	}
}

// ListSelections returns every assignment.
func (h *Handlers) ListSelections(_ context.Context, _ *struct{}) (*SelectionsOutput, error) {
	out := &SelectionsOutput{}
	out.Body.Selections = []SelectionView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.LoadSelections()
	if err != nil {
		return nil, err
	}
	cfg := h.config()
	for _, r := range rows {
		lanes := make([]LaneRefView, 0, len(r.Lanes))
		for _, l := range r.Lanes {
			lanes = append(lanes, LaneRefView{Lane: l.Lane, Order: l.Order})
		}
		v := SelectionView{
			Provider: r.Provider, Credential: r.Credential, Model: r.Model,
			Upstream: r.Upstream, Lanes: lanes, Quality: r.Quality, Note: r.Note,
			AtMS: r.At.UnixMilli(),
		}
		if cfg != nil {
			_, declared := cfg.Models[r.Model]
			_, discovered := cfg.Discovered()[r.Model]
			v.Serving = declared || discovered
		}
		out.Body.Selections = append(out.Body.Selections, v)
	}
	sort.Slice(out.Body.Selections, func(i, j int) bool {
		a, b := out.Body.Selections[i], out.Body.Selections[j]
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

type AssignModelInput struct {
	Body struct {
		Provider   string        `json:"provider" doc:"Provider whose directory offered it."`
		Credential string        `json:"credential" doc:"Account to serve it on."`
		Model      string        `json:"model" doc:"Served model name."`
		Upstream   string        `json:"upstream,omitempty" doc:"The provider's own model id. Required when assigning a model no discover filter contributes — it is the only record of what to put on the wire."`
		Lanes      []LaneRefView `json:"lanes,omitempty" doc:"Lanes to place it in, with priority."`
		Quality    float64       `json:"quality,omitempty" doc:"Rank to use instead of the discovery template's guess."`
		Note       string        `json:"note,omitempty"`
	}
}

// AssignModel selects a model and applies it to routing immediately.
//
// Idempotent: assigning an already-assigned model updates its placement, which
// is what "move it to another lane" is. There is no separate edit operation,
// because a selection is small enough to be written whole.
func (h *Handlers) AssignModel(_ context.Context, in *AssignModelInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	if h.Store == nil {
		out.Body.Message = "no store: selections cannot be recorded"
		return out, nil
	}
	b := in.Body
	if strings.TrimSpace(b.Provider) == "" || strings.TrimSpace(b.Model) == "" {
		out.Body.Message = "provider and model are required"
		return out, nil
	}
	if strings.TrimSpace(b.Credential) == "" {
		b.Credential = config.DefaultCredentialName
	}
	lanes := make([]store.LaneRef, 0, len(b.Lanes))
	for _, l := range b.Lanes {
		lanes = append(lanes, store.LaneRef{Lane: l.Lane, Order: l.Order})
	}
	if err := h.Store.SaveSelection(store.ModelSelection{
		Provider: b.Provider, Credential: b.Credential, Model: b.Model,
		Upstream: b.Upstream, Lanes: lanes, Quality: b.Quality, Note: b.Note,
	}); err != nil {
		return nil, err
	}
	if err := h.reloadSelections(); err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "assigned " + b.Model + " on " + b.Credential
	return out, nil
}

type UnassignModelInput struct {
	Body struct {
		Provider   string `json:"provider"`
		Credential string `json:"credential"`
		Model      string `json:"model"`
	}
}

// UnassignModel drops a selection.
//
// For a model chosen off a directory this removes it from service entirely —
// the selection was the only reason it existed. For a placement-only row it
// removes the lane placement and leaves the discover filter's contribution
// alone. Both are what "I no longer want this" means in context, which is why
// there is one verb rather than a reject and an unplace.
func (h *Handlers) UnassignModel(_ context.Context, in *UnassignModelInput) (*ConfigMutationOutput, error) {
	out := &ConfigMutationOutput{}
	if h.Store == nil {
		out.Body.Message = "no store: selections cannot be recorded"
		return out, nil
	}
	b := in.Body
	cred := b.Credential
	if strings.TrimSpace(cred) == "" {
		cred = config.DefaultCredentialName
	}
	if err := h.Store.DeleteSelection(b.Provider, cred, b.Model); err != nil {
		return nil, err
	}
	if err := h.reloadSelections(); err != nil {
		return nil, err
	}
	out.Body.OK = true
	out.Body.Message = "unassigned " + b.Model
	return out, nil
}

// reloadSelections re-reads every selection and installs it, so a change takes
// effect on the next request rather than the next restart.
func (h *Handlers) reloadSelections() error {
	rows, err := h.Store.LoadSelections()
	if err != nil {
		return err
	}
	InstallSelections(h.config(), rows)
	return nil
}

// InstallSelections applies a selection set to a live config.
//
// Exported and shared with startup and reload because those used to be separate
// copies of the same loop, and a copy that forgot one of them dropped every
// hand-chosen model — silently, and only on restart or on the next config
// write, which is the worst way to find out.
func InstallSelections(cfg *config.Config, rows []store.ModelSelection) {
	if cfg == nil {
		return
	}
	sel := make([]config.Selection, 0, len(rows))
	for _, r := range rows {
		lanes := make([]config.LanePlacement, 0, len(r.Lanes))
		for _, l := range r.Lanes {
			lanes = append(lanes, config.LanePlacement{Lane: l.Lane, Order: l.Order})
		}
		sel = append(sel, config.Selection{
			Provider: r.Provider, Credential: r.Credential, Model: r.Model,
			Upstream: r.Upstream, Lanes: lanes, Quality: r.Quality,
		})
	}
	cfg.SetSelections(sel)
}
