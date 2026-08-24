package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// oldActivityDDL is the activity table as it shipped BEFORE tickets: everything
// through retry_after_ms, and no ticket column.
//
// It exists because every other test in this package builds its database fresh
// from the current schema, where CREATE TABLE brings every column with it. That
// is the one shape an upgrade never has, and it is why an index added to the
// schema block next to a column added by a migration passed the whole suite and
// then crash-looped the production daemon:
//
//	apply schema: SQL logic error: no such column: ticket
//
// The schema block runs BEFORE the migrations, and on an existing database its
// CREATE TABLE is a no-op — so anything in it that touches a migrated column is
// referring to something that does not exist yet.
const oldActivityDDL = `
CREATE TABLE activity (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    served            TEXT    NOT NULL,
    placement         TEXT    NOT NULL DEFAULT '',
    key               TEXT    NOT NULL DEFAULT '',
    source_ip         TEXT    NOT NULL DEFAULT '',
    path              TEXT    NOT NULL,
    status            INTEGER NOT NULL,
    dwell_ms          INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL    NOT NULL DEFAULT 0,
    queued_ms         INTEGER NOT NULL DEFAULT 0,
    load_ms           INTEGER NOT NULL DEFAULT 0,
    audio_bytes       INTEGER NOT NULL DEFAULT 0,
    error             TEXT    NOT NULL DEFAULT '',
    ttfb_ms           INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    prompt_per_sec    REAL    NOT NULL DEFAULT 0,
    predicted_per_sec REAL    NOT NULL DEFAULT 0,
    finish_reason     TEXT    NOT NULL DEFAULT '',
    req_body          TEXT    NOT NULL DEFAULT '',
    resp_body         TEXT    NOT NULL DEFAULT '',
    retry_after_ms    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_activity_ts ON activity(ts);
CREATE INDEX idx_activity_key_ts ON activity(key, ts);
`

// Opening a database written by an EARLIER version must succeed, keep its rows,
// and end up with the current shape.
func TestOpenUpgradesAPreTicketDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldActivityDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO activity (ts, served, path, status) VALUES (1, 'm', '/v1/chat/completions', 200)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening a pre-ticket database failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The history survived.
	acts, err := st.RecentActivity(10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Served != "m" {
		t.Fatalf("existing rows did not survive the upgrade: %+v", acts)
	}

	// And the new machinery is actually there, not merely un-crashed.
	if err := st.InsertActivity(Activity{
		TS: 2, Served: "m", Path: "/v1/chat/completions", Status: 429, Ticket: "tkt_x",
	}); err != nil {
		t.Fatalf("insert with a ticket: %v", err)
	}
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_activity_ticket'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the ticket index was not created on an upgraded database")
	}
}
