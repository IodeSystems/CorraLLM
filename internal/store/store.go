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
    source_ip         TEXT    NOT NULL DEFAULT '', -- client IP (via middleware.RealIP / X-Forwarded-For)
    path              TEXT    NOT NULL,          -- request path
    status            INTEGER NOT NULL,          -- HTTP status
    dwell_ms          INTEGER NOT NULL DEFAULT 0, -- time in request
    prompt_tokens     INTEGER NOT NULL DEFAULT 0, -- metered prompt tokens (P6)
    completion_tokens INTEGER NOT NULL DEFAULT 0, -- metered completion tokens (P6)
    cost_usd          REAL    NOT NULL DEFAULT 0, -- resolved request cost in $ (P6)
    queued_ms         INTEGER NOT NULL DEFAULT 0, -- time spent queued before admit/reject (P8-beyond)
    audio_bytes       INTEGER NOT NULL DEFAULT 0, -- metered audio request bytes, STT/TTS (P9c)
    error             TEXT    NOT NULL DEFAULT '', -- proxy/backpressure error reason, if any (P10a)
    ttfb_ms           INTEGER NOT NULL DEFAULT 0, -- time to first response byte (P10b)
    cached_tokens     INTEGER NOT NULL DEFAULT 0, -- backend-reported cached prompt tokens
    prompt_per_sec    REAL    NOT NULL DEFAULT 0, -- backend-reported prompt-processing speed (tp/s)
    predicted_per_sec REAL    NOT NULL DEFAULT 0, -- backend-reported generation speed (tg/s)
    req_body          TEXT    NOT NULL DEFAULT '', -- captured request payload, capped (P10b)
    resp_body         TEXT    NOT NULL DEFAULT '' -- captured response payload, capped (P10b)
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts);

-- Periodic snapshots of instantaneous per-lane admission load (P8-beyond), so
-- queue depth is visible even before requests resolve. Sparse: only non-idle
-- lanes are sampled. ("grp" — "group" is reserved.)
-- bench_results: one row per (run, model). Published by llm-bench at the end of
-- a run, NOT derived from serving traffic.
--
-- Persisted, unlike capability verdicts (which live in memory because a verdict
-- describes what a backend does RIGHT NOW and a stale one would assert something
-- nobody rechecked). A completed run is the opposite: a historical fact about a
-- measurement that happened at a point in time, and losing the set on restart
-- would mean re-benching every model just to compare them again.
CREATE TABLE IF NOT EXISTS bench_results (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id            TEXT    NOT NULL,
  model             TEXT    NOT NULL,
  at                INTEGER NOT NULL,
  classes           TEXT    NOT NULL DEFAULT '',
  stages            INTEGER NOT NULL DEFAULT 0,
  stages_passed     INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  wall_ms           INTEGER NOT NULL DEFAULT 0,
  tok_per_sec       REAL    NOT NULL DEFAULT 0,
  footprint_mib     INTEGER NOT NULL DEFAULT 0,
  UNIQUE(run_id, model)
);
CREATE INDEX IF NOT EXISTS bench_results_model_at ON bench_results(model, at DESC);

