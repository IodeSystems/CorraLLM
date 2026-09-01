package configdb

import (
	"context"
	"database/sql"
	"testing"

	"github.com/iodesystems/corrallm/internal/config"
)

func policyConfig() *config.Config {
	return &config.Config{
		PriorityGroups: map[string]config.PriorityGroup{
			"interactive": {Weight: 10},
			"batch":       {Weight: 1},
			"default":     {Weight: 1},
		},
		Keys: map[string]config.KeyPolicy{
			"yscr":   {Group: "default"},
			"sk-aw4": {Group: "batch", Allow: map[string]bool{"interactive": true}},
		},
	}
}

func TestEscalationsSurviveTheDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := Write(ctx, db, policyConfig()); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if p := got.Keys["sk-aw4"]; p.Group != "batch" || !p.Allow["interactive"] {
		t.Errorf("sk-aw4 = %+v, want batch + interactive", p)
	}
	if p := got.Keys["yscr"]; p.Group != "default" || len(p.Allow) != 0 {
		t.Errorf("yscr = %+v, want default with no escalations", p)
	}
}

// The failure this guards is the one internal/store documents: CREATE TABLE IF
// NOT EXISTS is a no-op on an existing database, so a new column exists only on
// fresh installs and every write on the production box dies with "no such
// column". Build the OLD table, then Apply, and require it to work.
func TestApplyAddsAllowToAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE config_key (key TEXT PRIMARY KEY, group_name TEXT NOT NULL)`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config_key (key, group_name) VALUES ('legacy', 'default')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply over an existing table: %v", err)
	}
	// Applying twice must also work — every daemon start runs it.
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply is not idempotent: %v", err)
	}
	if err := Write(ctx, db, policyConfig()); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Keys["sk-aw4"].Allow["interactive"] {
		t.Error("escalation did not survive a migrated database")
	}
}

// A row written before the column existed reads as "no escalation" — failing
// closed, rather than granting a permission nobody wrote.
func TestLegacyRowGrantsNoEscalation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config_key (key, group_name) VALUES ('legacy', 'default')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := Read(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if p := got.Keys["legacy"]; len(p.Allow) != 0 {
		t.Errorf("legacy row granted %+v; a missing value must grant nothing", p.Allow)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
