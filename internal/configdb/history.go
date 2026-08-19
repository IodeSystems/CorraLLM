package configdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/corrallm/internal/config"
)

// Config history — the thing a file never had.
//
// The tables hold the CURRENT config; this holds what it was before each
// change. It exists because this project needed it twice in one week and did
// not have it: a `tools:` block vanished with nothing to compare against, and a
// dependency pin turned out to have been wrong for months. Both are "what did
// this look like yesterday, and who changed it", which is a question a
// rewritten file cannot answer at all.
//
// A whole snapshot per revision rather than a diff. Config is tens of
// kilobytes and changes a few times a day; storing a snapshot makes restore a
// copy instead of a replay, and a replay that goes wrong reconstructs a config
// nobody ever ran.
const historySchema = `
CREATE TABLE IF NOT EXISTS config_revision (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    ts     INTEGER NOT NULL,           -- unix millis
    note   TEXT    NOT NULL DEFAULT '', -- what caused it, in the caller's words
    yaml   TEXT    NOT NULL            -- the config as it was AFTER this change
);
CREATE INDEX IF NOT EXISTS idx_config_revision_ts ON config_revision(ts);
`

// Revision is one recorded state of the configuration.
type Revision struct {
	ID   int64
	At   time.Time
	Note string
	// Size is the YAML length; the body is fetched separately because a listing
	// of fifty revisions would otherwise be megabytes to render a list of dates.
	Size int
}

// Record snapshots the current config.
//
// Called AFTER a successful save, not before: a revision is a state the system
// actually reached, and recording an intent that then failed validation would
// fill the history with configs that never ran.
func Record(ctx context.Context, db *sql.DB, c *config.Config, note string) error {
	return recordTx(ctx, db, c, note)
}

// recordTx is Record inside a caller's transaction, so a config change and the
// revision describing it commit together. A revision recorded outside the
// transaction can survive a write that rolled back, which would describe a
// state the system never had.
func recordTx(ctx context.Context, db querier, c *config.Config, note string) error {
	b, err := yaml.Marshal(config.ForWriting(c))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO config_revision (ts, note, yaml) VALUES (?, ?, ?)`,
		time.Now().UnixMilli(), note, string(b))
	return err
}

// Revisions lists recorded states, newest first.
func Revisions(ctx context.Context, db *sql.DB, limit int) ([]Revision, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, ts, note, LENGTH(yaml) FROM config_revision ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		var ts int64
		if err := rows.Scan(&r.ID, &ts, &r.Note, &r.Size); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevisionYAML returns one revision's config.
func RevisionYAML(ctx context.Context, db *sql.DB, id int64) (string, error) {
	var y string
	err := db.QueryRowContext(ctx, `SELECT yaml FROM config_revision WHERE id = ?`, id).Scan(&y)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no revision %d", id)
	}
	return y, err
}

// Restore makes an earlier revision current.
//
// It does NOT rewind the table: the restore is itself a change, so it is stored
// like any other and recorded as a new revision. History that can be rewritten
// is not history, and "we rolled back at 14:02" is exactly the fact somebody
// will need later.
func (s *Source) Restore(ctx context.Context, id int64) error {
	y, err := RevisionYAML(ctx, s.DB, id)
	if err != nil {
		return err
	}
	var c config.Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		return fmt.Errorf("revision %d does not parse: %w", id, err)
	}
	// Through Save, so a revision that is no longer valid — it names a model
	// something else now depends on, or a server that has been removed — is
	// refused rather than restored into a daemon that cannot run it. Save
	// records the revision itself; recording again here would double every
	// restore in the history.
	if err := s.WithNote(fmt.Sprintf("restored revision %d", id)).Save(ctx, &c); err != nil {
		return fmt.Errorf("restoring revision %d: %w", id, err)
	}
	return nil
}

// PruneRevisions keeps the newest `keep`.
func PruneRevisions(ctx context.Context, db *sql.DB, keep int) (int64, error) {
	if keep <= 0 {
		keep = 100
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM config_revision WHERE id NOT IN (
		    SELECT id FROM config_revision ORDER BY id DESC LIMIT ?
		 )`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