-- bench_probe_results: one row per (run, model, probe, run_mode) — the detail
-- bench_results throws away.
--
-- bench_results aggregates every probe a model ran into a single pass rate,
-- which is only meaningful if the probes are comparable. They are not: a probe
-- the model cannot serve is SKIPPED, not failed, so an STT model runs its four
-- audio probes, passes them, and scores 100% while a chat model running twenty
-- mixed probes scores 90% — and the table ranks the STT model above it. The
-- capability column is what makes the two comparable again (compare chat to
-- chat), and the per-probe rows are what let the console answer "which probe,
-- and how did it do" instead of just showing an aggregate.
--
-- Skipped probes are recorded, not omitted. "This model ran no chat probes
-- because it has no chat capability" and "this model has no chat results yet"
-- look identical when the rows simply aren't there, and that ambiguity is the
-- thing that made the aggregate misleading in the first place.
CREATE TABLE IF NOT EXISTS bench_probe_results (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  at            INTEGER NOT NULL,
  probe         TEXT    NOT NULL,          -- task name, e.g. "capability-stt"
  class         TEXT    NOT NULL DEFAULT '', -- coding | tooluse | adversarial | capability
  capability    TEXT    NOT NULL DEFAULT '', -- serving surface the probe needs: chat | audio.stt | ...
  run_mode      TEXT    NOT NULL DEFAULT '', -- "" | cold | warm
  stages        INTEGER NOT NULL DEFAULT 0,
  stages_passed INTEGER NOT NULL DEFAULT 0,
  checks_passed INTEGER NOT NULL DEFAULT 0,
  checks_total  INTEGER NOT NULL DEFAULT 0,
  pass          INTEGER NOT NULL DEFAULT 0, -- every stage passed
  wall_ms       INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  skip_reason   TEXT    NOT NULL DEFAULT '',
  note          TEXT    NOT NULL DEFAULT '', -- first failing check, or combo error
  UNIQUE(run_id, model, probe, run_mode)
);
CREATE INDEX IF NOT EXISTS bench_probe_results_model_at ON bench_probe_results(model, at DESC);
CREATE INDEX IF NOT EXISTS bench_probe_results_run ON bench_probe_results(run_id, model);

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
	`ALTER TABLE activity ADD COLUMN audio_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN error TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN ttfb_ms INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN req_body TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN resp_body TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN source_ip TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE activity ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN prompt_per_sec REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE activity ADD COLUMN predicted_per_sec REAL NOT NULL DEFAULT 0`,
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
	ID               int64 // row id (P10b; 0 until persisted, set on read)
	TS               int64 // unix millis
	Served           string
	Backend          string
	Key              string
	SourceIP         string // client IP resolved via middleware.RealIP (X-Forwarded-For), "" if unknown
	Path             string
	Status           int
	DwellMS          int64
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int     // backend-reported cached prompt tokens
	PromptPerSec     float64 // backend-reported prompt-processing speed (tp/s)
	PredictedPerSec  float64 // backend-reported generation speed (tg/s)
	CostUSD          float64
	QueuedMS         int64  // time queued before admission/reject (P8-beyond)
	AudioBytes       int64  // metered audio request bytes for STT/TTS routes (P9c); 0 for text
	Error            string // proxy/backpressure error reason, if any (P10a); "" on success
	TTFBMs           int64  // time to first response byte (P10b)
	ReqBody          string // captured request payload, capped+summarized (P10b)
	RespBody         string // captured response payload, capped+summarized (P10b)
}

