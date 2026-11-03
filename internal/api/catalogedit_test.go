package api

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// A model chosen off a provider's catalog exists only as a selection. It must
// be listed, and a save must write it under its provider with the selection's
// upstream id, or its slots cannot be changed.
func TestACatalogModelIsListedAndEditable(t *testing.T) {
	var proxy yaml.Node
	if err := proxy.Encode(map[string]any{"host": "api.example.com", "port": 443, "scheme": "https"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Extensions: map[string]config.Extension{"fw": {Providers: map[string]config.Provider{"fw": {
		Manual: true, Proxy: proxy, Credentials: []config.Credential{{Name: "default"}},
	}}}}}
	h := storeBackedHandlers(t, cfg)
	h.config().SetSelections([]config.Selection{{Provider: "fw", Credential: "default", Model: "fw-qwen", Upstream: "acct/qwen"}})
	if _, ok := listedModels(h.config())["fw-qwen"]; !ok {
		t.Fatal("a catalog model must be listed")
	}
	ctx := context.Background()
	spec, err := h.GetModelSpec(ctx, &ModelSpecInput{Name: "fw-qwen"})
	if err != nil || !spec.Body.Exists || spec.Body.Spec.Upstream != "acct/qwen" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	in := &UpsertModelInput{Name: "fw-qwen", Body: spec.Body.Spec}
	in.Body.MaxConcurrent = 3
	if _, err := h.UpsertModel(ctx, in); err != nil {
		t.Fatalf("save refused: %v", err)
	}
	m, ok := reloadStored(t, h, "after save").Extensions["fw"].Providers["fw"].Provides["qwen"]
	if !ok || m.MaxConcurrent != 3 || m.Upstream != "acct/qwen" {
		t.Fatalf("stored = %+v (%v), want 3 slots under the provider with its upstream", m, ok)
	}
}
