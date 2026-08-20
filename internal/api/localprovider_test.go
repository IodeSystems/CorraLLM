package api

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/configdb"

	_ "modernc.org/sqlite"
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

// localHandlers builds handlers backed by a real configuration STORE, which is
// what production uses. They were backed by a managed file, and a file-backed
// edit path no longer exists — so a test using one would exercise a code path
// nothing ships.
//
// The returned path is where the fixture came from; assertions that used to
// re-read the file now reload from the store instead.
func localHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	path := managedConfig(t, localProviderYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return storeBackedHandlers(t, cfg), path
}

// storeBackedHandlers wires Handlers to a real configuration store, which is
// the only writable path production has.
func storeBackedHandlers(t *testing.T, cfg *config.Config) *Handlers {
	t.Helper()
	h := &Handlers{}
	src := testConfigSource(t, cfg)
	h.ConfigSource = src
	h.UpdateConfig = func(ctx context.Context, fn func(*config.Config) error) error {
		next, err := src.Update(ctx, fn)
		if err != nil {
			return err
		}
		h.SetConfig(next)
		return nil
	}
	h.SetConfig(cfg)
	return h
}

// reloadStored reads the config back OUT of the store.
//
// These assertions used to re-read the config FILE, which no longer receives
// edits — so they were checking a file nothing writes and would have passed
// forever without noticing a broken save. Reading from the store is what
// actually proves an edit persisted.
func reloadStored(t *testing.T, h *Handlers, when string) *config.Config {
	t.Helper()
	c, err := h.ConfigSource.Load(context.Background())
	if err != nil {
		t.Fatalf("stored config does not load %s: %v", when, err)
	}
	return c
}

