// Package store is corrallm's embedded SQLite persistence. A proxy is mostly
// stateless, so this is deliberately thin: an activity log (P1) and a place for
// persisted metric rollups (P8). Live metrics live in an in-memory ring, not
// here. modernc.org/sqlite is pure-Go (no CGO) so the binary cross-compiles.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
    placement         TEXT    NOT NULL DEFAULT '', -- WHICH placement served it (box + cmd); '' predates the column
    key               TEXT    NOT NULL DEFAULT '', -- caller identity
    source_ip         TEXT    NOT NULL DEFAULT '', -- client IP (via middleware.RealIP / X-Forwarded-For)
    path              TEXT    NOT NULL,          -- request path
    status            INTEGER NOT NULL,          -- HTTP status
    dwell_ms          INTEGER NOT NULL DEFAULT 0, -- time in request
    prompt_tokens     INTEGER NOT NULL DEFAULT 0, -- metered prompt tokens (P6)
    completion_tokens INTEGER NOT NULL DEFAULT 0, -- metered completion tokens (P6)
    cost_usd          REAL    NOT NULL DEFAULT 0, -- resolved request cost in $ (P6)
    queued_ms         INTEGER NOT NULL DEFAULT 0, -- time spent queued before admit/reject (P8-beyond)
    load_ms           INTEGER NOT NULL DEFAULT 0, -- time waiting for a backend to become resident (cold spawn + health)
    audio_bytes       INTEGER NOT NULL DEFAULT 0, -- metered audio request bytes, STT/TTS (P9c)
    error             TEXT    NOT NULL DEFAULT '', -- proxy/backpressure error reason, if any (P10a)
    ttfb_ms           INTEGER NOT NULL DEFAULT 0, -- time to first response byte (P10b)
    cached_tokens     INTEGER NOT NULL DEFAULT 0, -- backend-reported cached prompt tokens
    prompt_per_sec    REAL    NOT NULL DEFAULT 0, -- backend-reported prompt-processing speed (tp/s)
    predicted_per_sec REAL    NOT NULL DEFAULT 0, -- backend-reported generation speed (tg/s)
    req_body          TEXT    NOT NULL DEFAULT '', -- captured request payload, capped (P10b)
    resp_body         TEXT    NOT NULL DEFAULT '', -- captured response payload, capped (P10b)
    retry_after_ms    INTEGER NOT NULL DEFAULT 0  -- the Retry-After we PROMISED this caller on a 429 (P15)
);
CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts);
-- Correlating a promise with the caller's next request ("did they come back,
-- and when") is a per-key lookup by time, which the ts-only index can't serve.
CREATE INDEX IF NOT EXISTS idx_activity_key_ts ON activity(key, ts);

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
-- An ARM is (toolset, tool_format, run_mode): the same probe against the same
-- model, varied deliberately. Every one of those belongs in the key. Keying on
-- run_mode alone averaged the arms of an A/B into a single record — destroying
-- the comparison the arms existed to make.
CREATE TABLE IF NOT EXISTS bench_probe_results (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  at            INTEGER NOT NULL,
  probe         TEXT    NOT NULL,          -- task name, e.g. "capability-stt"
  class         TEXT    NOT NULL DEFAULT '', -- coding | tooluse | adversarial | capability
  capability    TEXT    NOT NULL DEFAULT '', -- serving surface the probe needs: chat | audio.stt | ...
  run_mode      TEXT    NOT NULL DEFAULT '', -- "" | cold | warm
  toolset       TEXT    NOT NULL DEFAULT '', -- A/B arm: tool surface offered
  tool_format   TEXT    NOT NULL DEFAULT '', -- A/B arm: tool-result encoding (json | toon | tight | …)
  -- repeat: 0-based index of which re-run of the SAME arm this is (--runs N,
  -- and any pass an agent retried). Part of the identity, because two repeats
  -- are two independent samples — folding them summed their stages and checks
  -- and produced rows reporting an exact multiple of the probe's real size.
  repeat        INTEGER NOT NULL DEFAULT 0,
  stages        INTEGER NOT NULL DEFAULT 0,
  stages_passed INTEGER NOT NULL DEFAULT 0,
  checks_passed INTEGER NOT NULL DEFAULT 0,
  checks_total  INTEGER NOT NULL DEFAULT 0,
  pass          INTEGER NOT NULL DEFAULT 0, -- every stage passed
  wall_ms       INTEGER NOT NULL DEFAULT 0,
  new_prompt_tokens INTEGER NOT NULL DEFAULT 0, -- prompt actually evaluated; the comparable token cost
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  skip_reason   TEXT    NOT NULL DEFAULT '',
  note          TEXT    NOT NULL DEFAULT '', -- first failing check, or combo error
  UNIQUE(run_id, model, probe, run_mode, toolset, tool_format, repeat)
);
CREATE INDEX IF NOT EXISTS bench_probe_results_model_at ON bench_probe_results(model, at DESC);
CREATE INDEX IF NOT EXISTS bench_probe_results_run ON bench_probe_results(run_id, model);

-- bench_probe_stages / bench_probe_checks: the evidence behind a probe's score.
--
-- "This probe scored 1/3" does not say WHY, and the why already exists — it is
-- in out/<ts>/runs.jsonl on the bench host and nowhere else. These two tables
-- carry the small, structured part of it (which check failed, and what the
-- stage cost) so a post-mortem survives out/ being deleted. The bulky replay —
-- transcripts and tool-call journals — stays on disk and is served from
-- bench_runs.out_dir, because duplicating it into SQLite would grow the DB by
-- the size of every conversation ever benched to serve a rare read.
CREATE TABLE IF NOT EXISTS bench_probe_stages (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id         TEXT    NOT NULL,
  model          TEXT    NOT NULL,
  probe          TEXT    NOT NULL,
  run_mode       TEXT    NOT NULL DEFAULT '',
  toolset        TEXT    NOT NULL DEFAULT '',
  tool_format    TEXT    NOT NULL DEFAULT '',
  stage          INTEGER NOT NULL,
  prompt         TEXT    NOT NULL DEFAULT '',
  pass           INTEGER NOT NULL DEFAULT 0,
  limit_breached INTEGER NOT NULL DEFAULT 0,
  note           TEXT    NOT NULL DEFAULT '',
  turns          INTEGER NOT NULL DEFAULT 0,
  tool_calls     INTEGER NOT NULL DEFAULT 0,
  -- new_prompt_tokens, not prompt_tokens: the cached prefix is sent every turn
  -- and evaluated once, so summing the prompt charges the tool schema per turn
  -- and made a ~12% gap look like 2.2x.
  new_prompt_tokens   INTEGER NOT NULL DEFAULT 0,
  completion_tokens   INTEGER NOT NULL DEFAULT 0,
  invalid_arg_retries INTEGER NOT NULL DEFAULT 0,
  json_errors         INTEGER NOT NULL DEFAULT 0,
  repeated_calls      INTEGER NOT NULL DEFAULT 0,
  bait_calls          INTEGER NOT NULL DEFAULT 0,
  broken_intermediates INTEGER NOT NULL DEFAULT 0,
  compactions    INTEGER NOT NULL DEFAULT 0,
  tok_per_sec    REAL    NOT NULL DEFAULT 0,
  wall_ms        INTEGER NOT NULL DEFAULT 0,
  -- queued_ms/exec_ms split wall_ms into waiting and working. Without them the
  -- dashboard can only show wall time, which on a shared box is dominated by
  -- whatever else was queued and moves with the neighbours rather than the model.
  queued_ms      INTEGER NOT NULL DEFAULT 0,
  exec_ms        INTEGER NOT NULL DEFAULT 0,
  UNIQUE(run_id, model, probe, run_mode, toolset, tool_format, stage)
);
CREATE INDEX IF NOT EXISTS bench_probe_stages_probe
  ON bench_probe_stages(run_id, model, probe);

