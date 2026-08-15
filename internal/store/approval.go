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
	// Upstream is the provider's own model id, set only when the model was
	// picked off a catalogue by hand instead of being admitted by a discovery
	// filter. For those rows this decision is the ONLY record the model should
	// exist — nothing else knows the id to put on the wire, and ServedName is
	// lossy so the served name cannot be turned back into it.
	//
	// Empty on every row written before catalogue browsing, and on every row
	// that decides the fate of something discovery found. Both are correct:
	// there, discovery supplies the id.
	Upstream string
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
INSERT INTO model_approval (provider, credential, model, state, lanes, quality, note, at, upstream)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(provider, credential, model) DO UPDATE SET
  state=excluded.state, lanes=excluded.lanes, quality=excluded.quality,
  note=excluded.note, at=excluded.at,
  -- Keep the recorded id when an update omits it. A later decision on a
  -- hand-picked model (reject it, move its lane) arrives from a form that has
  -- no reason to echo the upstream id back, and clearing it here would strand
  -- the model with no way to address it upstream.
  upstream=CASE WHEN excluded.upstream='' THEN model_approval.upstream ELSE excluded.upstream END`,
		a.Provider, a.Credential, a.Model, a.State, string(lanes), a.Quality, a.Note, a.At.UnixMilli(), a.Upstream)
	return err
}

// LoadApprovals returns every recorded decision, for seeding the in-memory view
// at startup. A decision that did not survive a restart would put the model
// back in the queue on the next refresh and ask the operator again.
func (s *Store) LoadApprovals() ([]ModelApproval, error) {
	rows, err := s.db.Query(`
SELECT provider, credential, model, state, lanes, quality, note, at, upstream FROM model_approval`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelApproval
	for rows.Next() {
		var a ModelApproval
		var lanes string
		var atMS int64
		if err := rows.Scan(&a.Provider, &a.Credential, &a.Model, &a.State, &lanes, &a.Quality, &a.Note, &atMS, &a.Upstream); err != nil {
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
