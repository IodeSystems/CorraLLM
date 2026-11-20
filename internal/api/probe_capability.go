package api

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// ProbeCapabilityInput names the model to probe.
type ProbeCapabilityInput struct {
	Name string `path:"name" doc:"Served model name. Must be a remote model."`
}

// CapView is one fact in the capability record, as published in /v1/models.
type CapView struct {
	Value  string    `json:"value"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// ProbeCapabilityOutput is the record after the probe ran.
type ProbeCapabilityOutput struct {
	Body struct {
		Model        string           `json:"model"`
		Capabilities map[string]CapView `json:"capabilities"`
	}
}

// ProbeCapability runs the manual reasoning-effect probe for a remote model.
// It walks the effort ladder (none→xhigh), measures the reasoning tokens per
// effort, and stores the probed facts. The record is immediately visible in
// /v1/models.
//
// Remote models only: a local model has no upstream to refuse or measure.
// Manual only: no schedule, no automatic trigger. The probe spends paid
// tokens, so it runs only when an operator asks.
func (h *Handlers) ProbeCapability(ctx context.Context, in *ProbeCapabilityInput) (*ProbeCapabilityOutput, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("no proxy: probes unavailable")
	}
	caps, err := h.Proxy.ProbeCapability(ctx, in.Name)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	out := &ProbeCapabilityOutput{}
	out.Body.Model = in.Name
	out.Body.Capabilities = make(map[string]CapView, len(caps))
	for k, v := range caps {
		out.Body.Capabilities[k] = CapView{Value: v.Value, Source: v.Source, At: v.At}
	}
	return out, nil
}
