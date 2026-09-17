package configdb

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

// oldModelTable is config_model EXACTLY as the schema defined it before
// `disabled` existed — the shape of the live database at ~/.corrallm/var.
//
// Copied verbatim rather than derived, because the point of the test is to
// upgrade a table this code did not create. A definition regenerated from the
// current schema would test nothing.
const oldModelTable = `
CREATE TABLE config_model (
    provider            TEXT NOT NULL DEFAULT '',
    name                TEXT NOT NULL,
    type                TEXT NOT NULL DEFAULT '',
    quality             REAL NOT NULL DEFAULT 0,
    cmd                 TEXT NOT NULL DEFAULT '',
    server              TEXT NOT NULL DEFAULT '',
    upstream            TEXT NOT NULL DEFAULT '',
    max_concurrent      INTEGER NOT NULL DEFAULT 0,
    context_per_request INTEGER NOT NULL DEFAULT 0,
    persistent          INTEGER NOT NULL DEFAULT 0,
    notes               TEXT NOT NULL DEFAULT '',
    proxy_json          TEXT NOT NULL DEFAULT '',
    aliases_json        TEXT NOT NULL DEFAULT '',
    sticky_json         TEXT NOT NULL DEFAULT '',
    swap_json           TEXT NOT NULL DEFAULT '',
    ram_usage_json      TEXT NOT NULL DEFAULT '',
    modalities_json     TEXT NOT NULL DEFAULT '',
    convert_json        TEXT NOT NULL DEFAULT '',
    limits_json         TEXT NOT NULL DEFAULT '',
    placements_json     TEXT NOT NULL DEFAULT '',
    sampling_json       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, name)
)`

func disabledConfig(disabled bool) *config.Config {
	return &config.Config{Models: map[string]config.Model{
		"groq-gpt-oss-120b": {Type: "chat", Quality: 3, Upstream: "openai/gpt-oss-120b", Disabled: disabled},
		"local-floor":       {Type: "chat", Quality: 1},
	}}
}

// The failure this guards is the one internal/store documents and this package
// already hit once: CREATE TABLE IF NOT EXISTS is a no-op on an existing
// database, so a new column exists only on fresh installs and every write on
// the production box dies with "no such column".
//
// Build the table the OLD schema built, put a row in it, then Apply.
func TestApplyAddsDisabledToAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx, oldModelTable); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config_model (provider, name, type, quality) VALUES ('local', 'legacy', 'chat', 2)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply over an existing table: %v", err)
	}
	// Every daemon start runs Apply, so a migration that only works once is a
	// migration that breaks the second boot.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply is not idempotent: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply is not idempotent on the third run: %v", err)
	}
	if err := Write(ctx, db, disabledConfig(true)); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Models["groq-gpt-oss-120b"].Disabled {
		t.Error("disabled did not survive a migrated database")
	}
	if got.Models["local-floor"].Disabled {
		t.Error("an enabled model came back disabled")
	}
}

// A row written before the column existed reads as ENABLED — which is what it
// was. Failing the other way would take every model on the box out of service
// on the first restart after the upgrade.
func TestLegacyModelRowIsEnabled(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx, oldModelTable); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config_model (provider, name, type, quality, proxy_json)
		 VALUES ('', 'legacy', 'chat', 2, '8080')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, ok := got.Models["legacy"]
	if !ok {
		t.Fatal("the pre-existing row did not survive the migration")
	}
	if m.Disabled {
		t.Error("a row that predates the column came back disabled — the upgrade must be inert")
	}
	if m.Type != "chat" || m.Quality != 2 {
		t.Errorf("the pre-existing row changed: %+v", m)
	}
}

// False must not be stored as a difference. `disabled` is omitempty, so an
// enabled model's map has no such key, and putBool omits a false column —
// which is what keeps the round trip byte-identical for every config that has
// never used this.
func TestEnabledModelRoundTripsUnchanged(t *testing.T) {
	in := disabledConfig(false)
	compare(t, in, roundTrip(t, in))
}

func TestDisabledModelRoundTrips(t *testing.T) {
	in := disabledConfig(true)
	compare(t, in, roundTrip(t, in))
}
