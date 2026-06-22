// Package store is corrallm's embedded SQLite persistence. A proxy is mostly
// stateless, so this is deliberately thin: an activity log (P1) and a place for
// persisted metric rollups (P8). Live metrics live in an in-memory ring, not
// here. modernc.org/sqlite is pure-Go (no CGO) so the binary cross-compiles.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schema is applied idempotently on Open. Migrations stay inline until the
// schema is large enough to warrant golang-migrate.
const schema = `
CREATE TABLE IF NOT EXISTS activity (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,          -- unix millis
    served     TEXT    NOT NULL,          -- served model name
    backend    TEXT    NOT NULL,          -- backend that handled it
    key        TEXT    NOT NULL DEFAULT '', -- caller identity
    path       TEXT    NOT NULL,          -- request path
    status     INTEGER NOT NULL,          -- HTTP status
    dwell_ms   INTEGER NOT NULL DEFAULT 0 -- time in request
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts);
`

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the SQLite database at path and applies the
// schema. Use ":memory:" for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite is single-writer; one connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for query layers added in later phases.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
