package store

import (
	"encoding/json"
	"time"
)

// LaneRef is one lane a model was assigned to, and where it sits in that lane's
// ladder.
//
// Order is explicit rather than implied by list position because a lane is
// assembled from several sources — declared members, selector expansions,
// assignments — and only a number lets them interleave deterministically. Lower
// sorts earlier, matching "priority 1 is tried first".
type LaneRef struct {
	Lane  string `json:"lane"`
	Order int    `json:"order"`
}

// ModelSelection is one model an operator chose from a provider's directory,
// and where they put it.
//
// It replaced an approval table, and the difference is the whole point. An
// approval was a VERDICT on something discovery had already found: three states
// (pending/approved/rejected), a queue of questions owed, and a gate that had
// to be switched on per credential before any of it meant anything. Nobody
// wanted a queue. What they wanted was to look at what a provider offers and
// say "that one, in this lane, at this priority".
//
// So there is no state column. The row EXISTS or it does not:
//
//	row present  the model is selected — serve it, and place it as recorded
//	row absent   nothing selected; the model is not served on this account
//
// "Reject" collapses into deleting the row, which is the same operation as
// changing your mind, and needs no vocabulary of its own.
//
// Keyed per (provider, credential, model) because catalogues differ by key: the
// same upstream id can be wanted on one account and not another, and a paid
// key's model is a spending decision the free key's is not.
type ModelSelection struct {
	Provider   string
	Credential string
	Model      string
	// Upstream is the provider's own model id. Set when this row is what makes
	// the model exist at all — picked off a directory rather than admitted by a
	// discover filter — because nothing else knows the id to put on the wire
	// and ServedName is lossy, so the served name cannot be turned back into
	// it. Empty means the row only carries PLACEMENT for a model discovery
	// already contributes.
	Upstream string
	Lanes    []LaneRef
	Quality  float64
	Note     string
	At       time.Time
}

// SaveSelection records or replaces one selection.
func (s *Store) SaveSelection(a ModelSelection) error {
	lanes, err := json.Marshal(a.Lanes)
	if err != nil {
		return err
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	_, err = s.db.Exec(`
INSERT INTO model_selection (provider, credential, model, upstream, lanes, quality, note, at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(provider, credential, model) DO UPDATE SET
  lanes=excluded.lanes, quality=excluded.quality, note=excluded.note, at=excluded.at,
  -- Keep the recorded id when an update omits it. Re-placing a model in another
  -- lane arrives from a form with no reason to echo the upstream id back, and
  -- clearing it would strand the model with no way to address it upstream.
  upstream=CASE WHEN excluded.upstream='' THEN model_selection.upstream ELSE excluded.upstream END`,
		a.Provider, a.Credential, a.Model, a.Upstream, string(lanes), a.Quality, a.Note, a.At.UnixMilli())
	return err
}

// LoadSelections returns every selection, for seeding the in-memory view at
// startup and after each change. A selection that did not survive a restart
// would take a hand-chosen model out of service silently.
func (s *Store) LoadSelections() ([]ModelSelection, error) {
	rows, err := s.db.Query(`
SELECT provider, credential, model, upstream, lanes, quality, note, at FROM model_selection`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelSelection
	for rows.Next() {
		var a ModelSelection
		var lanes string
		var atMS int64
		if err := rows.Scan(&a.Provider, &a.Credential, &a.Model, &a.Upstream, &lanes, &a.Quality, &a.Note, &atMS); err != nil {
			return nil, err
		}
		if lanes != "" {
			// A row whose JSON cannot be parsed keeps the model selected and
			// loses only its lane placement: refusing the whole row would take
			// a working model out of service over a formatting problem.
			_ = json.Unmarshal([]byte(lanes), &a.Lanes)
		}
		a.At = time.UnixMilli(atMS)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteSelection unselects a model. For one picked off a directory this
// removes it entirely; for a discovered model it drops the placement and leaves
// the filter's contribution alone.
func (s *Store) DeleteSelection(provider, credential, model string) error {
	_, err := s.db.Exec(
		`DELETE FROM model_selection WHERE provider=? AND credential=? AND model=?`,
		provider, credential, model)
	return err
}
