package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// enrollSchema is applied on Open alongside the main schema.
//
// Enrollment tokens are runtime state, not configuration: they expire, they get
// consumed, and they are secrets. Config is the wrong home for all three — it is
// rewritten, read by the dashboard, and meant to be legible.
const enrollSchema = `
CREATE TABLE IF NOT EXISTS enrollment_tokens (
  token_sha  TEXT PRIMARY KEY,
  server     TEXT NOT NULL DEFAULT '',
  note       TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL,
  expires_ms INTEGER NOT NULL,
  used_ms    INTEGER NOT NULL DEFAULT 0,
  used_by    TEXT NOT NULL DEFAULT ''
);
`

// EnrollmentToken is a one-time credential that lets a machine attach itself.
type EnrollmentToken struct {
	Server    string // the server name it may claim; empty = the agent chooses
	Note      string
	CreatedMS int64
	ExpiresMS int64
	UsedMS    int64
	UsedBy    string
}

// hashToken stores only the digest.
//
// The plaintext is shown once, at mint time, and never again — a leaked
// database should not hand someone the ability to attach a machine that runs
// arbitrary commands. Same reasoning as never storing a password.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewEnrollmentToken mints one and returns the PLAINTEXT, which is the only
// time it exists outside the caller.
func (s *Store) NewEnrollmentToken(server, note string, ttl time.Duration) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := "enr_" + hex.EncodeToString(raw)
	now := time.Now()
	if ttl <= 0 {
		ttl = time.Hour
	}
	_, err := s.db.Exec(
		`INSERT INTO enrollment_tokens (token_sha, server, note, created_ms, expires_ms) VALUES (?,?,?,?,?)`,
		hashToken(tok), server, note, now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return "", err
	}
	return tok, nil
}

// ClaimEnrollmentToken consumes a token, returning what it authorises.
//
// Single use, checked and marked in one statement so two agents racing the same
// token cannot both win — the loser gets "already used" rather than a second
// machine silently attaching under the same identity.
func (s *Store) ClaimEnrollmentToken(tok, byWhom string) (EnrollmentToken, error) {
	var out EnrollmentToken
	sha := hashToken(tok)
	now := time.Now().UnixMilli()

	row := s.db.QueryRow(
		`SELECT server, note, created_ms, expires_ms, used_ms, used_by FROM enrollment_tokens WHERE token_sha = ?`, sha)
	if err := row.Scan(&out.Server, &out.Note, &out.CreatedMS, &out.ExpiresMS, &out.UsedMS, &out.UsedBy); err != nil {
		return out, fmt.Errorf("enrollment token not recognised")
	}
	if out.UsedMS != 0 {
		return out, fmt.Errorf("enrollment token was already used (by %q)", out.UsedBy)
	}
	if now > out.ExpiresMS {
		return out, fmt.Errorf("enrollment token expired")
	}

	// Conditional update IS the lock: only the first claimer sees rows affected.
	res, err := s.db.Exec(
		`UPDATE enrollment_tokens SET used_ms = ?, used_by = ? WHERE token_sha = ? AND used_ms = 0`,
		now, byWhom, sha)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, fmt.Errorf("enrollment token was claimed concurrently")
	}
	out.UsedMS, out.UsedBy = now, byWhom
	return out, nil
}

// ListEnrollmentTokens returns tokens for the dashboard, newest first. Never
// includes the plaintext — it does not exist here.
func (s *Store) ListEnrollmentTokens(limit int) ([]EnrollmentToken, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT server, note, created_ms, expires_ms, used_ms, used_by
		 FROM enrollment_tokens ORDER BY created_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EnrollmentToken
	for rows.Next() {
		var t EnrollmentToken
		if err := rows.Scan(&t.Server, &t.Note, &t.CreatedMS, &t.ExpiresMS, &t.UsedMS, &t.UsedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PeekEnrollmentToken reports what a token authorises WITHOUT consuming it.
//
// Enrollment has to know the server name a token fixes before it can validate
// the rest of the request, and validating after consuming means a rejected
// request burns a single-use credential — the operator then has to mint another
// to fix a typo. Peek first, claim once everything that can fail has passed.
func (s *Store) PeekEnrollmentToken(tok string) (EnrollmentToken, error) {
	var out EnrollmentToken
	row := s.db.QueryRow(
		`SELECT server, note, created_ms, expires_ms, used_ms, used_by FROM enrollment_tokens WHERE token_sha = ?`,
		hashToken(tok))
	if err := row.Scan(&out.Server, &out.Note, &out.CreatedMS, &out.ExpiresMS, &out.UsedMS, &out.UsedBy); err != nil {
		return out, fmt.Errorf("enrollment token not recognised")
	}
	if out.UsedMS != 0 {
		return out, fmt.Errorf("enrollment token was already used (by %q)", out.UsedBy)
	}
	if time.Now().UnixMilli() > out.ExpiresMS {
		return out, fmt.Errorf("enrollment token expired")
	}
	return out, nil
}