// testConfigSource is an in-memory configuration store seeded with cfg.
func testConfigSource(t *testing.T, cfg *config.Config) *configdb.Source {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := configdb.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	src := &configdb.Source{DB: db}
	if err := src.Save(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return src
}

// A model owned by a provider must be CREATED, EDITED and DELETED in the
// provider's block. c.Models holds only a folded copy, and forWriting drops it,
// so any handler that treats c.Models as the source of truth reports success
// and changes nothing on disk. That bug appeared three separate times — in the
// writer, in upsert and in delete — which is what earns it a test.
func TestProviderOwnedModelRoundTripsThroughItsBlock(t *testing.T) {
	h, _ := localHandlers(t)

	// CREATE under the provider.
	in := &UpsertModelInput{Name: "local-fresh", Provider: "local"}
	in.Body = ModelSpec{Name: "local-fresh", Proxy: "127.0.0.1:9003", Type: "chat"}
	if _, err := h.UpsertModel(context.Background(), in); err != nil {
		t.Fatalf("create: %v", err)
	}
	saved := reloadStored(t, h, "after create")
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
	saved = reloadStored(t, h, "after edit")
	if got := saved.Providers["local"].Models["keeper"].Proxy; got.IsZero() {
		t.Error("edit did not reach the provider block")
	}

	// DELETE removes it from the block, so it does not come back on reload.
	h.SetConfig(saved)
	del := &DeleteEntryInput{Kind: "model", Name: "local-goner"}
	if _, err := h.DeleteEntry(context.Background(), del); err != nil {
		t.Fatalf("delete: %v", err)
	}
	saved = reloadStored(t, h, "after delete")
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

const remoteProviderYAML = `extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
        provides:
          llama-70b:
            upstream: llama-3.3-70b-versatile
            type: chat
            quality: 3
          spare-70b:
            upstream: spare
            type: chat
            quality: 2
`

// A REMOTE provider's model is authored under
// extensions.<ext>.providers.<p>.provides — a third location, distinct from a
// top-level provider's block and from a bare top-level model. It must also be
// stripped of anything describing a local process: a stray cmd here would make
// corrallm try to RUN somebody else's hosted model.
func TestRemoteProviderModelRoundTrip(t *testing.T) {
	path := managedConfig(t, remoteProviderYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)

	in := &UpsertModelInput{Name: "groq-kimi-k2", Provider: "groq", Extension: "free"}
	in.Body = ModelSpec{
		Name: "groq-kimi-k2", Upstream: "moonshotai/kimi-k2-instruct",
		Type: "chat", Quality: 4,
		// Deliberately hostile input: a caller sending process fields for a
		// remote model must not have them persisted.
		Cmd: "llama-server --port 1", Server: "box1", Proxy: "127.0.0.1:1234",
	}
	if _, err := h.UpsertModel(context.Background(), in); err != nil {
		t.Fatalf("create: %v", err)
	}
	saved := reloadStored(t, h, "reload")
	got, ok := saved.Extensions["free"].Providers["groq"].Provides["kimi-k2"]
	if !ok {
		t.Fatalf("not authored under the provider's provides: %v",
			saved.Extensions["free"].Providers["groq"].Provides)
	}
	if got.Upstream != "moonshotai/kimi-k2-instruct" || got.Quality != 4 {
		t.Errorf("fields lost: %+v", got)
	}
	if got.Cmd != "" || got.Server != "" || !got.Proxy.IsZero() {
		t.Errorf("a remote model kept local-process fields — corrallm would try to run it: cmd=%q server=%q", got.Cmd, got.Server)
	}
	// It serves under the provider-prefixed name, reached through the
	// provider's own endpoint.
	m, ok := saved.Models["groq-kimi-k2"]
	if !ok {
		t.Fatal("does not resolve under its served name")
	}
	tgt, err := m.ProxyTarget()
	if err != nil {
		t.Fatalf("no usable target: %v", err)
	}
	if tgt.URL.Hostname() != "api.groq.com" {
		t.Errorf("target = %s, want the provider's endpoint", tgt.URL)
	}
	if tgt.Model != "moonshotai/kimi-k2-instruct" {
		t.Errorf("upstream id not sent on the wire: %q", tgt.Model)
	}
	// The provider's existing model survived.
	if _, ok := saved.Extensions["free"].Providers["groq"].Provides["llama-70b"]; !ok {
		t.Error("the write clobbered the provider's other model")
	}
}

// Deleting a REMOTE provider's model has to reach its provides block — the
// fourth location this same trap appears in. Creating something you cannot
// delete is worse than not being able to create it.
func TestRemoteProviderModelDeletes(t *testing.T) {
	path := managedConfig(t, remoteProviderYAML)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)

	if _, err := h.DeleteEntry(context.Background(),
		&DeleteEntryInput{Kind: "model", Name: "groq-llama-70b"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	saved := reloadStored(t, h, "reload")
	if _, back := saved.Extensions["free"].Providers["groq"].Provides["llama-70b"]; back {
		t.Error("still in the provider's provides; it would return on the next load")
	}
	if _, back := saved.Models["groq-llama-70b"]; back {
		t.Error("still resolves")
	}
	if _, ok := saved.Extensions["free"].Providers["groq"].Provides["spare-70b"]; !ok {
		t.Error("the delete took an unrelated model with it")
	}
}

// Deleting a provider's LAST model would leave an endpoint nothing can reach.
// Config validation already refuses that, but it complains about provider shape
// rather than about the delete — so the refusal is raised here, naming the ways
// out.
func TestDeletingAProvidersLastModelIsRefusedClearly(t *testing.T) {
	path := managedConfig(t, `extensions:
  free:
    providers:
      groq:
        proxy: {host: api.groq.com, port: 443, basePath: /openai}
        provides:
          only-one:
            upstream: x
            type: chat
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := storeBackedHandlers(t, cfg)
	_, err = h.DeleteEntry(context.Background(), &DeleteEntryInput{Kind: "model", Name: "groq-only-one"})
	if err == nil {
		t.Fatal("deleting the only model of a provider was allowed")
	}
	if !strings.Contains(err.Error(), "only model") {
		t.Errorf("message does not explain the refusal: %v", err)
	}
	// And it must not have half-applied: the model is still there.
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Extensions["free"].Providers["groq"].Provides["only-one"]; !ok {
		t.Error("the refused delete still removed the model")
	}
}
