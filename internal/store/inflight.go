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

// BeginInflight records that a request is about to be served and returns the
// marker's id, to hand back as Activity.InflightID on the completing row.
func (s *Store) BeginInflight(a Activity) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO activity_inflight (ts, served, requested, placement, key, source_ip, path, ticket, queued_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.Served, a.Requested, a.Placement, a.Key, a.SourceIP, a.Path, a.Ticket, a.QueuedMS)
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

// SweepInflight turns every marker still standing into a cut activity row and
// returns how many. Run once at daemon start, before serving: whatever is left
// belonged to a process that is gone. It is NOT safe to run from a second
// process sharing the database while the first still serves.
func (s *Store) SweepInflight() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`INSERT INTO activity (ts, served, requested, placement, key, source_ip, path, status, error, ticket, queued_ms)
		 SELECT ts, served, requested, placement, key, source_ip, path, ?, ?, ticket, queued_ms
		   FROM activity_inflight ORDER BY id`, CutRestartStatus, CutRestartError)
	if err != nil {
		return 0, fmt.Errorf("sweep in-flight: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM activity_inflight`); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// InflightCount is how many markers stand now (tests, diagnostics).
func (s *Store) InflightCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM activity_inflight`).Scan(&n)
	return n, err
}
