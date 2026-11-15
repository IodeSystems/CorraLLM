package store

import (
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Backend restart requests (`corrallm backend restart`).
//
// The CLI runs on the box with no admin token (it may be disabled), like
// `spend` and `config load`: it writes a request row here and sends the
// daemon a reload (SIGHUP to the main pid). The daemon re-reads the config,
// claims the pending rows, runs each restart and writes its progress back to
// the row, which the CLI polls. So the restart runs the config the reload just
// read, and an older daemon that knows nothing of this table leaves the row
// pending, which the CLI reports instead of waiting forever.
const backendRestartSchema = `
CREATE TABLE IF NOT EXISTS backend_restart (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    model        TEXT    NOT NULL,
    reason       TEXT    NOT NULL DEFAULT '',
    drain_ms     INTEGER NOT NULL DEFAULT 0,  -- 0 = the daemon's default
    requested_at INTEGER NOT NULL,            -- unix millis
    updated_at   INTEGER NOT NULL,            -- unix millis
    state        TEXT    NOT NULL DEFAULT 'pending', -- pending | claimed | draining | stopping | loading | done | failed
    detail       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_backend_restart_state ON backend_restart(state);
`

// Restart request states. Every state but pending, done and failed is a
// stage of a restart the daemon is running.
const (
	RestartPending = "pending"
	RestartClaimed = "claimed"
	RestartDone    = "done"
	RestartFailed  = "failed"
)

// BackendRestart is one restart request.
type BackendRestart struct {
	ID          int64
	Model       string
	Reason      string
	Drain       time.Duration
	RequestedAt time.Time
	UpdatedAt   time.Time
	State       string
	Detail      string
}

// Finished reports that the request reached done or failed.
func (r BackendRestart) Finished() bool {
	return r.State == RestartDone || r.State == RestartFailed
}

// AddBackendRestart records a pending restart request and returns its id.
func (s *Store) AddBackendRestart(model, reason string, drain time.Duration, at time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO backend_restart (model, reason, drain_ms, requested_at, updated_at) VALUES (?,?,?,?,?)`,
		model, reason, drain.Milliseconds(), at.UnixMilli(), at.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ClaimBackendRestarts moves every pending request made after notBefore to
// claimed and returns them, oldest first. Older pending requests are failed
// as expired: their CLI has given up, and a restart nobody waits for is a
// surprise.
func (s *Store) ClaimBackendRestarts(notBefore, now time.Time) ([]BackendRestart, error) {
	if _, err := s.db.Exec(`UPDATE backend_restart SET state = ?, detail = ?, updated_at = ? WHERE state = ? AND requested_at < ?`,
		RestartFailed, "expired: no daemon claimed it in time", now.UnixMilli(), RestartPending, notBefore.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`UPDATE backend_restart SET state = ?, updated_at = ? WHERE state = ?
		RETURNING id, model, reason, drain_ms, requested_at, updated_at, state, detail`,
		RestartClaimed, now.UnixMilli(), RestartPending)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BackendRestart
	for rows.Next() {
		r, err := scanBackendRestart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING gives no order; the oldest request runs first.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UpdateBackendRestart sets a request's state and detail.
func (s *Store) UpdateBackendRestart(id int64, state, detail string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE backend_restart SET state = ?, detail = ?, updated_at = ? WHERE id = ?`,
		state, detail, at.UnixMilli(), id)
	return err
}

// AbandonBackendRestart fails a request only if it is still pending, and
// reports whether it did. The CLI's way out when no daemon claimed it.
func (s *Store) AbandonBackendRestart(id int64, detail string, at time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE backend_restart SET state = ?, detail = ?, updated_at = ? WHERE id = ? AND state = ?`,
		RestartFailed, detail, at.UnixMilli(), id, RestartPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GetBackendRestart reads one request.
func (s *Store) GetBackendRestart(id int64) (BackendRestart, error) {
	row := s.db.QueryRow(`SELECT id, model, reason, drain_ms, requested_at, updated_at, state, detail FROM backend_restart WHERE id = ?`, id)
	r, err := scanBackendRestart(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errors.New("no such restart request")
	}
	return r, err
}

func scanBackendRestart(sc interface{ Scan(...any) error }) (BackendRestart, error) {
	var r BackendRestart
	var drainMS, reqMS, updMS int64
	err := sc.Scan(&r.ID, &r.Model, &r.Reason, &drainMS, &reqMS, &updMS, &r.State, &r.Detail)
	r.Drain = time.Duration(drainMS) * time.Millisecond
	r.RequestedAt, r.UpdatedAt = time.UnixMilli(reqMS), time.UnixMilli(updMS)
	return r, err
}
