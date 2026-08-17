package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

const localProviderYAML = `servers:
  box1:
    pools: {gpu0: 24GB}
providers:
  local:
    models:
      keeper:
        proxy: 127.0.0.1:9001
        type: chat
      goner:
        proxy: 127.0.0.1:9002
        type: chat
`

func localHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	path := managedConfig(t, localProviderYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{ConfigPath: path}
	h.SetConfig(cfg)
	return h, path
}

// A model owned by a provider must be CREATED, EDITED and DELETED in the
// provider's block. c.Models holds only a folded copy, and forWriting drops it,
// so any handler that treats c.Models as the source of truth reports success
// and changes nothing on disk. That bug appeared three separate times — in the
// writer, in upsert and in delete — which is what earns it a test.
func TestProviderOwnedModelRoundTripsThroughItsBlock(t *testing.T) {
	h, path := localHandlers(t)

	// CREATE under the provider.
	in := &UpsertModelInput{Name: "local-fresh", Provider: "local"}
	in.Body = ModelSpec{Name: "local-fresh", Proxy: "127.0.0.1:9003", Type: "chat"}
	if _, err := h.UpsertModel(context.Background(), in); err != nil {
		t.Fatalf("create: %v", err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("config no longer loads after create: %v", err)
	}
	if _, ok := saved.Providers["local"].Models["fresh"]; !ok {
		t.Errorf("created model not in the provider block: %v", saved.Providers["local"].Models)
	}
	if _, ok := saved.Models["local-fresh"]; !ok {
		t.Error("created model does not resolve under its served name")
	}

	// EDIT: the change lands in the block, not as a top-level orphan.
	h.SetConfig(saved)
	edit := &UpsertModelInput{Name: "local-keeper"}
	edit.Body = ModelSpec{Name: "local-keeper", Proxy: "127.0.0.1:9999", Type: "chat"}
	if _, err := h.UpsertModel(context.Background(), edit); err != nil {
		t.Fatalf("edit: %v", err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatalf("config no longer loads after edit: %v", err)
	}
	if got := saved.Providers["local"].Models["keeper"].Proxy; got.IsZero() {
		t.Error("edit did not reach the provider block")
	}

	// DELETE removes it from the block, so it does not come back on reload.
	h.SetConfig(saved)
	del := &DeleteEntryInput{Kind: "model", Name: "local-goner"}
	if _, err := h.DeleteEntry(context.Background(), del); err != nil {
		t.Fatalf("delete: %v", err)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatalf("config no longer loads after delete: %v", err)
	}
	if _, back := saved.Providers["local"].Models["goner"]; back {
		t.Error("deleted model is still in the provider block; it would return on the next load")
	}
	if _, back := saved.Models["local-goner"]; back {
		t.Error("deleted model still resolves")
	}
	// The others survived.
	if _, ok := saved.Providers["local"].Models["keeper"]; !ok {
		t.Error("delete took an unrelated model with it")
	}
}

// TestListProvidersReadsTheLiveConfig: a reload installs a new config through
// SetConfig, and a handler reading the construction-time field reports the
// world as it was at startup. That is invisible until something changes — here,
// a provider gaining notes.
func TestListProvidersReadsTheLiveConfig(t *testing.T) {
	h, _ := localHandlers(t)
	updated, err := config.Load(managedConfig(t, localProviderYAML+"    notes: hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	h.SetConfig(updated)
	out, err := h.ListProviders(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Local) != 1 {
		t.Fatalf("want the local provider, got %+v", out.Body.Local)
	}
	if out.Body.Local[0].Notes != "hello" {
		t.Errorf("notes = %q — the handler is reading the config it was built with, not the live one", out.Body.Local[0].Notes)
	}
}
