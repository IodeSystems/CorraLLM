package api

import (
	"context"

	"github.com/iodesystems/corrallm/internal/freeroster"
)

// Free-model roster (P16e) — each provider's currently-free models, refreshed
// periodically so a churned-out free model is visible and routed around.

// RosterOutput lists each provider's free roster.
type RosterOutput struct {
	Body struct {
		Providers []freeroster.ProviderView `json:"providers"`
	}
}

// FreeRoster reports the current free-model roster.
func (h *Handlers) FreeRoster(_ context.Context, _ *struct{}) (*RosterOutput, error) {
	out := &RosterOutput{}
	out.Body.Providers = []freeroster.ProviderView{}
	if h.Proxy == nil {
		return out, nil
	}
	if s := h.Proxy.RosterSnapshot(); s != nil {
		out.Body.Providers = s
	}
	return out, nil
}
