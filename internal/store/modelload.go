package store

import (
	"database/sql"
	"errors"
)

// Model loads, and WHY.
//
// The activity log said a backend loaded — `load_ms` on the row that paid for
// it — and never said what caused the load or what it displaced. Diagnosing the
// 2026-09-05 slowdown therefore took a database session instead of a glance:
// the numbers showed a model being reloaded repeatedly and nothing on any
// screen said which model had taken its place, or who had asked for it.
//
// This is the missing half. One row per load, carrying the two facts the log
// could not answer: who triggered it, and what had to be unloaded to fit it.

// ModelLoad is one backend coming up, and the circumstances.
type ModelLoad struct {
	ID       int64
	TS       int64
	Model    string
	Server   string
	Requester string // the caller whose request triggered it; empty for a preload
	// Evicted names what was unloaded to make room, comma-separated and empty
	// when the model simply fit. This is the field the slowdown needed: a model
	// that keeps loading is only half a story until you know what keeps
	// pushing it out.
	Evicted string
	MS      int64
	OK      bool
	Err     string
}

const modelLoadSchema = `
CREATE TABLE IF NOT EXISTS model_load (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        INTEGER NOT NULL,
    model     TEXT    NOT NULL,
    server    TEXT    NOT NULL DEFAULT '',
    requester TEXT    NOT NULL DEFAULT '',  -- caller key; '' for a preload
    evicted   TEXT    NOT NULL DEFAULT '',  -- comma-separated; '' when it just fit
    ms        INTEGER NOT NULL DEFAULT 0,
    ok        INTEGER NOT NULL DEFAULT 1,
    err       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_model_load_ts ON model_load(ts);
`

// InsertModelLoad records one load.
//
// Never fatal to the load it describes: a spawn that worked must not be
// reported as failed because the note about it could not be written.
func (s *Store) InsertModelLoad(l ModelLoad) error {
	_, err := s.db.Exec(
		`INSERT INTO model_load (ts, model, server, requester, evicted, ms, ok, err)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.TS, l.Model, l.Server, l.Requester, l.Evicted, l.MS, boolToInt(l.OK), l.Err)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ModelLoads returns the loads in a window, newest first. An empty model reads
// every model's.
func (s *Store) ModelLoads(w Window, model string, limit int) ([]ModelLoad, error) {
	if limit <= 0 {
		limit = 50
	}
	where, args := w.where("ts")
	q := `SELECT id, ts, model, server, requester, evicted, ms, ok, err FROM model_load WHERE ` + where
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelLoad
	for rows.Next() {
		var l ModelLoad
		var ok int
		var server, requester, evicted, errStr sql.NullString
		if err := rows.Scan(&l.ID, &l.TS, &l.Model, &server, &requester, &evicted, &l.MS, &ok, &errStr); err != nil {
			return nil, err
		}
		l.Server, l.Requester, l.Evicted, l.Err = server.String, requester.String, evicted.String, errStr.String
		l.OK = ok != 0
		out = append(out, l)
	}
	return out, rows.Err()
}

// EvictionPressure is how often a server has had to unload something to fit
// something else, and the most recent instance.
//
// It exists to answer the question a full memory bar raises and could not
// settle: "96%" and "0%" were drawn the same way, and the number alone cannot
// say whether it is a problem — because usually it is not. A device pool at 96%
// normally means a model is RESIDENT, which is the box working. It becomes a
// problem only when something else wants the space, and that is an event, not a
// level.
//
// Lenny run 9, on the Machines page: "I left it alone, but I did so guessing,
// not knowing."
type EvictionPressure struct {
	Server      string
	Count       int64
	LastMS      int64
	LastEvicted string // what was unloaded
	LastFor     string // and what it made room for
}

// EvictionPressureSince reports, per server, the loads in a window that had to
// evict something first.
func (s *Store) EvictionPressureSince(w Window) ([]EvictionPressure, error) {
	where, args := w.where("ts")
	rows, err := s.db.Query(
		`SELECT server, COUNT(*), MAX(ts) FROM model_load
		  WHERE `+where+` AND evicted <> '' AND server <> ''
		  GROUP BY server ORDER BY 2 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EvictionPressure
	for rows.Next() {
		var e EvictionPressure
		if err := rows.Scan(&e.Server, &e.Count, &e.LastMS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The most recent instance, named. "Three times" is a rate; "unloaded
	// chandra-ocr-2 to fit Qwen3.8-27B" is the thing somebody can act on.
	for i := range out {
		var evicted, model sql.NullString
		err := s.db.QueryRow(
			`SELECT evicted, model FROM model_load
			  WHERE server = ? AND ts = ? AND evicted <> '' LIMIT 1`,
			out[i].Server, out[i].LastMS).Scan(&evicted, &model)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out[i].LastEvicted, out[i].LastFor = evicted.String, model.String
	}
	return out, nil
}
