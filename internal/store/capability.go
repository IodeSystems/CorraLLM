package store

// capabilitySchema: one row per fact agentkit knows about one remote model
// (P73). endpoint is the backend's target URL normalized by agentkit
// (Client.Capabilities().Endpoint: no userinfo, query or fragment) and model
// the upstream id,
// the key of the record-only llm.Client the proxy keeps for it. key is an
// agentkit Cap* key; value is the fact's value as JSON; source is agentkit's
// llm.Source (refused, observed, probed, declared). evidence is what was sent
// and what came back, short, empty when the fact has none.
const capabilitySchema = `
CREATE TABLE IF NOT EXISTS capability (
    endpoint TEXT    NOT NULL,
    model    TEXT    NOT NULL,
    key      TEXT    NOT NULL,
    value    TEXT    NOT NULL,           -- JSON
    source   TEXT    NOT NULL,
    at       INTEGER NOT NULL,           -- unix millis
    evidence TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (endpoint, model, key)
)`

// CapabilityFact is one row of the capability table.
type CapabilityFact struct {
	Endpoint, Model, Key string
	Value                string // JSON
	Source               string
	AtMS                 int64
	Evidence             string
}

// PutCapability stores one fact, replacing the one of the same key unless the
// stored one is newer. agentkit calls OnLearn after it releases its lock, so
// two learns of one key can reach here in either order; the fact's own time
// decides, and the table keeps what the record in memory holds.
func (s *Store) PutCapability(f CapabilityFact) error {
	_, err := s.db.Exec(
		`INSERT INTO capability (endpoint, model, key, value, source, at, evidence)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(endpoint, model, key) DO UPDATE SET
		   value = excluded.value, source = excluded.source, at = excluded.at, evidence = excluded.evidence
		 WHERE excluded.at >= capability.at`,
		f.Endpoint, f.Model, f.Key, f.Value, f.Source, f.AtMS, f.Evidence)
	return err
}

// Capabilities returns every stored fact.
func (s *Store) Capabilities() ([]CapabilityFact, error) {
	rows, err := s.db.Query(`SELECT endpoint, model, key, value, source, at, evidence FROM capability
	                         ORDER BY endpoint, model, key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CapabilityFact
	for rows.Next() {
		var f CapabilityFact
		if err := rows.Scan(&f.Endpoint, &f.Model, &f.Key, &f.Value, &f.Source, &f.AtMS, &f.Evidence); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
