package store

import "fmt"

// inflightSchema is the record of requests that are being served right now.
//
// A SEPARATE TABLE, not a status on activity, for two reasons. activity has
// ~25 readers (rollups, rates, usage and cost series, the Keys and Bench pages)
// and every one of them would have to learn to skip a half-written row, or
// count it as an error or a zero-token success; a miss fails silently. And the
// activity row is written once, at completion, with its final id, so the
// payload and conversation side tables are written once too.
//
// What a restart needs is only "this request existed and was cut", so the
// marker holds just the identity of the request. Payloads stay out: the request
// body is written with the final row, as before, and not twice.
const inflightSchema = `CREATE TABLE IF NOT EXISTS activity_inflight (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        INTEGER NOT NULL,           -- unix millis, request start
    served    TEXT    NOT NULL,           -- the candidate it was handed to
    requested TEXT    NOT NULL DEFAULT '',
    placement TEXT    NOT NULL DEFAULT '',
    key       TEXT    NOT NULL DEFAULT '',
    source_ip TEXT    NOT NULL DEFAULT '',
    path      TEXT    NOT NULL,
    ticket    TEXT    NOT NULL DEFAULT '',
    queued_ms INTEGER NOT NULL DEFAULT 0
)`

// CutRestartStatus and CutRestartError mark a request whose process died with
// it in flight. 502, not 499 (the caller hung up) and not 0: a rollup counts
// it as a failed request without an answer, and the "cut:" prefix is what the
// activity page and a query can tell it apart by.
const (
	CutRestartStatus = 502
	CutRestartError  = "cut: corrallm restarted"
)

// SetOwner names the process writing markers from here on: its pid and an
// opaque start stamp that a reused pid cannot share (host.ProcIdentity's
// Start). Open defaults to this pid with no stamp.
func (s *Store) SetOwner(pid int, start string) {
	s.ownerPID, s.ownerStart = int64(pid), start
}

// BeginInflight records that a request is about to be served and returns the
// marker's id, to hand back as Activity.InflightID on the completing row.
func (s *Store) BeginInflight(a Activity) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO activity_inflight (ts, served, requested, placement, key, source_ip, path, ticket, queued_ms, owner_pid, owner_start)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Requested, a.Placement, a.Key, a.SourceIP, a.Path, a.Ticket, a.QueuedMS, s.ownerPID, s.ownerStart)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndInflight drops a marker whose request left no row of its own (a spill
// that is not logged, a candidate that failed before the backend was reached).
func (s *Store) EndInflight(id int64) error {
	_, err := s.db.Exec(`DELETE FROM activity_inflight WHERE id = ?`, id)
	return err
}

// SweepInflight turns every marker whose writer is gone into a cut activity
// row and returns how many.
//
// SCOPED BY OWNER, because a blue-green handoff starts this process while its
// predecessor is still serving from the same database: a sweep of every marker
// would record the predecessor's live streams as cut, and those requests would
// then end with a second, real row. alive reports whether the process that
// wrote a marker still runs (pid AND start stamp, so a reused pid is not
// mistaken for the writer). This process's own markers are never swept, and a
// marker with no owner (written before the columns existed) always is. A nil
// alive treats every other owner as dead.
//
// Run at start, and again when a predecessor exits: whatever it left was cut.
func (s *Store) SweepInflight(alive func(pid int64, start string) bool) (int, error) {
	type owner struct {
		pid   int64
		start string
	}
	rows, err := s.db.Query(`SELECT DISTINCT owner_pid, owner_start FROM activity_inflight`)
	if err != nil {
		return 0, fmt.Errorf("sweep in-flight: %w", err)
	}
	var dead []owner
	for rows.Next() {
		var o owner
		if err := rows.Scan(&o.pid, &o.start); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if o.pid == s.ownerPID && o.start == s.ownerStart {
			continue
		}
		if o.pid != 0 && alive != nil && alive(o.pid, o.start) {
			continue
		}
		dead = append(dead, o)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(dead) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	total := 0
	for _, o := range dead {
		res, err := tx.Exec(
			`INSERT INTO activity (ts, served, requested, placement, key, source_ip, path, status, error, ticket, queued_ms)
			 SELECT ts, served, requested, placement, key, source_ip, path, ?, ?, ticket, queued_ms
			   FROM activity_inflight WHERE owner_pid = ? AND owner_start = ? ORDER BY id`,
			CutRestartStatus, CutRestartError, o.pid, o.start)
		if err != nil {
			return 0, fmt.Errorf("sweep in-flight: %w", err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if _, err := tx.Exec(`DELETE FROM activity_inflight WHERE owner_pid = ? AND owner_start = ?`, o.pid, o.start); err != nil {
			return 0, err
		}
	}
	return total, tx.Commit()
}

// InflightCount is how many markers stand now (tests, diagnostics).
func (s *Store) InflightCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM activity_inflight`).Scan(&n)
	return n, err
}
