package store

import "database/sql"

// AccessRequest is one row of access_request. Asked and Granted are JSON the
// store does not interpret.
type AccessRequest struct {
	ID           string
	ClaimHash    string
	Status       string // pending | approved | denied
	RequesterKey string
	SourceIP     string
	Via          string // the accessRequests rule that admitted it
	FiledBy      string // dashboard user, if filed from the dashboard
	Asked        string
	Granted      string
	IssuedKey    string
	Delivered    bool
	Reason       string
	CreatedMS    int64
	DecidedMS    int64
	DecidedBy    string
}

const accessRequestCols = `id, claim_hash, status, requester_key, source_ip, via, filed_by, asked, granted, issued_key,
  delivered, reason, created_at, decided_at, decided_by`

func scanAccessRequest(sc interface{ Scan(...any) error }) (AccessRequest, error) {
	var a AccessRequest
	var delivered int
	err := sc.Scan(&a.ID, &a.ClaimHash, &a.Status, &a.RequesterKey, &a.SourceIP, &a.Via, &a.FiledBy, &a.Asked, &a.Granted,
		&a.IssuedKey, &delivered, &a.Reason, &a.CreatedMS, &a.DecidedMS, &a.DecidedBy)
	a.Delivered = delivered != 0
	return a, err
}

// SaveAccessRequest upserts a request.
func (s *Store) SaveAccessRequest(a AccessRequest) error {
	_, err := s.db.Exec(`
INSERT INTO access_request (`+accessRequestCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET status=excluded.status, granted=excluded.granted, issued_key=excluded.issued_key,
  delivered=excluded.delivered, reason=excluded.reason, decided_at=excluded.decided_at, decided_by=excluded.decided_by`,
		a.ID, a.ClaimHash, a.Status, a.RequesterKey, a.SourceIP, a.Via, a.FiledBy, a.Asked, a.Granted, a.IssuedKey,
		boolInt(a.Delivered), a.Reason, a.CreatedMS, a.DecidedMS, a.DecidedBy)
	return err
}

// AccessRequestByID returns one request; ok is false when there is none.
func (s *Store) AccessRequestByID(id string) (AccessRequest, bool, error) {
	a, err := scanAccessRequest(s.db.QueryRow(`SELECT `+accessRequestCols+` FROM access_request WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return AccessRequest{}, false, nil
	}
	return a, err == nil, err
}

// MarkAccessDelivered records that the issued key was handed over, once:
// it reports false when another read got there first.
func (s *Store) MarkAccessDelivered(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE access_request SET delivered = 1 WHERE id = ? AND delivered = 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AccessRequests lists requests, pending first, then newest first; at most limit.
func (s *Store) AccessRequests(limit int) ([]AccessRequest, error) {
	rows, err := s.db.Query(`SELECT `+accessRequestCols+` FROM access_request
ORDER BY (status = 'pending') DESC, created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AccessRequest
	for rows.Next() {
		a, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingAccessRequests counts pending requests in all and from one source IP.
func (s *Store) PendingAccessRequests(ip string) (total, fromIP int, err error) {
	err = s.db.QueryRow(`SELECT count(*), coalesce(sum(source_ip = ?), 0) FROM access_request WHERE status = 'pending'`, ip).
		Scan(&total, &fromIP)
	return total, fromIP, err
}