// InsertActivity appends a request record to the activity log.
func (s *Store) InsertActivity(a Activity) error {
	_, err := s.db.Exec(
		`INSERT INTO activity (ts, served, backend, key, source_ip, path, status, dwell_ms,
		                       prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error,
		                       ttfb_ms, cached_tokens, prompt_per_sec, predicted_per_sec, req_body, resp_body)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Backend, a.Key, a.SourceIP, a.Path, a.Status, a.DwellMS,
		a.PromptTokens, a.CompletionTokens, a.CostUSD, a.QueuedMS, a.AudioBytes, a.Error,
		a.TTFBMs, a.CachedTokens, a.PromptPerSec, a.PredictedPerSec, a.ReqBody, a.RespBody,
	)
	return err
}

// ActivityByID returns one full activity record including the captured payloads
// (P10b/P10c — the detail modal). The list query (RecentActivity) omits payloads
// to stay lean; this fetches them on demand.
func (s *Store) ActivityByID(id int64) (Activity, error) {
	var a Activity
	err := s.db.QueryRow(
		`SELECT id, ts, served, backend, key, source_ip, path, status, dwell_ms,
		        prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error,
		        ttfb_ms, cached_tokens, prompt_per_sec, predicted_per_sec, req_body, resp_body
		 FROM activity WHERE id = ?`, id).Scan(
		&a.ID, &a.TS, &a.Served, &a.Backend, &a.Key, &a.SourceIP, &a.Path, &a.Status, &a.DwellMS,
		&a.PromptTokens, &a.CompletionTokens, &a.CostUSD, &a.QueuedMS, &a.AudioBytes, &a.Error,
		&a.TTFBMs, &a.CachedTokens, &a.PromptPerSec, &a.PredictedPerSec, &a.ReqBody, &a.RespBody)
	return a, err
}

// PruneActivity deletes activity rows older than beforeMS (retention), returning
// the number removed. SQLite reuses the freed pages, so the file plateaus at
// steady state rather than growing unbounded (no VACUUM needed).
func (s *Store) PruneActivity(beforeMS int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM activity WHERE ts < ?`, beforeMS)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecentActivity returns the most recent records, newest first.
func (s *Store) RecentActivity(limit int) ([]Activity, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, served, backend, key, source_ip, path, status, dwell_ms,
		        prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error, ttfb_ms,
		        cached_tokens, prompt_per_sec, predicted_per_sec
		 FROM activity ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.TS, &a.Served, &a.Backend, &a.Key, &a.SourceIP, &a.Path, &a.Status, &a.DwellMS,
			&a.PromptTokens, &a.CompletionTokens, &a.CostUSD, &a.QueuedMS, &a.AudioBytes, &a.Error, &a.TTFBMs,
			&a.CachedTokens, &a.PromptPerSec, &a.PredictedPerSec); err != nil {
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

// BenchResult is one model's aggregate outcome from one bench run.
//
// PromptTokens is the total prompted; CachedTokens is the part that was served
// from the prompt cache. "Tokens processed" for comparison purposes is
// PromptTokens - CachedTokens: charging a model for cache hits would flatter
// whichever model happened to run second on the same fixtures.
type BenchResult struct {
	ID               int64   `json:"id"`
	RunID            string  `json:"runId"`
	Model            string  `json:"model"`
	At               int64   `json:"at"`
	Classes          string  `json:"classes"`
	Stages           int     `json:"stages"`
	StagesPassed     int     `json:"stagesPassed"`
	PromptTokens     int     `json:"promptTokens"`
	CachedTokens     int     `json:"cachedTokens"`
	CompletionTokens int     `json:"completionTokens"`
	WallMS           int64   `json:"wallMs"`
	TokPerSec        float64 `json:"tokPerSec"`
	FootprintMiB     int     `json:"footprintMiB"`
}

// SaveBenchResult upserts one (run, model) aggregate. Re-publishing the same
// pair replaces it, so a re-run or a retried publish cannot double-count.
func (s *Store) SaveBenchResult(ctx context.Context, r BenchResult) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO bench_results
  (run_id, model, at, classes, stages, stages_passed, prompt_tokens, cached_tokens, completion_tokens, wall_ms, tok_per_sec, footprint_mib)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, model) DO UPDATE SET
  at=excluded.at, classes=excluded.classes, stages=excluded.stages,
  stages_passed=excluded.stages_passed, prompt_tokens=excluded.prompt_tokens,
  cached_tokens=excluded.cached_tokens, completion_tokens=excluded.completion_tokens,
  wall_ms=excluded.wall_ms, tok_per_sec=excluded.tok_per_sec, footprint_mib=excluded.footprint_mib`,
		r.RunID, r.Model, r.At, r.Classes, r.Stages, r.StagesPassed,
		r.PromptTokens, r.CachedTokens, r.CompletionTokens, r.WallMS, r.TokPerSec, r.FootprintMiB)
	return err
}

// LatestBenchResults returns the most recent result per model — the comparison
// view's data. Older runs stay in the table for history but do not compete with
// the current one in a side-by-side.
func (s *Store) LatestBenchResults(ctx context.Context) ([]BenchResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.run_id, b.model, b.at, b.classes, b.stages, b.stages_passed,
       b.prompt_tokens, b.cached_tokens, b.completion_tokens, b.wall_ms, b.tok_per_sec, b.footprint_mib
FROM bench_results b
JOIN (SELECT model, MAX(at) AS at FROM bench_results GROUP BY model) m
  ON m.model = b.model AND m.at = b.at
ORDER BY b.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchResults(rows)
}

// BenchResultsFor returns a model's history, newest first.
func (s *Store) BenchResultsFor(ctx context.Context, model string, limit int) ([]BenchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, run_id, model, at, classes, stages, stages_passed,
       prompt_tokens, cached_tokens, completion_tokens, wall_ms, tok_per_sec, footprint_mib
FROM bench_results WHERE model = ? ORDER BY at DESC LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchResults(rows)
}

// BenchProbeResult is one probe's outcome for one model in one run, at one
// residency mode. Stages/StagesPassed are that probe's own stages, so a probe
// score stands on its own rather than being diluted into a run-wide average.
//
// Skipped rows carry SkipReason and zero counts: they say the probe was never a
// candidate (wrong capability or undeclared modality), which is a configuration
// fact and must not read as a capability gap.
type BenchProbeResult struct {
	ID           int64  `json:"id"`
	RunID        string `json:"runId"`
	Model        string `json:"model"`
	At           int64  `json:"at"`
	Probe        string `json:"probe"`
	Class        string `json:"class"`
	Capability   string `json:"capability"`
	RunMode      string `json:"runMode"`
	Stages       int    `json:"stages"`
	StagesPassed int    `json:"stagesPassed"`
	ChecksPassed int    `json:"checksPassed"`
	ChecksTotal  int    `json:"checksTotal"`
	Pass         bool   `json:"pass"`
	WallMS       int64  `json:"wallMs"`
	Skipped      bool   `json:"skipped"`
	SkipReason   string `json:"skipReason"`
	Note         string `json:"note"`
}

// SaveBenchProbeResults upserts a batch of probe rows in one transaction.
// Keyed by (run, model, probe, runMode), so a retried publish replaces rather
// than duplicates.
func (s *Store) SaveBenchProbeResults(ctx context.Context, rows []BenchProbeResult) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO bench_probe_results
  (run_id, model, at, probe, class, capability, run_mode, stages, stages_passed,
   checks_passed, checks_total, pass, wall_ms, skipped, skip_reason, note)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, model, probe, run_mode) DO UPDATE SET
  at=excluded.at, class=excluded.class, capability=excluded.capability,
  stages=excluded.stages, stages_passed=excluded.stages_passed,
  checks_passed=excluded.checks_passed, checks_total=excluded.checks_total,
  pass=excluded.pass, wall_ms=excluded.wall_ms, skipped=excluded.skipped,
  skip_reason=excluded.skip_reason, note=excluded.note`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.RunID, r.Model, r.At, r.Probe, r.Class,
			r.Capability, r.RunMode, r.Stages, r.StagesPassed, r.ChecksPassed,
			r.ChecksTotal, boolInt(r.Pass), r.WallMS, boolInt(r.Skipped),
			r.SkipReason, r.Note); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BenchProbeResultsFor returns a model's probe rows. With runID set it scopes to
