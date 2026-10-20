package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// authUserSchema is applied on Open alongside the main schema.
//
// Runtime state about SSO users, kept out of config for the same reason
// enrollment tokens are: it changes on every sign-in, and config is a document
// people read and restore. not_before_ms is what makes a stateless session
// cookie revocable: a session issued at or before it is refused.
const authUserSchema = `
CREATE TABLE IF NOT EXISTS auth_users_state (
  email          TEXT PRIMARY KEY,
  not_before_ms  INTEGER NOT NULL DEFAULT 0,
  last_signin_ms INTEGER NOT NULL DEFAULT 0,
  last_signin_ip TEXT NOT NULL DEFAULT ''
);
`

// AuthUserState is one row of auth_users_state.
type AuthUserState struct {
	NotBeforeMS  int64
	LastSignInMS int64
	LastSignInIP string
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// AuthNotBefore returns the revocation cutoff for email; 0 when none.
func (s *Store) AuthNotBefore(ctx context.Context, email string) (int64, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx,
		`SELECT not_before_ms FROM auth_users_state WHERE email = ?`, normEmail(email)).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return ms, err
}

// AuthRevoke refuses every session for email issued at or before atMS.
func (s *Store) AuthRevoke(ctx context.Context, email string, atMS int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_users_state (email, not_before_ms) VALUES (?, ?)
		 ON CONFLICT(email) DO UPDATE SET not_before_ms = excluded.not_before_ms`,
		normEmail(email), atMS)
	return err
}

// AuthRecordSignIn notes a successful SSO sign-in.
func (s *Store) AuthRecordSignIn(ctx context.Context, email, ip string, atMS int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_users_state (email, last_signin_ms, last_signin_ip) VALUES (?, ?, ?)
		 ON CONFLICT(email) DO UPDATE SET last_signin_ms = excluded.last_signin_ms, last_signin_ip = excluded.last_signin_ip`,
		normEmail(email), atMS, ip)
	return err
}

// AuthUserStates returns every row, keyed by email.
func (s *Store) AuthUserStates(ctx context.Context) (map[string]AuthUserState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, not_before_ms, last_signin_ms, last_signin_ip FROM auth_users_state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]AuthUserState{}
	for rows.Next() {
		var e string
		var st AuthUserState
		if err := rows.Scan(&e, &st.NotBeforeMS, &st.LastSignInMS, &st.LastSignInIP); err != nil {
			return nil, err
		}
		out[e] = st
	}
	return out, rows.Err()
}
