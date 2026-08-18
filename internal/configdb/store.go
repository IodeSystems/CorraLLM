package configdb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/iodesystems/corrallm/internal/config"
)

// Apply creates the config tables. Idempotent, like every other schema here.
func Apply(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("config schema: %w", err)
	}
	return nil
}

// Write replaces the stored configuration with c.
//
// ONE TRANSACTION, and the whole set is rewritten inside it. A file could not be
// half-written; neither can this. Replacing rather than merging is what makes a
// deletion work at all — a model removed from c has to disappear, and a
// merge-only write would resurrect it on every save.
func Write(ctx context.Context, db *sql.DB, c *config.Config) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, t := range allTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}

	if err := writeScalars(ctx, tx, c); err != nil {
		return err
	}
	if err := writeServers(ctx, tx, c); err != nil {
		return err
	}
	if err := writeModels(ctx, tx, c); err != nil {
		return err
	}
	if err := writeLanes(ctx, tx, c); err != nil {
		return err
	}
	if err := writeGroups(ctx, tx, c); err != nil {
		return err
	}
	if err := writeKeys(ctx, tx, c); err != nil {
		return err
	}
	if err := writeTools(ctx, tx, c); err != nil {
		return err
	}
	if err := writeExtensions(ctx, tx, c); err != nil {
		return err
	}
	if err := writeProviders(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}

// Read assembles a Config from the tables.
//
// It returns the config as WRITTEN, not as resolved: extensions are not
// expanded into models and nothing is validated here. That is deliberate — the
// file path does resolution and validation in config.Load, after parsing, and
// doing it in two places is how the two drift.
func Read(ctx context.Context, db *sql.DB) (*config.Config, error) {
	c := &config.Config{}
	if err := readScalars(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readServers(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readModels(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readLanes(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readGroups(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readKeys(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readTools(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readExtensions(ctx, db, c); err != nil {
		return nil, err
	}
	if err := readProviders(ctx, db, c); err != nil {
		return nil, err
	}
	return c, nil
}

// IsEmpty reports whether any configuration has been stored.
//
// The question a boot asks: an empty database with a file present is the
// one-time import, and an empty database with no file is a fresh install.
func IsEmpty(ctx context.Context, db *sql.DB) (bool, error) {
	for _, t := range allTables {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}

// allTables is every table Write owns, and the set IsEmpty inspects. Listed
// once so a new table cannot be added to the schema and forgotten by the
// clear-and-replace — which would leave stale rows a save was supposed to drop.
var allTables = []string{
	"config_scalar",
	"config_server",
	"config_server_pool",
	"config_server_device",
	"config_model",
	"config_lane",
	"config_lane_member",
	"config_group",
	"config_key",
	"config_tool",
	"config_tool_host",
	"config_extension",
	"config_provider",
	"config_local_provider",
}

// --- scalars ---------------------------------------------------------------

// The top-level singletons. Each is stored under its yaml key so the table
// reads like the file it replaces, and so adding one needs no schema change.
//
// The whole-config map is produced once and drained: every section with its own
// table is removed as it is written, and whatever is LEFT is the set of
// scalars. That way a new top-level field is persisted the day it is added
// rather than silently dropped — the exact failure that motivated this port.
var sectionKeys = []string{
	"include", "servers", "models", "lanes", "priorityGroups",
	"keys", "tools", "extensions", "providers",
}

func writeScalars(ctx context.Context, tx *sql.Tx, c *config.Config) error {
	m, err := toMap(c)
	if err != nil {
		return err
	}
	for _, k := range sectionKeys {
		delete(m, k)
	}
	for k, v := range m {
		blob, err := takeJSONValue(v)
		if err != nil {
			return fmt.Errorf("scalar %s: %w", k, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_scalar (key, value) VALUES (?, ?)`, k, blob); err != nil {
			return fmt.Errorf("write scalar %s: %w", k, err)
		}
	}
	return nil
}

func readScalars(ctx context.Context, db *sql.DB, c *config.Config) error {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM config_scalar`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]any{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if err := putJSON(m, k, v); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(m) == 0 {
		return nil
	}
	return fromMap(m, c)
}