CREATE TABLE IF NOT EXISTS bench_probe_checks (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id   TEXT    NOT NULL,
  model    TEXT    NOT NULL,
  probe    TEXT    NOT NULL,
  run_mode TEXT    NOT NULL DEFAULT '',
  toolset  TEXT    NOT NULL DEFAULT '',
  tool_format TEXT NOT NULL DEFAULT '',
  stage    INTEGER NOT NULL,
  idx      INTEGER NOT NULL,          -- order within the stage; checks have no natural key
  kind     TEXT    NOT NULL DEFAULT '',
  descr    TEXT    NOT NULL DEFAULT '', -- ("desc" is reserved)
  pass     INTEGER NOT NULL DEFAULT 0,
  detail   TEXT    NOT NULL DEFAULT '',
  UNIQUE(run_id, model, probe, run_mode, toolset, tool_format, stage, idx)
);
CREATE INDEX IF NOT EXISTS bench_probe_checks_probe
  ON bench_probe_checks(run_id, model, probe);

-- bench_runs: where a run's artifacts landed, so the transcript/journal
-- endpoints can find them.
--
-- Recorded rather than inferred: llm-bench's --out defaults to ./out relative
-- to ITS cwd, so the path depends on how the run was launched and corrallm
-- previously learned it only by scraping "wrote out/<ts>" from the child's
-- stdout. Host is stored because a run benched elsewhere has artifacts this
-- server cannot read, and saying so beats a confusing empty transcript.
CREATE TABLE IF NOT EXISTS bench_runs (
  run_id  TEXT    PRIMARY KEY,
  out_dir TEXT    NOT NULL DEFAULT '',
  host    TEXT    NOT NULL DEFAULT '',
  at      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS lane_samples (
    ts      INTEGER NOT NULL,   -- unix millis of the sample
    grp     TEXT    NOT NULL,   -- priority group
    active  INTEGER NOT NULL,   -- in-flight slots across backends
    waiting INTEGER NOT NULL    -- queued requests across backends
);
CREATE INDEX IF NOT EXISTS idx_lane_samples_ts ON lane_samples(ts);

-- quota_counter: durable state for a counter-mode backend's falloff counters
-- (P16 durability). A header-tracked backend needs no persistence — it relearns
-- its budget from the next response's X-Ratelimit-* headers — but a
-- locally-counted daily budget (OpenRouter's 1000/day, which sends no headers)
-- would reset to zero on every restart and over-send against the provider's real
-- cap. The counter is a leaky bucket, so its whole state is (level, updatedAt):
-- on load the ledger decays the level for the elapsed downtime.
CREATE TABLE IF NOT EXISTS quota_counter (
    backend TEXT NOT NULL,          -- served model name = one key = one budget
    label   TEXT NOT NULL,          -- "1m" | "1d"
    used    REAL    NOT NULL DEFAULT 0, -- decaying fill level as of the at column
    at      INTEGER NOT NULL DEFAULT 0, -- unix millis when used was last updated
    PRIMARY KEY (backend, label)
);

-- model_pause: an operator's "do not run this" order. Operational state, not
-- config: it lives here rather than in the YAML so pausing a model for an
-- afternoon does not rewrite the user's configuration file. It MUST be durable
-- though — an in-memory pause means a restart quietly reloads the very model
-- the operator was keeping off the box. Rows are deleted on unpause and when
-- resume_at passes.
--
-- The key is a PROCESS, not a name: a served model name, or "extension:<name>"
-- when an extension hosts the process. That is what gives a pause the same
-- blast radius as an unload — an extension's models are one process, so they
-- pause and resume together.
CREATE TABLE IF NOT EXISTS model_pause (
    target    TEXT    NOT NULL PRIMARY KEY, -- process key: model name | "extension:<name>"
    reason    TEXT    NOT NULL DEFAULT '',  -- free text: why it is off
    at        INTEGER NOT NULL DEFAULT 0,   -- unix millis the pause was set
    resume_at INTEGER NOT NULL DEFAULT 0    -- unix millis it lifts; 0 = indefinite
);
`

// migrations upgrade an activity table created by an earlier schema in place.
//
// Each runs once on Open, and two errors mean "already applied" rather than
// failure, because SQLite has neither ADD COLUMN IF NOT EXISTS nor DROP COLUMN
// IF EXISTS:
//
//   - "duplicate column" — an ADD whose column is already there.
//   - "no such column"  — a DROP on a database created FRESH from the current
//     schema, which never had the column to begin with. Without this a new
//     install fails to open at all, which is a worse outcome than the tidiness
//     the DROP was for.
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
	`ALTER TABLE activity ADD COLUMN finish_reason TEXT NOT NULL DEFAULT ''`,
	// WHERE the request was served. A model can be placed on more than one box,
	// so `backend` — the served model name — stopped identifying where the work
	// actually happened. Two requests to one model can now differ by machine,
	// quantisation and context window, and nothing in this table could say so.
	//
	// Old rows keep '' rather than being backfilled. There is no honest value to
	// backfill WITH: the whole reason the column exists is that the placement is
	// not derivable from the model, and guessing the only placement that exists
	// today would silently mislabel history the moment a second one is added.
	`ALTER TABLE activity ADD COLUMN placement TEXT NOT NULL DEFAULT ''`,
	// And `backend` goes. It held the SERVED MODEL NAME — identical to `served`
	// on every row ever written — which identified where work happened only
	// while a model had one home. `placement` is that answer now, and keeping a
	// duplicate of `served` beside it invites reading one as the machine.
	//
	// Dropped rather than deprecated: a column nothing writes and nothing reads
	// is a question every future reader has to answer again. Data loss is nil —
	// it duplicated `served`, which stays.
	`ALTER TABLE activity DROP COLUMN backend`,
	// Cold-spawn wait, split out from dwell. A request that waited 6.7s for a
	// model to load and then answered in 375ms is not a slow model, and without
	// this column nothing downstream can tell the two apart.
	`ALTER TABLE activity ADD COLUMN load_ms INTEGER NOT NULL DEFAULT 0`,
	// The retry hint we handed a rejected caller. Until this column the promise
	// was written to the wire and forgotten: the log could say "we rejected this
	// key at 14:03" but never "…and told them to come back in 4s", so nothing
	// could check whether the estimate was honest or who was already scheduled
	// to return. Rows written before this land at 0, which reads as "no promise
	// recorded" — correct, since none was.
	`ALTER TABLE activity ADD COLUMN retry_after_ms INTEGER NOT NULL DEFAULT 0`,
	// Stage timing split. Rows written before this land at 0/0, which reads as
	// "no queueing measured" — correct for the exclusive runs that predate the
	// shared mode, since those held the lease and nothing else could queue.
	`ALTER TABLE bench_probe_stages ADD COLUMN queued_ms INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE bench_probe_stages ADD COLUMN exec_ms INTEGER NOT NULL DEFAULT 0`,
}

// dropStaleProbeTables removes a bench_probe_results created before A/B arms
// were part of its identity, so the schema below can recreate it correctly.
//
// A rebuild rather than an ALTER because the fix is to the UNIQUE constraint —
// the old key was (run, model, probe, run_mode), which UPSERTs one arm of an
// A/B over another instead of storing both — and SQLite cannot alter a
// constraint in place. ADDing the columns alone would leave the broken key and
// silently keep collapsing arms.
//
// Dropping the rows is acceptable ONLY because this table has never carried a
// real run: it was added and revised in the same unreleased change, and every
// row in it is either synthetic or re-derivable by re-benching. If it ever ships
// with real history, this must become a copy-into-new-table migration instead.
func dropStaleProbeTables(ctx context.Context, db *sql.DB) error {
	var ddl string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='bench_probe_results'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil // fresh database; the schema creates the current shape
	}
	if err != nil {
		return fmt.Errorf("inspect bench_probe_results: %w", err)
	}
	if strings.Contains(ddl, "toolset") {
		return nil // already the arm-aware shape
	}
	for _, t := range []string{"bench_probe_results", "bench_probe_stages", "bench_probe_checks"} {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
			return fmt.Errorf("drop stale %s: %w", t, err)
		}
	}
	return nil
}

// dropStaleModelPause removes a model_pause created before a pause keyed on the
// PROCESS rather than the model, so the schema can recreate it with the right
// column.
//
// The first cut keyed by served model name (column `model`); an extension's
// models are one process, so a pause has to key by ProcKey — a model name, or
// "extension:<name>" — and the column is now `target`. CREATE TABLE IF NOT
// EXISTS would leave the old shape in place and every query against it would
// fail on the missing column.
//
// Dropping rows is acceptable ONLY because this table has never shipped: it was
// added and revised in the same unreleased change, and a pause is a live
// operator decision that is trivially re-made. If it ever ships, this must
// become a copy-into-new-table migration instead.
func dropStaleModelPause(ctx context.Context, db *sql.DB) error {
	var ddl string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='model_pause'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil // fresh database; the schema creates the current shape
	}
	if err != nil {
		return fmt.Errorf("inspect model_pause: %w", err)
	}
	if strings.Contains(ddl, "target") {
		return nil // already the process-keyed shape
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS model_pause`); err != nil {
		return fmt.Errorf("drop stale model_pause: %w", err)
	}
	return nil
}

