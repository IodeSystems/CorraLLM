package store

// ChildKey is one row of child_key. Limits is the JSON of a config.LimitSet;
// the store does not interpret it.
type ChildKey struct {
	Key       string
	Parent    string
	Label     string
	Limits    string
	CreatedMS int64
	UpdatedMS int64
}

// SaveChildKey upserts a child key. created_at is kept on update.
func (s *Store) SaveChildKey(k ChildKey) error {
	_, err := s.db.Exec(`
INSERT INTO child_key (key, parent, label, limits, created_at, updated_at) VALUES (?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET label=excluded.label, limits=excluded.limits, updated_at=excluded.updated_at`,
		k.Key, k.Parent, k.Label, k.Limits, k.CreatedMS, k.UpdatedMS)
	return err
}

// DeleteChildKey removes a child key.
func (s *Store) DeleteChildKey(key string) error {
	_, err := s.db.Exec(`DELETE FROM child_key WHERE key = ?`, key)
	return err
}

// LoadChildKeys returns every child key, for the proxy's in-memory roster.
func (s *Store) LoadChildKeys() ([]ChildKey, error) {
	rows, err := s.db.Query(`SELECT key, parent, label, limits, created_at, updated_at FROM child_key ORDER BY parent, created_at, key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ChildKey
	for rows.Next() {
		var k ChildKey
		if err := rows.Scan(&k.Key, &k.Parent, &k.Label, &k.Limits, &k.CreatedMS, &k.UpdatedMS); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
