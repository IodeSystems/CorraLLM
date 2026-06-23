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
    cost_usd          REAL    NOT NULL DEFAULT 0, -- resolved request cost in $ (P6)
    queued_ms         INTEGER NOT NULL DEFAULT 0  -- time spent queued before admit/reject (P8-beyond)
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts);

-- Periodic snapshots of instantaneous per-lane admission load (P8-beyond), so
-- queue depth is visible even before requests resolve. Sparse: only non-idle
-- lanes are sampled. ("grp" — "group" is reserved.)
CREATE TABLE IF NOT EXISTS lane_samples (
    ts      INTEGER NOT NULL,   -- unix millis of the sample
    grp     TEXT    NOT NULL,   -- priority group
    active  INTEGER NOT NULL,   -- in-flight slots across backends
    waiting INTEGER NOT NULL    -- queued requests across backends
);
CREATE INDEX IF NOT EXISTS idx_lane_samples_ts ON lane_samples(ts);
`

// migrations upgrade an activity table created by an earlier schema in place.
// Each is run once on Open; a "duplicate column" error means it already applied
// (SQLite has no ADD COLUMN IF NOT EXISTS), so it is swallowed.
var migrations = []string{
	`ALTER TABLE activity ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN queued_ms INTEGER NOT NULL DEFAULT 0`,
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
	QueuedMS         int64 // time queued before admission/reject (P8-beyond)
}

// InsertActivity appends a request record to the activity log.
func (s *Store) InsertActivity(a Activity) error {
	_, err := s.db.Exec(
		`INSERT INTO activity (ts, served, backend, key, path, status, dwell_ms,
		                       prompt_tokens, completion_tokens, cost_usd, queued_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Backend, a.Key, a.Path, a.Status, a.DwellMS,
		a.PromptTokens, a.CompletionTokens, a.CostUSD, a.QueuedMS,
	)
	return err
}

// RecentActivity returns the most recent records, newest first.
func (s *Store) RecentActivity(limit int) ([]Activity, error) {
	rows, err := s.db.Query(
		`SELECT ts, served, backend, key, path, status, dwell_ms,
		        prompt_tokens, completion_tokens, cost_usd, queued_ms
		 FROM activity ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.TS, &a.Served, &a.Backend, &a.Key, &a.Path, &a.Status, &a.DwellMS,
			&a.PromptTokens, &a.CompletionTokens, &a.CostUSD, &a.QueuedMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Rollup is aggregated activity for one served model over a window (P8).
type Rollup struct {
	Served           string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	DwellMS          int64
	CostUSD          float64
}

// RollupByModel aggregates activity at or after sinceMS, grouped by served
// model, ordered by cost (then request count) descending. sinceMS <= 0 covers
// all records.
func (s *Store) RollupByModel(sinceMS int64) ([]Rollup, error) {
	rows, err := s.db.Query(
		`SELECT served,
		        COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(dwell_ms), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM activity WHERE ts >= ?
		 GROUP BY served
		 ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Rollup
	for rows.Next() {
		var r Rollup
		if err := rows.Scan(&r.Served, &r.Requests, &r.PromptTokens, &r.CompletionTokens,
			&r.DwellMS, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeyRollup is aggregated activity for one caller key over a window (P8).
type KeyRollup struct {
	Key              string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	DwellMS          int64
	CostUSD          float64
}

// RollupByKey aggregates activity at or after sinceMS, grouped by caller key,
// ordered by cost (then request count) descending. sinceMS <= 0 covers all
// records. An empty key means an unkeyed caller.
func (s *Store) RollupByKey(sinceMS int64) ([]KeyRollup, error) {
	rows, err := s.db.Query(
		`SELECT key,
		        COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(dwell_ms), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM activity WHERE ts >= ?
		 GROUP BY key
		 ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KeyRollup
	for rows.Next() {
		var r KeyRollup
		if err := rows.Scan(&r.Key, &r.Requests, &r.PromptTokens, &r.CompletionTokens,
			&r.DwellMS, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeriesRow is one (key, time-bucket) aggregate for time-series charts (P8).
type SeriesRow struct {
	BucketTS         int64 // bucket start, unix millis (ts floored to bucketMS)
	Key              string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	DwellMS          int64
	CostUSD          float64
	QueuedMS         int64 // total time queued before admit/reject (P8-beyond)
	Rejected         int64 // requests backpressured with 429 (queue pressure)
}

// RollupSeries aggregates activity at or after sinceMS into time buckets of
// bucketMS, grouped by (bucket, caller key), ordered by bucket then key. It is
// the backing query for per-key time-series graphs.
func (s *Store) RollupSeries(sinceMS, bucketMS int64) ([]SeriesRow, error) {
	if bucketMS <= 0 {
		bucketMS = 3600_000 // default 1h
	}
	rows, err := s.db.Query(
		`SELECT (ts / ?) * ?      AS bucket,
		        key,
		        COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(dwell_ms), 0),
		        COALESCE(SUM(cost_usd), 0),
		        COALESCE(SUM(queued_ms), 0),
		        SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END)
		 FROM activity WHERE ts >= ?
		 GROUP BY bucket, key
		 ORDER BY bucket, key`, bucketMS, bucketMS, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SeriesRow
	for rows.Next() {
		var r SeriesRow
		if err := rows.Scan(&r.BucketTS, &r.Key, &r.Requests, &r.PromptTokens,
			&r.CompletionTokens, &r.DwellMS, &r.CostUSD, &r.QueuedMS, &r.Rejected); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LaneSample is one priority group's instantaneous load at a sample tick.
type LaneSample struct {
	Group   string
	Active  int
	Waiting int
}

// InsertLaneSamples records a batch of per-lane load samples at ts.
func (s *Store) InsertLaneSamples(ts int64, samples []LaneSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO lane_samples (ts, grp, active, waiting) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, sm := range samples {
		if _, err := stmt.Exec(ts, sm.Group, sm.Active, sm.Waiting); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// PruneLaneSamples deletes samples older than beforeMS (retention).
func (s *Store) PruneLaneSamples(beforeMS int64) error {
	_, err := s.db.Exec(`DELETE FROM lane_samples WHERE ts < ?`, beforeMS)
	return err
}

// LaneDepthRow is one (bucket, group) aggregate of sampled load.
type LaneDepthRow struct {
	BucketTS   int64
	Group      string
	AvgActive  float64
	AvgWaiting float64
	MaxWaiting int64
}

// LaneDepthSeries buckets the lane samples at/after sinceMS into bucketMS
// windows, reporting mean active/waiting and peak waiting per (bucket, group).
func (s *Store) LaneDepthSeries(sinceMS, bucketMS int64) ([]LaneDepthRow, error) {
	if bucketMS <= 0 {
		bucketMS = 3600_000
	}
	rows, err := s.db.Query(
		`SELECT (ts / ?) * ? AS bucket, grp,
		        AVG(active), AVG(waiting), MAX(waiting)
		 FROM lane_samples WHERE ts >= ?
		 GROUP BY bucket, grp
		 ORDER BY bucket, grp`, bucketMS, bucketMS, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LaneDepthRow
	for rows.Next() {
		var r LaneDepthRow
		if err := rows.Scan(&r.BucketTS, &r.Group, &r.AvgActive, &r.AvgWaiting, &r.MaxWaiting); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DB exposes the underlying handle for query layers added in later phases.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
