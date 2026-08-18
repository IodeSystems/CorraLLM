package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Tool build history.
//
// The Builder keeps the running job and the last finished one in memory, which
// is right for a live modal and useless the moment the daemon restarts — and
// this daemon restarts on every deploy. "Did that build work?" is asked hours
// later, usually after several restarts, and the in-memory answer is gone by
// then. So a build is also a historical fact, like a bench run and unlike a
// capability verdict.
//
// The log is stored WITH the row rather than in a side table. It is the reason
// anybody opens an old build, it is bounded by the ring the Builder already
// keeps, and splitting it would buy a join for no benefit.
const toolBuildSchema = `
CREATE TABLE IF NOT EXISTS tool_build (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tool        TEXT    NOT NULL,
    host        TEXT    NOT NULL,
    status      TEXT    NOT NULL,            -- running | ok | failed | interrupted
    started_at  INTEGER NOT NULL,            -- unix millis
    finished_at INTEGER NOT NULL DEFAULT 0,  -- 0 while running
    skipped     INTEGER NOT NULL DEFAULT 0,  -- stamp matched; nothing compiled
    version     TEXT    NOT NULL DEFAULT '',
    stamp       TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tool_build_started ON tool_build(started_at);
CREATE INDEX IF NOT EXISTS idx_tool_build_target ON tool_build(tool, host, started_at);
`

// ToolBuild is one persisted build.
type ToolBuild struct {
	ID         int64
	Tool       string
	Host       string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	Skipped    bool
	Version    string
	Stamp      string
	Error      string
	Log        string
}

// maxBuildLogBytes caps what is written per build.
//
// A CUDA build emits megabytes, nearly all of it nvcc warnings about somebody
// else's headers, and the tail is what diagnoses a failure. Storing the whole
// thing would put tens of megabytes of noise in a database whose whole point is
// being small.
const maxBuildLogBytes = 256 * 1024

// StartToolBuild records a build as it begins and returns its row id.
//
// Written at START, not only at completion, so a build killed by a restart
// leaves evidence. Without it, deploying mid-build makes twenty minutes of work
// vanish with no trace that it ever ran.
func (s *Store) StartToolBuild(ctx context.Context, tool, host string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_build (tool, host, status, started_at) VALUES (?, ?, 'running', ?)`,
		tool, host, at.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishToolBuild records the outcome.
func (s *Store) FinishToolBuild(ctx context.Context, id int64, b ToolBuild) error {
	log := b.Log
	if len(log) > maxBuildLogBytes {
		// Keep the TAIL: cmake's error is within a few lines of the end, and a
		// truncated head loses exactly the part worth reading.
		log = "… earlier output trimmed …\n" + log[len(log)-maxBuildLogBytes:]
	}
	skipped := 0
	if b.Skipped {
		skipped = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_build SET status=?, finished_at=?, skipped=?, version=?, stamp=?, error=?, log=? WHERE id=?`,
		b.Status, b.FinishedAt.UnixMilli(), skipped, b.Version, b.Stamp, b.Error, log, id)
	return err
}

// InterruptStaleToolBuilds marks builds still flagged running as interrupted.
//
// Called at startup. A build is a child of this process, so a restart kills it
// — a row left saying "running" would be a lie that never resolves, and the UI
// would show a spinner for a build that died days ago.
func (s *Store) InterruptStaleToolBuilds(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tool_build SET status='interrupted', finished_at=?, error=?
		 WHERE status='running'`,
		time.Now().UnixMilli(),
		"the daemon restarted while this build was running, which kills it")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecentToolBuilds returns the most recent builds, newest first. Optionally
// scoped to one tool/host pair; empty strings mean "any".
func (s *Store) RecentToolBuilds(ctx context.Context, tool, host string, limit int) ([]ToolBuild, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, tool, host, status, started_at, finished_at, skipped, version, stamp, error
	      FROM tool_build`
	var where []string
	var args []any
	if strings.TrimSpace(tool) != "" {
		where = append(where, "tool = ?")
		args = append(args, tool)
	}
	if strings.TrimSpace(host) != "" {
		where = append(where, "host = ?")
		args = append(args, host)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolBuild
	for rows.Next() {
		var b ToolBuild
		var started, finished, skipped int64
		if err := rows.Scan(&b.ID, &b.Tool, &b.Host, &b.Status, &started, &finished,
			&skipped, &b.Version, &b.Stamp, &b.Error); err != nil {
			return nil, err
		}
		b.StartedAt = time.UnixMilli(started)
		if finished > 0 {
			b.FinishedAt = time.UnixMilli(finished)
		}
		b.Skipped = skipped != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// ToolBuildLog returns one build's captured output.
//
// Separate from the listing on purpose: a history table of twenty builds would
// otherwise carry twenty logs, which is megabytes to render a list of dates.
func (s *Store) ToolBuildLog(ctx context.Context, id int64) (string, error) {
	var log string
	err := s.db.QueryRowContext(ctx, `SELECT log FROM tool_build WHERE id = ?`, id).Scan(&log)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return log, err
}

// PruneToolBuilds keeps the newest `keep` rows and deletes the rest.
func (s *Store) PruneToolBuilds(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = 100
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tool_build WHERE id NOT IN (
		    SELECT id FROM tool_build ORDER BY started_at DESC LIMIT ?
		 )`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
