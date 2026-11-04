package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

const gatewayYAML = `extensions:
  corp:
    providers:
      gw:
        proxy: {host: gw.example, port: 443}
        manual: true
`

func gatewayHandlers(t *testing.T) *Handlers {
	t.Helper()
	cfg, err := config.Load(managedConfig(t, gatewayYAML))
	if err != nil {
		t.Fatal(err)
	}
	return storeBackedHandlers(t, cfg)
}

func listedGateway(t *testing.T, h *Handlers) ProviderSpec {
	t.Helper()
	out, err := h.ListProviders(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Body.Providers {
		if p.Name == "gw" {
			return p
		}
	}
	t.Fatal("gw not listed")
	return ProviderSpec{}
}

func saveGateway(t *testing.T, h *Handlers, edit func(*ProviderSpec)) string {
	t.Helper()
	spec := listedGateway(t, h)
	edit(&spec)
	out, err := h.UpsertProvider(context.Background(), &UpsertProviderInput{Body: spec})
	if err != nil {
		t.Fatal(err)
	}
	return out.Body.Message
}

func TestPassthroughAndProviderCapSaveAndRoundTrip(t *testing.T) {
	h := gatewayHandlers(t)
	four, seven, wait := 4, 7, "30s"
	saveGateway(t, h, func(p *ProviderSpec) {
		p.MaxConcurrent, p.MaxQueueDepth, p.MaxWait = &four, &seven, &wait
		p.Passthrough = &PassthroughSpec{Enabled: true, MaxConcurrent: 2, MaxQueueDepth: 3, InputPerM: 1, OutputPerM: 2}
	})

	stored := reloadStored(t, h, "after the save")
	pv := stored.Extensions["corp"].Providers["gw"]
	if pv.MaxConcurrent != 4 || pv.Scheduler == nil || pv.Scheduler.MaxQueueDepth != 7 || pv.Scheduler.MaxWait != "30s" {
		t.Errorf("provider cap not stored: %+v / %+v", pv.MaxConcurrent, pv.Scheduler)
	}
	pt := pv.Passthrough
	if pt == nil || pt.Template.MaxConcurrent != 2 || pt.Template.Scheduler == nil || pt.Template.Scheduler.MaxQueueDepth != 3 ||
		pt.Template.Price == nil || pt.Template.Price.OutputPerM != 2 {
		t.Fatalf("passthrough not stored: %+v", pt)
	}

	got := listedGateway(t, h)
	if got.Passthrough == nil || !got.Passthrough.Enabled || got.Passthrough.MaxQueueDepth != 3 || got.Passthrough.InputPerM != 1 {
		t.Errorf("listed passthrough = %+v", got.Passthrough)
	}
	if got.MaxConcurrent == nil || *got.MaxConcurrent != 4 || got.MaxWait == nil || *got.MaxWait != "30s" {
		t.Errorf("listed cap = %v / %v", got.MaxConcurrent, got.MaxWait)
	}

	// A save that does not send the fields keeps them.
	saveGateway(t, h, func(p *ProviderSpec) {
		p.MaxConcurrent, p.MaxQueueDepth, p.MaxWait, p.Passthrough = nil, nil, nil, nil
	})
	pv = reloadStored(t, h, "after a save without the fields").Extensions["corp"].Providers["gw"]
	if pv.MaxConcurrent != 4 || pv.Passthrough == nil {
		t.Errorf("an unrelated save dropped the cap or passthrough: %+v", pv)
	}

	// Switching it off, and clearing the cap.
	zero := 0
	saveGateway(t, h, func(p *ProviderSpec) {
		p.MaxConcurrent = &zero
		p.Passthrough = &PassthroughSpec{Enabled: false}
	})
	pv = reloadStored(t, h, "after switching off").Extensions["corp"].Providers["gw"]
	if pv.MaxConcurrent != 0 || pv.Passthrough != nil {
		t.Errorf("not cleared: cap %d passthrough %+v", pv.MaxConcurrent, pv.Passthrough)
	}
}

func TestABadWaitIsRefusedOnSave(t *testing.T) {
	h := gatewayHandlers(t)
	spec := listedGateway(t, h)
	bad := "soon"
	spec.MaxWait = &bad
	if _, err := h.UpsertProvider(context.Background(), &UpsertProviderInput{Body: spec}); err == nil {
		t.Error("a wait that is not a duration was accepted")
	}
	if pv := reloadStored(t, h, "after a refused save").Extensions["corp"].Providers["gw"]; pv.Scheduler != nil {
		t.Errorf("a bad wait was stored: %+v", pv.Scheduler)
	}
}
