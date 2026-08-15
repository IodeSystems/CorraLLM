package store

import (
	"encoding/json"
	"time"
)

// Approval states. A discovered model is in exactly one per (provider,
// credential) pair.
const (
	// ApprovalPending — discovered, no decision yet. Does not serve.
	ApprovalPending = "pending"
	// ApprovalApproved — a human said yes, on the terms in LaneRefs/Quality.
	ApprovalApproved = "approved"
	// ApprovalRejected — a human said no. Never re-asked; the row is what stops
	// the same model reappearing in the queue on every refresh.
	ApprovalRejected = "rejected"
)

// LaneRef is one lane a model was approved into, and where it sits in that
// lane's ladder.
//
// Order is explicit rather than implied by list position because a lane is
// assembled from several sources — declared members, selector expansions,
// approvals — and only a number lets them interleave deterministically. Lower
// sorts earlier, matching "priority 1 is tried first".
type LaneRef struct {
	Lane  string `json:"lane"`
	Order int    `json:"order"`
}

// ModelApproval is one decision about one discovered model on one credential.
type ModelApproval struct {
	Provider   string
	Credential string
	Model      string
	State      string
	Lanes      []LaneRef
	Quality    float64
	Note       string
	At         time.Time
}

// SaveApproval records or replaces a decision.
func (s *Store) SaveApproval(a ModelApproval) error {
	lanes, err := json.Marshal(a.Lanes)
	if err != nil {
		return err
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	_, err = s.db.Exec(`
INSERT INTO model_approval (provider, credential, model, state, lanes, quality, note, at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(provider, credential, model) DO UPDATE SET
  state=excluded.state, lanes=excluded.lanes, quality=excluded.quality,
  note=excluded.note, at=excluded.at`,
		a.Provider, a.Credential, a.Model, a.State, string(lanes), a.Quality, a.Note, a.At.UnixMilli())
	return err
}

// LoadApprovals returns every recorded decision, for seeding the in-memory view
// at startup. A decision that did not survive a restart would put the model
// back in the queue on the next refresh and ask the operator again.
func (s *Store) LoadApprovals() ([]ModelApproval, error) {
	rows, err := s.db.Query(`
SELECT provider, credential, model, state, lanes, quality, note, at FROM model_approval`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelApproval
	for rows.Next() {
		var a ModelApproval
		var lanes string
		var atMS int64
		if err := rows.Scan(&a.Provider, &a.Credential, &a.Model, &a.State, &lanes, &a.Quality, &a.Note, &atMS); err != nil {
			return nil, err
		}
		if lanes != "" {
			// A row whose JSON cannot be parsed keeps its state and loses only
			// its lane placement: refusing the whole row would resurrect a
			// rejection, which is the worse failure.
			_ = json.Unmarshal([]byte(lanes), &a.Lanes)
		}
		a.At = time.UnixMilli(atMS)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteApproval removes a decision, returning the model to pending.
func (s *Store) DeleteApproval(provider, credential, model string) error {
	_, err := s.db.Exec(
		`DELETE FROM model_approval WHERE provider=? AND credential=? AND model=?`,
		provider, credential, model)
	return err
}
