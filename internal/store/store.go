// Package store is corrallm's embedded SQLite persistence. A proxy is mostly
// stateless, so this is deliberately thin: an activity log (P1) and a place for
// persisted metric rollups (P8). Live metrics live in an in-memory ring, not
// here. modernc.org/sqlite is pure-Go (no CGO) so the binary cross-compiles.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// schema is applied idempotently on Open. Migrations stay inline until the
// schema is large enough to warrant golang-migrate.
const schema = `
CREATE TABLE IF NOT EXISTS activity (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,          -- unix millis
    served            TEXT    NOT NULL,          -- served model name
    backend           TEXT    NOT NULL,          -- backend that handled it
    key               TEXT    NOT NULL DEFAULT '', -- caller identity
    path              TEXT    NOT NULL,          -- request path
    status            INTEGER NOT NULL,          -- HTTP status
    dwell_ms          INTEGER NOT NULL DEFAULT 0, -- time in request
    prompt_tokens     INTEGER NOT NULL DEFAULT 0, -- metered prompt tokens (P6)
    completion_tokens INTEGER NOT NULL DEFAULT 0, -- metered completion tokens (P6)
    cost_usd          REAL    NOT NULL DEFAULT 0  -- resolved request cost in $ (P6)
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts);
`

// migrations upgrade an activity table created by an earlier schema in place.
// Each is run once on Open; a "duplicate column" error means it already applied
// (SQLite has no ADD COLUMN IF NOT EXISTS), so it is swallowed.
var migrations = []string{
	`ALTER TABLE activity ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`,
}

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
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Activity is one proxied request record. The token/cost fields are metered on
// served requests (P6); the explicit error/backpressure paths (429/503, client
// 499) record them as zero. A request preempted mid-serve still records the cost
// actually consumed before the abort (partial tokens + any swap energy spent).
type Activity struct {
	TS               int64 // unix millis
	Served           string
	Backend          string
	Key              string
	Path             string
	Status           int
	DwellMS          int64
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

// InsertActivity appends a request record to the activity log.
func (s *Store) InsertActivity(a Activity) error {
	_, err := s.db.Exec(
		`INSERT INTO activity (ts, served, backend, key, path, status, dwell_ms,
		                       prompt_tokens, completion_tokens, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Backend, a.Key, a.Path, a.Status, a.DwellMS,
		a.PromptTokens, a.CompletionTokens, a.CostUSD,
	)
	return err
}

// RecentActivity returns the most recent records, newest first.
func (s *Store) RecentActivity(limit int) ([]Activity, error) {
	rows, err := s.db.Query(
		`SELECT ts, served, backend, key, path, status, dwell_ms,
		        prompt_tokens, completion_tokens, cost_usd
		 FROM activity ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.TS, &a.Served, &a.Backend, &a.Key, &a.Path, &a.Status, &a.DwellMS,
			&a.PromptTokens, &a.CompletionTokens, &a.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DB exposes the underlying handle for query layers added in later phases.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