// that run; empty runID returns the model's most recent run only — the console
// asks "how did the last bench go", and mixing runs would average away the
// regression it is there to show.
func (s *Store) BenchProbeResultsFor(ctx context.Context, model, runID string) ([]BenchProbeResult, error) {
	const cols = `id, run_id, model, at, probe, class, capability, run_mode, stages,
       stages_passed, checks_passed, checks_total, pass, wall_ms, skipped, skip_reason, note`
	var (
		rows *sql.Rows
		err  error
	)
	if runID != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+`
FROM bench_probe_results WHERE model = ? AND run_id = ?
ORDER BY capability, class, probe, run_mode`, model, runID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+`
FROM bench_probe_results
WHERE model = ? AND run_id = (SELECT run_id FROM bench_probe_results WHERE model = ? ORDER BY at DESC LIMIT 1)
ORDER BY capability, class, probe, run_mode`, model, model)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchProbeResults(rows)
}

// LatestBenchProbeResults returns every model's most recent run's probe rows —
// the cross-model comparison's data.
//
// Latest-per-model rather than latest-overall: models are benched at different
// times, and scoping to one run id would silently drop every model that was not
// in it, which reads as "no data" rather than "not in that run".
func (s *Store) LatestBenchProbeResults(ctx context.Context) ([]BenchProbeResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.run_id, b.model, b.at, b.probe, b.class, b.capability, b.run_mode,
       b.stages, b.stages_passed, b.checks_passed, b.checks_total, b.pass,
       b.wall_ms, b.skipped, b.skip_reason, b.note
FROM bench_probe_results b
JOIN (SELECT model, MAX(at) AS at FROM bench_probe_results GROUP BY model) m
  ON m.model = b.model AND m.at = b.at
ORDER BY b.capability, b.model, b.probe, b.run_mode`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchProbeResults(rows)
}

func scanBenchProbeResults(rows *sql.Rows) ([]BenchProbeResult, error) {
	var out []BenchProbeResult
	for rows.Next() {
		var r BenchProbeResult
		var pass, skipped int
		if err := rows.Scan(&r.ID, &r.RunID, &r.Model, &r.At, &r.Probe, &r.Class,
			&r.Capability, &r.RunMode, &r.Stages, &r.StagesPassed, &r.ChecksPassed,
			&r.ChecksTotal, &pass, &r.WallMS, &skipped, &r.SkipReason, &r.Note); err != nil {
			return nil, err
		}
		r.Pass, r.Skipped = pass != 0, skipped != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanBenchResults(rows *sql.Rows) ([]BenchResult, error) {
	var out []BenchResult
	for rows.Next() {
		var r BenchResult
		if err := rows.Scan(&r.ID, &r.RunID, &r.Model, &r.At, &r.Classes, &r.Stages, &r.StagesPassed,
			&r.PromptTokens, &r.CachedTokens, &r.CompletionTokens, &r.WallMS, &r.TokPerSec, &r.FootprintMiB); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
