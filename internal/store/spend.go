package store

import (
	"database/sql"
	"errors"
)

// spendSchema holds cost tokens and the last real figure given for a provider.
// A token is stored as its hash: it reads spend and nothing else, but a leaked
// table should still not be a list of them.
const spendTokenSchema = `
CREATE TABLE IF NOT EXISTS spend_token (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    hash       TEXT    NOT NULL UNIQUE,
    label      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,          -- unix millis
    expires_at INTEGER NOT NULL DEFAULT 0, -- unix millis, 0 = never
    revoked_at INTEGER NOT NULL DEFAULT 0, -- unix millis, 0 = live
    used_at    INTEGER NOT NULL DEFAULT 0  -- unix millis of the last read
)`

const spendReconcileSchema = `
CREATE TABLE IF NOT EXISTS spend_reconcile (
    provider TEXT    PRIMARY KEY,
    usd      REAL    NOT NULL,   -- what the provider's console showed this month
    at       INTEGER NOT NULL    -- unix millis it was given; spend after it is estimated
)`

// SpendToken is a cost token as listed. The token itself is never kept.
type SpendToken struct {
	ID                                      int64
	Label                                   string
	CreatedMS, ExpiresMS, RevokedMS, UsedMS int64
}

// CreateSpendToken stores a token's hash and returns its id.
func (s *Store) CreateSpendToken(hash, label string, createdMS, expiresMS int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO spend_token (hash, label, created_at, expires_at) VALUES (?,?,?,?)`,
		hash, label, createdMS, expiresMS)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SpendTokens lists every token, newest first.
func (s *Store) SpendTokens() ([]SpendToken, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at, expires_at, revoked_at, used_at FROM spend_token ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SpendToken{}
	for rows.Next() {
		var t SpendToken
		if err := rows.Scan(&t.ID, &t.Label, &t.CreatedMS, &t.ExpiresMS, &t.RevokedMS, &t.UsedMS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeSpendToken ends a token, and reports whether a live one had that id.
func (s *Store) RevokeSpendToken(id, atMS int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE spend_token SET revoked_at=? WHERE id=? AND revoked_at=0`, atMS, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UseSpendToken reports whether hash is a live token at atMS, and records the use.
func (s *Store) UseSpendToken(hash string, atMS int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE spend_token SET used_at=? WHERE hash=? AND revoked_at=0 AND (expires_at=0 OR expires_at>?)`,
		atMS, hash, atMS)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetSpendReconcile stores the real figure for a provider, replacing the last.
func (s *Store) SetSpendReconcile(provider string, usd float64, atMS int64) error {
	_, err := s.db.Exec(`INSERT INTO spend_reconcile (provider, usd, at) VALUES (?,?,?)
		ON CONFLICT(provider) DO UPDATE SET usd=excluded.usd, at=excluded.at`, provider, usd, atMS)
	return err
}

// SpendReconcile returns the last real figure for a provider; ok is false with none.
func (s *Store) SpendReconcile(provider string) (usd float64, atMS int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT usd, at FROM spend_reconcile WHERE provider=?`, provider).Scan(&usd, &atMS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	return usd, atMS, err == nil, err
}