// addProbeResultRepeat widens bench_probe_results' identity to include the
// repeat index, PRESERVING existing rows.
//
// This is the copy-into-new-table migration dropStaleProbeTables said would be
// required "if it ever ships with real history". It has: the table now holds
// months of runs on the production box, so dropping is no longer an option, and
// SQLite still cannot alter a UNIQUE constraint in place.
//
// Existing rows land at repeat 0. That is correct for every row written by a
// single-pass run, and it is the only honest answer for rows that folded
// repeats: the per-repeat detail was summed away before it was stored and
// cannot be recovered here. Those rows keep their inflated counts and stay
// recognisable by disagreeing with their probe's stage count — which is what
// the dashboard flags — rather than being silently halved into a number nothing
// measured.
func addProbeResultRepeat(ctx context.Context, db *sql.DB) error {
	var ddl string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='bench_probe_results'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return nil // fresh database; the schema creates the current shape
	}
	if err != nil {
		return fmt.Errorf("inspect bench_probe_results: %w", err)
	}
	if strings.Contains(ddl, "repeat") {
		return nil // already widened
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The new table is created under a temporary name and renamed into place, so
	// a failure at any point leaves the original untouched rather than half a
	// schema. The indexes are dropped first because SQLite carries index names
	// across a rename and they would collide when the schema recreates them.
	stmts := []string{
		`DROP INDEX IF EXISTS bench_probe_results_model_at`,
		`DROP INDEX IF EXISTS bench_probe_results_run`,
		`CREATE TABLE bench_probe_results_new (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT    NOT NULL,
  model         TEXT    NOT NULL,
  at            INTEGER NOT NULL,
  probe         TEXT    NOT NULL,
  class         TEXT    NOT NULL DEFAULT '',
  capability    TEXT    NOT NULL DEFAULT '',
  run_mode      TEXT    NOT NULL DEFAULT '',
  toolset       TEXT    NOT NULL DEFAULT '',
  tool_format   TEXT    NOT NULL DEFAULT '',
  repeat        INTEGER NOT NULL DEFAULT 0,
  stages        INTEGER NOT NULL DEFAULT 0,
  stages_passed INTEGER NOT NULL DEFAULT 0,
  checks_passed INTEGER NOT NULL DEFAULT 0,
  checks_total  INTEGER NOT NULL DEFAULT 0,
  pass          INTEGER NOT NULL DEFAULT 0,
  wall_ms       INTEGER NOT NULL DEFAULT 0,
  new_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  skipped       INTEGER NOT NULL DEFAULT 0,
  skip_reason   TEXT    NOT NULL DEFAULT '',
  note          TEXT    NOT NULL DEFAULT '',
  UNIQUE(run_id, model, probe, run_mode, toolset, tool_format, repeat)
)`,
		`INSERT INTO bench_probe_results_new
  (id, run_id, model, at, probe, class, capability, run_mode, toolset, tool_format,
   repeat, stages, stages_passed, checks_passed, checks_total, pass, wall_ms,
   new_prompt_tokens, completion_tokens, skipped, skip_reason, note)
 SELECT id, run_id, model, at, probe, class, capability, run_mode, toolset, tool_format,
   0, stages, stages_passed, checks_passed, checks_total, pass, wall_ms,
   new_prompt_tokens, completion_tokens, skipped, skip_reason, note
 FROM bench_probe_results`,
		`DROP TABLE bench_probe_results`,
		`ALTER TABLE bench_probe_results_new RENAME TO bench_probe_results`,
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("widen bench_probe_results: %w", err)
		}
	}
	return tx.Commit()
}

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the SQLite database at path and applies the
// schema. Use ":memory:" for tests.
//
// The parent directory is created first. SQLite creates the FILE but not the
// directory holding it, so a first run against the default path failed at boot
// with "unable to open database file (14)" — a message that names neither the
// path nor the actual problem. Every other component that owns a path on disk
// (config.Save, the tune cache) already MkdirAlls; this brings the DB in line.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." && !strings.HasPrefix(path, ":") {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite is single-writer; one connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if err := dropStaleProbeTables(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := dropStaleModelPause(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Before the schema below runs: it CREATEs IF NOT EXISTS, so an existing
	// table keeps its old shape and the widening has to happen first.
	if err := addProbeResultRepeat(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, enrollSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply enrollment schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") &&
			!strings.Contains(err.Error(), "no such column") {
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
	ID     int64 // row id (P10b; 0 until persisted, set on read)
	TS     int64 // unix millis
	Served string
	// Placement is which way of serving Served handled this request: the box
	// and the cmd. Empty on rows written before the column existed, and on any
	// request served by something corrallm does not place (a pure proxy).
	Placement        string
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
	LoadMS           int64  // time waiting for the backend to become resident
	AudioBytes       int64  // metered audio request bytes for STT/TTS routes (P9c); 0 for text
	Error            string // proxy/backpressure error reason, if any (P10a); "" on success
	TTFBMs           int64  // time to first response byte (P10b)
	// FinishReason is the model's own account of why it stopped: "stop" (it
	// chose to), "length" (it ran into a cap), "tool_calls", "content_filter".
	// Empty when the backend did not report one, or the reply exceeded the
	// capture cap.
	//
	// The distinction that matters operationally is stop vs length: a reply that
	// ended because it hit a wall did NOT finish, and a run of them is the
	// signature of a caller with no max_tokens whose generations are running
	// away — visible here per request rather than by reading a backend's slots.
	FinishReason string
	ReqBody      string // captured request payload, capped+summarized (P10b)
	RespBody     string // captured response payload, capped+summarized (P10b)
	// RetryAfterMS is the backoff we PROMISED this caller when we turned them
	// away — exactly the value that went out on the Retry-After header, not a
	// re-derivation. Zero on anything that wasn't a 429 (and on 429 rows written
	// before the column existed).
	//
	// Recorded because a promise is a scheduled future arrival: TS+RetryAfterMS
	// is when we said to come back, so the log can answer "who is due back, and
	// when" and — by comparing against the caller's next request — whether the
	// estimate was honest.
	RetryAfterMS int64
}

// InsertActivity appends a request record to the activity log.
func (s *Store) InsertActivity(a Activity) error {
	_, err := s.db.Exec(
		`INSERT INTO activity (ts, served, placement, key, source_ip, path, status, dwell_ms,
		                       prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error,
		                       ttfb_ms, cached_tokens, prompt_per_sec, predicted_per_sec, req_body, resp_body,
		                       finish_reason, load_ms, retry_after_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Placement, a.Key, a.SourceIP, a.Path, a.Status, a.DwellMS,
		a.PromptTokens, a.CompletionTokens, a.CostUSD, a.QueuedMS, a.AudioBytes, a.Error,
		a.TTFBMs, a.CachedTokens, a.PromptPerSec, a.PredictedPerSec, a.ReqBody, a.RespBody,
		a.FinishReason, a.LoadMS, a.RetryAfterMS,
	)
	return err
}

// ActivityByID returns one full activity record including the captured payloads
// (P10b/P10c — the detail modal). The list query (RecentActivity) omits payloads
// to stay lean; this fetches them on demand.
func (s *Store) ActivityByID(id int64) (Activity, error) {
	var a Activity
	err := s.db.QueryRow(
		`SELECT id, ts, served, placement, key, source_ip, path, status, dwell_ms,
		        prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error,
		        ttfb_ms, cached_tokens, prompt_per_sec, predicted_per_sec, req_body, resp_body,
		        finish_reason, load_ms, retry_after_ms
		 FROM activity WHERE id = ?`, id).Scan(
		&a.ID, &a.TS, &a.Served, &a.Placement, &a.Key, &a.SourceIP, &a.Path, &a.Status, &a.DwellMS,
		&a.PromptTokens, &a.CompletionTokens, &a.CostUSD, &a.QueuedMS, &a.AudioBytes, &a.Error,
		&a.TTFBMs, &a.CachedTokens, &a.PromptPerSec, &a.PredictedPerSec, &a.ReqBody, &a.RespBody,
		&a.FinishReason, &a.LoadMS, &a.RetryAfterMS)
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

// RecentActivity returns the most recent records, newest first. A non-empty
// served filters to one model's requests (the per-model console usage tab);
// empty returns every model's activity (the global activity page).
// RecentActivity returns the newest rows, optionally narrowed to one served
// model and/or one caller key.
//
// The key filter is what turns the caller roster from a list into something you
// can act on: "this key did 9,971 requests" invites the question "doing what",
// and without it the only answer was to read every row and squint.
//
// Built by composition rather than by another if/else pair — two independent
// filters are four branches, and the next one is eight.
// A placement filter answers the question the whole placement thread was built
// toward: not "how does this model behave" but "how does it behave ON THAT
// BOX". With one model served from two machines, a mean across both describes
// neither.
func (s *Store) RecentActivity(limit int, served, key, placement string) ([]Activity, error) {
	const cols = `id, ts, served, placement, key, source_ip, path, status, dwell_ms,
	        prompt_tokens, completion_tokens, cost_usd, queued_ms, audio_bytes, error, ttfb_ms,
	        cached_tokens, prompt_per_sec, predicted_per_sec, finish_reason, load_ms, retry_after_ms`
	q := `SELECT ` + cols + ` FROM activity`
	var args []any
	var where []string
	if served != "" {
		where = append(where, "served = ?")
		args = append(args, served)
	}
	if key != "" {
		where = append(where, "key = ?")
		args = append(args, key)
	}
	if placement != "" {
		where = append(where, "placement = ?")
		args = append(args, placement)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.TS, &a.Served, &a.Placement, &a.Key, &a.SourceIP, &a.Path, &a.Status, &a.DwellMS,
			&a.PromptTokens, &a.CompletionTokens, &a.CostUSD, &a.QueuedMS, &a.AudioBytes, &a.Error, &a.TTFBMs,
			&a.CachedTokens, &a.PromptPerSec, &a.PredictedPerSec, &a.FinishReason, &a.LoadMS,
			&a.RetryAfterMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RetryPromise is one "come back later" we handed a caller: who we turned away,
// when, how long we told them to wait — and when they actually came back.
//
// A 429 is not just a rejection, it is an appointment. The caller was told a
// time and (if they honor Retry-After) will return at it, so every outstanding
// promise is a scheduled future arrival that the queue's own waiter count cannot
// see. Surfacing them makes two things checkable that were previously invisible:
// who is due back, and whether the number we gave them was honest.
type RetryPromise struct {
	ID           int64
	TS           int64  // unix millis the promise was made
	Key          string // caller identity; empty for an unkeyed caller
	SourceIP     string
	Served       string
	Reason       string // backpressure reason: rejected | queue-timeout | exhausted | bumped
	RetryAfterMS int64  // what we promised
	// ReturnedMS is the caller's next request of any kind after TS, or 0 if they
	// have not been back. Compared against TS+RetryAfterMS it says whether the
	// caller honored the hint (returned at or after it), jumped the gun, or gave
	// up entirely.
	ReturnedMS int64
}

// RetryPromises returns the "come back later" hints handed out at or after
// sinceMS, newest first, optionally narrowed to one caller key.
//
// Caller identity for the return correlation is the key, falling back to the
// source IP for unkeyed callers — without the fallback every anonymous caller
// would share one bucket and the first unrelated anonymous request would look
// like an instant return. Two hosts sharing one key still collapse into one
// caller (the return is then the earliest of either), which is conservative: it
// can report a return sooner than the promised caller made it, never later.
func (s *Store) RetryPromises(sinceMS int64, limit int, key string) ([]RetryPromise, error) {
	q := `SELECT a.id, a.ts, a.key, a.source_ip, a.served, a.error, a.retry_after_ms,
	             COALESCE((SELECT MIN(b.ts) FROM activity b
	                        WHERE b.ts > a.ts
	                          AND CASE WHEN a.key <> '' THEN b.key = a.key
	                                   ELSE b.key = '' AND b.source_ip = a.source_ip END), 0)
	      FROM activity a
	      WHERE a.status = 429 AND a.retry_after_ms > 0 AND a.ts >= ?`
	args := []any{sinceMS}
	if key != "" {
		q += " AND a.key = ?"
		args = append(args, key)
	}
	q += " ORDER BY a.ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RetryPromise
	for rows.Next() {
		var p RetryPromise
		if err := rows.Scan(&p.ID, &p.TS, &p.Key, &p.SourceIP, &p.Served, &p.Reason,
			&p.RetryAfterMS, &p.ReturnedMS); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueueWait is the measured cost of arriving at a served model: how long
// requests that HAD to wait actually waited before getting a slot.
type QueueWait struct {
	Served  string
	MeanMS  int64 // mean queued_ms across the samples
	MaxMS   int64
	Samples int64 // how many requests queued at all — a mean of 1 is not a trend
}

// QueueWaitByModel returns the measured queue wait per served model since
// sinceMS, over requests that were ADMITTED after queueing.
//
// Two exclusions carry the meaning. Requests that never queued (queued_ms = 0)
// are left out: averaging in every instant admission drives the mean toward
// zero during quiet periods and makes the scheduler's estimate look wildly
// pessimistic, when the honest question is "when callers had to wait, how long
// was it". And rejections are left out because their queued_ms measures how long
// someone waited before being turned AWAY, which is a different quantity from
// how long it takes to get in — mixing them would let a maxWait timeout masquerade
// as a service time.
func (s *Store) QueueWaitByModel(sinceMS int64) ([]QueueWait, error) {
	rows, err := s.db.Query(
		`SELECT served, AVG(queued_ms), MAX(queued_ms), COUNT(*)
		   FROM activity
		  WHERE ts >= ? AND queued_ms > 0 AND status < 400
		  GROUP BY served`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QueueWait
	for rows.Next() {
		var q QueueWait
		var mean float64
		if err := rows.Scan(&q.Served, &mean, &q.MaxMS, &q.Samples); err != nil {
			return nil, err
		}
		q.MeanMS = int64(mean)
		out = append(out, q)
	}
	return out, rows.Err()
}

// ServiceStat is the distribution of SERVICE time — how long a request actually
// occupied a slot — for one served model, optionally narrowed to one caller.
//
// Service, not dwell: `dwell_ms` includes queueing and cold-load, both of which
// are consequences of contention rather than properties of the work. Feeding
// them back into a wait estimate would be circular — the queue's own delay would
// inflate the prediction of the queue's delay.
type ServiceStat struct {
	Served  string
	Key     string // "" when this is the model-level aggregate
	N       int64
	MeanMS  float64
	StdMS   float64
	MaxMS   int64
	TotalMS int64 // summed service time — the numerator of a utilization figure
}

// CV is the coefficient of variation (stddev/mean), the shape parameter that
// decides how badly a mean alone describes a queue. A CV near 0 means every
// request costs the same and position×mean is a fair estimate; a CV above 1
// means the mean is dominated by a tail, and queueing behind one of those costs
// far more than the average suggests.
func (s ServiceStat) CV() float64 {
	if s.MeanMS <= 0 {
		return 0
	}
	return s.StdMS / s.MeanMS
}

// ServiceStats returns per-model (byKey=false) or per-model-per-caller
// (byKey=true) service-time distributions since sinceMS.
//
// Only requests that were actually served count. A 429 never occupied a slot,
// and a failed request's duration describes the failure, not the work.
func (s *Store) ServiceStats(sinceMS int64, byKey bool) ([]ServiceStat, error) {
	group, keyCol := "served", "''"
	if byKey {
		group, keyCol = "served, key", "key"
	}
	// SQLite has no STDDEV; E[x²]−E[x]² is computed inline. MAX(...,0) guards the
	// floating-point case where the two terms cancel to a tiny negative.
	rows, err := s.db.Query(`
		SELECT served, `+keyCol+`, COUNT(*), AVG(svc),
		       SQRT(MAX(AVG(svc*svc) - AVG(svc)*AVG(svc), 0)), MAX(svc), SUM(svc)
		  FROM (SELECT served, key, (dwell_ms - queued_ms - load_ms) AS svc
		          FROM activity
		         WHERE ts >= ? AND status < 400 AND (dwell_ms - queued_ms - load_ms) > 0)
		 GROUP BY `+group, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ServiceStat
	for rows.Next() {
		var st ServiceStat
		if err := rows.Scan(&st.Served, &st.Key, &st.N, &st.MeanMS, &st.StdMS, &st.MaxMS, &st.TotalMS); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ModelsSeenSince returns the served names with any activity at or after
// sinceMS. The row set for a utilization view: what the box has actually been
// asked for lately, not the whole declared catalog (a model nobody called is not
// "0% utilized", it is absent).
func (s *Store) ModelsSeenSince(sinceMS int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT served FROM activity WHERE ts >= ? AND served <> ''`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
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
	// CachedTokens is the part of PromptTokens the backend served from its
	// prompt cache instead of reprocessing — the whole point of caching, and
	// invisible until it is summed somewhere.
	CachedTokens int64
	// CacheReports counts requests that reported ANY cache hit.
	//
	// It exists because a zero in CachedTokens has two incompatible meanings: a
	// backend that has no prompt cache (or does not report one) and a backend
	// that genuinely missed. Reporting both as "0% hit rate" would invent a
	// cache-performance problem for every embedding model and every remote
	// provider on the box. When this is 0, the honest reading is "nothing has
	// ever reported a hit here" — which still cannot separate "does not report"
	// from "never hits", and must not be shown as a measured 0%.
	CacheReports int64
	// PromptPerSec is the mean backend-reported prompt-processing speed over the
	// requests that reported one, for estimating the time cache hits avoided.
	PromptPerSec float64
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
		        COALESCE(SUM(cost_usd), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END),
		        COALESCE(AVG(NULLIF(prompt_per_sec, 0)), 0)
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
			&r.DwellMS, &r.CostUSD, &r.CachedTokens, &r.CacheReports, &r.PromptPerSec); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeyRollup is aggregated activity for one caller key over a window (P8).
type KeyRollup struct {
	Key      string
	Requests int64
	// LastSeenMS is the most recent request from this key. A key assigned a
	// lane months ago and silent since is a different thing from one hammering
	// the box right now, and a roster that cannot tell them apart makes you
	// chase ghosts.
	LastSeenMS       int64
	PromptTokens     int64
	CompletionTokens int64
	DwellMS          int64
	CostUSD          float64
	// CachedTokens/CacheReports mirror Rollup — a caller's cache hit rate is a
	// property of how it prompts (a stable system prefix reuses cache; a shuffled
	// one never does), so it belongs per caller and not only per model.
	CachedTokens int64
	CacheReports int64
}

// RollupByKey aggregates activity at or after sinceMS, grouped by caller key,
// ordered by cost (then request count) descending. sinceMS <= 0 covers all
// records. An empty key means an unkeyed caller.
func (s *Store) RollupByKey(sinceMS int64) ([]KeyRollup, error) {
	rows, err := s.db.Query(
		`SELECT key,
		        COUNT(*),
		        COALESCE(MAX(ts), 0),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(dwell_ms), 0),
		        COALESCE(SUM(cost_usd), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END)
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
		if err := rows.Scan(&r.Key, &r.Requests, &r.LastSeenMS, &r.PromptTokens,
			&r.CompletionTokens, &r.DwellMS, &r.CostUSD, &r.CachedTokens, &r.CacheReports); err != nil {
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

// SeriesByModelRow is one (served model, time-bucket) aggregate — the other axis
// of the same traffic RollupSeries slices by caller. Answering "who is spending"
// and "on what" needs both, and a chart cannot derive one from the other.
type SeriesByModelRow struct {
	BucketTS int64 // bucket start, unix millis
	Served   string
	Requests int64
	DwellMS  int64
	CostUSD  float64
}

// RollupSeriesByModel aggregates activity into time buckets grouped by served
// model. A non-empty key narrows to one caller — the same chart then answers
// "what does THIS caller spend it on".
func (s *Store) RollupSeriesByModel(sinceMS, bucketMS int64, key string) ([]SeriesByModelRow, error) {
	if bucketMS <= 0 {
		bucketMS = 3600_000
	}
	q := `SELECT (ts / ?) * ? AS bucket, served, COUNT(*),
	             COALESCE(SUM(dwell_ms), 0), COALESCE(SUM(cost_usd), 0)
	        FROM activity WHERE ts >= ?`
	args := []any{bucketMS, bucketMS, sinceMS}
	if key != "" {
		q += " AND key = ?"
		args = append(args, key)
	}
	q += " GROUP BY bucket, served ORDER BY bucket, served"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SeriesByModelRow
	for rows.Next() {
		var r SeriesByModelRow
		if err := rows.Scan(&r.BucketTS, &r.Served, &r.Requests, &r.DwellMS, &r.CostUSD); err != nil {
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

// QuotaCounter is one falloff-counter window's persisted state (P16 durability).
type QuotaCounter struct {
	Backend string
	Label   string
	Used    float64
	AtMS    int64 // unix millis of the last update
}

// SaveQuotaCounter upserts one window's decaying usage level. Called on each
// counter-mode request; the write volume is tiny (free tiers are rate-limited to
// a handful of requests per minute), so per-request persistence is cheap.
func (s *Store) SaveQuotaCounter(backend, label string, used float64, atMS int64) error {
	_, err := s.db.Exec(`
INSERT INTO quota_counter (backend, label, used, at) VALUES (?,?,?,?)
ON CONFLICT(backend, label) DO UPDATE SET used=excluded.used, at=excluded.at`,
		backend, label, used, atMS)
	return err
}

// LoadQuotaCounters returns every persisted falloff-counter window, for seeding
// the ledger on startup so a daily budget survives a restart.
func (s *Store) LoadQuotaCounters() ([]QuotaCounter, error) {
	rows, err := s.db.Query(`SELECT backend, label, used, at FROM quota_counter`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []QuotaCounter
	for rows.Next() {
		var c QuotaCounter
		if err := rows.Scan(&c.Backend, &c.Label, &c.Used, &c.AtMS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ModelPause is one persisted "this is out of service" order. Target is a
// PROCESS key — a served model name, or "extension:<name>".
type ModelPause struct {
	Target     string
	Reason     string
	AtMS       int64
	ResumeAtMS int64 // 0 = indefinite
}

// SavePause upserts a pause. Written once per operator action, so the volume is
// negligible.
func (s *Store) SavePause(target, reason string, atMS, resumeAtMS int64) error {
	_, err := s.db.Exec(`
INSERT INTO model_pause (target, reason, at, resume_at) VALUES (?,?,?,?)
ON CONFLICT(target) DO UPDATE SET reason=excluded.reason, at=excluded.at, resume_at=excluded.resume_at`,
		target, reason, atMS, resumeAtMS)
	return err
}

// DeletePause clears a pause (unpause, or its resume time passing).
func (s *Store) DeletePause(target string) error {
	_, err := s.db.Exec(`DELETE FROM model_pause WHERE target = ?`, target)
	return err
}

// LoadPauses returns every persisted pause, for restoring them at boot BEFORE
// preload — otherwise a paused pinned model warms itself back up on restart.
// Expiry is the caller's job: the manager owns the clock and drops rows whose
// resume time passed while corrallm was down.
func (s *Store) LoadPauses() ([]ModelPause, error) {
	rows, err := s.db.Query(`SELECT target, reason, at, resume_at FROM model_pause`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelPause
	for rows.Next() {
		var p ModelPause
		if err := rows.Scan(&p.Target, &p.Reason, &p.AtMS, &p.ResumeAtMS); err != nil {
			return nil, err
		}
		out = append(out, p)
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
	ID         int64  `json:"id"`
	RunID      string `json:"runId"`
	Model      string `json:"model"`
	At         int64  `json:"at"`
	Probe      string `json:"probe"`
	Class      string `json:"class"`
	Capability string `json:"capability"`
	RunMode    string `json:"runMode"`
	Toolset    string `json:"toolset"`
	ToolFormat string `json:"toolFormat"`
	// Repeat is which re-run of this arm the row is (0-based).
	Repeat           int    `json:"repeat"`
	Stages           int    `json:"stages"`
	StagesPassed     int    `json:"stagesPassed"`
	ChecksPassed     int    `json:"checksPassed"`
	ChecksTotal      int    `json:"checksTotal"`
	Pass             bool   `json:"pass"`
	WallMS           int64  `json:"wallMs"`
	NewPromptTokens  int    `json:"newPromptTokens"`
	CompletionTokens int    `json:"completionTokens"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skipReason"`
	Note             string `json:"note"`
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
  (run_id, model, at, probe, class, capability, run_mode, toolset, tool_format,
   repeat, stages, stages_passed, checks_passed, checks_total, pass, wall_ms,
   new_prompt_tokens, completion_tokens, skipped, skip_reason, note)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, model, probe, run_mode, toolset, tool_format, repeat) DO UPDATE SET
  at=excluded.at, class=excluded.class, capability=excluded.capability,
  stages=excluded.stages, stages_passed=excluded.stages_passed,
  checks_passed=excluded.checks_passed, checks_total=excluded.checks_total,
  pass=excluded.pass, wall_ms=excluded.wall_ms,
  new_prompt_tokens=excluded.new_prompt_tokens,
  completion_tokens=excluded.completion_tokens, skipped=excluded.skipped,
  skip_reason=excluded.skip_reason, note=excluded.note`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.RunID, r.Model, r.At, r.Probe, r.Class,
			r.Capability, r.RunMode, r.Toolset, r.ToolFormat, r.Repeat, r.Stages, r.StagesPassed,
			r.ChecksPassed, r.ChecksTotal, boolInt(r.Pass), r.WallMS,
			r.NewPromptTokens, r.CompletionTokens, boolInt(r.Skipped),
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

// benchProbeResultCols is the column list every bench_probe_results query
// selects. Shared because scanBenchProbeResults reads them POSITIONALLY: a query
// that listed them in its own order would not fail, it would silently populate
// the wrong fields.
const benchProbeResultCols = `id, run_id, model, at, probe, class, capability, run_mode, toolset,
       tool_format, repeat, stages, stages_passed, checks_passed, checks_total, pass, wall_ms,
       new_prompt_tokens, completion_tokens, skipped, skip_reason, note`

// BenchProbeResultsFor returns a model's probe rows. With runID set it scopes to
// that run; empty runID returns the model's most recent run only — the console
// asks "how did the last bench go", and mixing runs would average away the
// regression it is there to show.
func (s *Store) BenchProbeResultsFor(ctx context.Context, model, runID string) ([]BenchProbeResult, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if runID != "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+benchProbeResultCols+`
FROM bench_probe_results WHERE model = ? AND run_id = ?
ORDER BY capability, class, probe, run_mode, toolset, tool_format, repeat`, model, runID)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+benchProbeResultCols+`
FROM bench_probe_results
WHERE model = ? AND run_id = (SELECT run_id FROM bench_probe_results WHERE model = ? ORDER BY at DESC LIMIT 1)
ORDER BY capability, class, probe, run_mode, toolset, tool_format, repeat`, model, model)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchProbeResults(rows)
}

// BenchProbeStage is one stage of one probe arm: what it was asked, what it
// cost, and whether it passed.
type BenchProbeStage struct {
	RunID               string  `json:"runId"`
	Model               string  `json:"model"`
	Probe               string  `json:"probe"`
	RunMode             string  `json:"runMode"`
	Toolset             string  `json:"toolset"`
	ToolFormat          string  `json:"toolFormat"`
	Stage               int     `json:"stage"`
	Prompt              string  `json:"prompt"`
	Pass                bool    `json:"pass"`
	LimitBreached       bool    `json:"limitBreached"`
	Note                string  `json:"note"`
	Turns               int     `json:"turns"`
	ToolCalls           int     `json:"toolCalls"`
	NewPromptTokens     int     `json:"newPromptTokens"`
	CompletionTokens    int     `json:"completionTokens"`
	InvalidArgRetries   int     `json:"invalidArgRetries"`
	JSONErrors          int     `json:"jsonErrors"`
	RepeatedCalls       int     `json:"repeatedCalls"`
	BaitCalls           int     `json:"baitCalls"`
	BrokenIntermediates int     `json:"brokenIntermediates"`
	Compactions         int     `json:"compactions"`
	TokPerSec           float64 `json:"tokPerSec"`
	WallMS              int64   `json:"wallMs"`
	// QueuedMS is time the stage waited on corrallm rather than the model:
	// 429 backoff plus the admission and cold-load waits inside accepted
	// requests. ExecMS is WallMS minus that.
	QueuedMS int64 `json:"queuedMs"`
	ExecMS   int64 `json:"execMs"`
}

// BenchProbeCheck is one assertion's verdict within a stage.
type BenchProbeCheck struct {
	RunID      string `json:"runId"`
	Model      string `json:"model"`
	Probe      string `json:"probe"`
	RunMode    string `json:"runMode"`
	Toolset    string `json:"toolset"`
	ToolFormat string `json:"toolFormat"`
	Stage      int    `json:"stage"`
	Idx        int    `json:"idx"`
	Kind       string `json:"kind"`
	Desc       string `json:"desc"`
	Pass       bool   `json:"pass"`
	Detail     string `json:"detail"`
}

// BenchRun records where a run's on-disk artifacts live.
type BenchRun struct {
	RunID  string `json:"runId"`
	OutDir string `json:"outDir"`
	Host   string `json:"host"`
	At     int64  `json:"at"`
}

// SaveBenchRun upserts a run's artifact location.
func (s *Store) SaveBenchRun(ctx context.Context, r BenchRun) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO bench_runs (run_id, out_dir, host, at) VALUES (?,?,?,?)
ON CONFLICT(run_id) DO UPDATE SET
  out_dir=excluded.out_dir, host=excluded.host, at=excluded.at`,
		r.RunID, r.OutDir, r.Host, r.At)
	return err
}

// BenchRunFor returns a run's artifact location, or ok=false when unknown.
func (s *Store) BenchRunFor(ctx context.Context, runID string) (BenchRun, bool, error) {
	var r BenchRun
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, out_dir, host, at FROM bench_runs WHERE run_id = ?`, runID).
		Scan(&r.RunID, &r.OutDir, &r.Host, &r.At)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

// BenchRunSummary is one run as a whole: when it happened and how much of it
// passed, without the caller fetching every row to find out.
type BenchRunSummary struct {
	BenchRun
	Models   int   `json:"models"`
	Probes   int   `json:"probes"`
	Rows     int   `json:"rows"`
	Passed   int   `json:"passed"`
	Skipped  int   `json:"skipped"`
	WallMSum int64 `json:"wallMsSum"`
}

// BenchRuns lists runs newest first.
//
// Runs are enumerated from bench_probe_results rather than bench_runs, because
// the two do not agree: bench_runs records where a run's ARTIFACTS live and is
// only written when a run has an out/ directory, so a published run whose
// artifacts were pruned — or that never wrote any — is absent from it while its
// results sit in the database. Listing from the results means the index shows
// every run there is evidence for, and the artifact row is a left join that
// simply tells you whether transcripts are still on disk.
func (s *Store) BenchRuns(ctx context.Context, limit int) ([]BenchRunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT r.run_id,
       COALESCE(b.out_dir, ''), COALESCE(b.host, ''),
       COALESCE(b.at, MAX(r.at)) AS at,
       COUNT(DISTINCT r.model), COUNT(DISTINCT r.probe), COUNT(*),
       SUM(CASE WHEN r.pass = 1 THEN 1 ELSE 0 END),
       SUM(CASE WHEN r.skipped = 1 THEN 1 ELSE 0 END),
       COALESCE(SUM(r.wall_ms), 0)
FROM bench_probe_results r
LEFT JOIN bench_runs b ON b.run_id = r.run_id
GROUP BY r.run_id
ORDER BY at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRunSummary{}
	for rows.Next() {
		var v BenchRunSummary
		if err := rows.Scan(&v.RunID, &v.OutDir, &v.Host, &v.At,
			&v.Models, &v.Probes, &v.Rows, &v.Passed, &v.Skipped, &v.WallMSum); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// BenchProbeResultsForRun returns every model's probe rows for one run.
//
// The model-scoped query cannot answer "what did this run do": it needs a model
// up front, so a caller would have to know the participants before it could ask
// who they were.
func (s *Store) BenchProbeResultsForRun(ctx context.Context, runID string) ([]BenchProbeResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+benchProbeResultCols+`
FROM bench_probe_results WHERE run_id = ?
ORDER BY model, capability, class, probe, run_mode, toolset, tool_format, repeat`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchProbeResults(rows)
}

// BenchProbeHistory returns one probe's rows across every model and run.
//
// This is the suite-and-test view: a probe that every model fails is evidence
// about the PROBE, not about the models, and neither the per-model nor the
// per-run query can show that — both slice the other way.
func (s *Store) BenchProbeHistory(ctx context.Context, probe string, limit int) ([]BenchProbeResult, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+benchProbeResultCols+`
FROM bench_probe_results WHERE probe = ?
ORDER BY at DESC, model, run_mode, toolset, tool_format, repeat
LIMIT ?`, probe, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchProbeResults(rows)
}

// SaveBenchProbeStages upserts per-stage detail in one transaction.
func (s *Store) SaveBenchProbeStages(ctx context.Context, rows []BenchProbeStage) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO bench_probe_stages
  (run_id, model, probe, run_mode, toolset, tool_format, stage, prompt, pass,
   limit_breached, note, turns, tool_calls, new_prompt_tokens, completion_tokens,
   invalid_arg_retries, json_errors, repeated_calls, bait_calls,
   broken_intermediates, compactions, tok_per_sec, wall_ms, queued_ms, exec_ms)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, model, probe, run_mode, toolset, tool_format, stage) DO UPDATE SET
  prompt=excluded.prompt, pass=excluded.pass, limit_breached=excluded.limit_breached,
  note=excluded.note, turns=excluded.turns, tool_calls=excluded.tool_calls,
  new_prompt_tokens=excluded.new_prompt_tokens,
  completion_tokens=excluded.completion_tokens,
  invalid_arg_retries=excluded.invalid_arg_retries, json_errors=excluded.json_errors,
  repeated_calls=excluded.repeated_calls, bait_calls=excluded.bait_calls,
  broken_intermediates=excluded.broken_intermediates,
  compactions=excluded.compactions, tok_per_sec=excluded.tok_per_sec,
  wall_ms=excluded.wall_ms, queued_ms=excluded.queued_ms, exec_ms=excluded.exec_ms`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.RunID, r.Model, r.Probe, r.RunMode,
			r.Toolset, r.ToolFormat, r.Stage, r.Prompt, boolInt(r.Pass),
			boolInt(r.LimitBreached), r.Note, r.Turns, r.ToolCalls, r.NewPromptTokens,
			r.CompletionTokens, r.InvalidArgRetries, r.JSONErrors, r.RepeatedCalls,
			r.BaitCalls, r.BrokenIntermediates, r.Compactions, r.TokPerSec,
			r.WallMS, r.QueuedMS, r.ExecMS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveBenchProbeChecks upserts per-check verdicts in one transaction.
func (s *Store) SaveBenchProbeChecks(ctx context.Context, rows []BenchProbeCheck) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO bench_probe_checks
  (run_id, model, probe, run_mode, toolset, tool_format, stage, idx, kind, descr, pass, detail)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, model, probe, run_mode, toolset, tool_format, stage, idx) DO UPDATE SET
  kind=excluded.kind, descr=excluded.descr, pass=excluded.pass, detail=excluded.detail`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.RunID, r.Model, r.Probe, r.RunMode,
			r.Toolset, r.ToolFormat, r.Stage, r.Idx, r.Kind, r.Desc,
			boolInt(r.Pass), r.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BenchProbeStagesFor returns one probe arm's stages, in order.
func (s *Store) BenchProbeStagesFor(ctx context.Context, runID, model, probe string) ([]BenchProbeStage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, model, probe, run_mode, toolset, tool_format, stage, prompt, pass,
       limit_breached, note, turns, tool_calls, new_prompt_tokens, completion_tokens,
       invalid_arg_retries, json_errors, repeated_calls, bait_calls,
       broken_intermediates, compactions, tok_per_sec, wall_ms, queued_ms, exec_ms
FROM bench_probe_stages WHERE run_id = ? AND model = ? AND probe = ?
ORDER BY toolset, tool_format, run_mode, stage`, runID, model, probe)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchProbeStage
	for rows.Next() {
		var r BenchProbeStage
		var pass, breached int
		if err := rows.Scan(&r.RunID, &r.Model, &r.Probe, &r.RunMode, &r.Toolset,
			&r.ToolFormat, &r.Stage, &r.Prompt, &pass, &breached, &r.Note, &r.Turns,
			&r.ToolCalls, &r.NewPromptTokens, &r.CompletionTokens, &r.InvalidArgRetries,
			&r.JSONErrors, &r.RepeatedCalls, &r.BaitCalls, &r.BrokenIntermediates,
			&r.Compactions, &r.TokPerSec, &r.WallMS, &r.QueuedMS, &r.ExecMS); err != nil {
			return nil, err
		}
		r.Pass, r.LimitBreached = pass != 0, breached != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// BenchProbeChecksFor returns one probe arm's checks, in stage then declared order.
func (s *Store) BenchProbeChecksFor(ctx context.Context, runID, model, probe string) ([]BenchProbeCheck, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, model, probe, run_mode, toolset, tool_format, stage, idx, kind, descr, pass, detail
FROM bench_probe_checks WHERE run_id = ? AND model = ? AND probe = ?
ORDER BY toolset, tool_format, run_mode, stage, idx`, runID, model, probe)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchProbeCheck
	for rows.Next() {
		var r BenchProbeCheck
		var pass int
		if err := rows.Scan(&r.RunID, &r.Model, &r.Probe, &r.RunMode, &r.Toolset,
			&r.ToolFormat, &r.Stage, &r.Idx, &r.Kind, &r.Desc, &pass, &r.Detail); err != nil {
			return nil, err
		}
		r.Pass = pass != 0
		out = append(out, r)
	}
	return out, rows.Err()
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
       b.toolset, b.tool_format, b.repeat, b.stages, b.stages_passed, b.checks_passed,
       b.checks_total, b.pass, b.wall_ms, b.new_prompt_tokens, b.completion_tokens,
       b.skipped, b.skip_reason, b.note
FROM bench_probe_results b
JOIN (SELECT model, MAX(at) AS at FROM bench_probe_results GROUP BY model) m
  ON m.model = b.model AND m.at = b.at
ORDER BY b.capability, b.model, b.probe, b.run_mode, b.toolset, b.tool_format, b.repeat`)
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
			&r.Capability, &r.RunMode, &r.Toolset, &r.ToolFormat, &r.Repeat, &r.Stages,
			&r.StagesPassed, &r.ChecksPassed, &r.ChecksTotal, &pass, &r.WallMS,
			&r.NewPromptTokens, &r.CompletionTokens, &skipped, &r.SkipReason,
			&r.Note); err != nil {
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
