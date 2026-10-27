package store

// ChildKey is one row of child_key. Limits is the JSON of a config.LimitSet
// and Credentials a JSON list of credential scope keys; Grants is the JSON of a
// config.Grant list (the key's allow patterns). The store interprets none of
// them. Parent "" is a key an admin issued (P62), which carries its own Group,
// Credentials, Grants, CanMint and Disabled; a child's are unused.
//
// Disabled is the one flag every key carries: a runtime key is created with it
// SET, so minting a key is not the same act as allowing it to spend.
type ChildKey struct {
	Key         string
	Parent      string
	Label       string
	Limits      string
	CreatedMS   int64
	UpdatedMS   int64
	ExpiresMS   int64 // 0 = never
	Group       string
	Credentials string
	CanMint     bool
	Disabled    bool
	Grants      string
}

// SaveChildKey upserts a child key. created_at is kept on update.
func (s *Store) SaveChildKey(k ChildKey) error {
	_, err := s.db.Exec(`
INSERT INTO child_key (key, parent, label, limits, created_at, updated_at, expires_at, grp, credentials, can_mint, disabled, grants)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET label=excluded.label, limits=excluded.limits, updated_at=excluded.updated_at,
  expires_at=excluded.expires_at, grp=excluded.grp, credentials=excluded.credentials, can_mint=excluded.can_mint,
  disabled=excluded.disabled, grants=excluded.grants`,
		k.Key, k.Parent, k.Label, k.Limits, k.CreatedMS, k.UpdatedMS, k.ExpiresMS, k.Group, k.Credentials,
		boolInt(k.CanMint), boolInt(k.Disabled), k.Grants)
	return err
}

// DeleteChildKey removes a child key.
func (s *Store) DeleteChildKey(key string) error {
	_, err := s.db.Exec(`DELETE FROM child_key WHERE key = ?`, key)
	return err
}

// LoadChildKeys returns every child key, for the proxy's in-memory roster.
func (s *Store) LoadChildKeys() ([]ChildKey, error) {
	rows, err := s.db.Query(`SELECT key, parent, label, limits, created_at, updated_at, expires_at, grp, credentials, can_mint, disabled, grants
FROM child_key ORDER BY parent, created_at, key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ChildKey
	for rows.Next() {
		var k ChildKey
		var mint, disabled int
		if err := rows.Scan(&k.Key, &k.Parent, &k.Label, &k.Limits, &k.CreatedMS, &k.UpdatedMS,
			&k.ExpiresMS, &k.Group, &k.Credentials, &mint, &disabled, &k.Grants); err != nil {
			return nil, err
		}
		k.CanMint, k.Disabled = mint != 0, disabled != 0
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyGrant is one row of key_grant: a one-time value add to a caller key.
type KeyGrant struct {
	ID        int64
	Key       string
	USD       float64
	Model     string // "" = any model
	Spent     float64
	ExpiresMS int64 // 0 = until spent
	CreatedMS int64
	Note      string
}

// AddKeyGrant stores a new grant and returns its id.
func (s *Store) AddKeyGrant(g KeyGrant) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO key_grant (key, usd, model, spent, expires_at, created_at, note) VALUES (?,?,?,?,?,?,?)`,
		g.Key, g.USD, g.Model, g.Spent, g.ExpiresMS, g.CreatedMS, g.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SpendKeyGrant adds usd to a grant's spent amount and returns the new total.
// A delta, not an absolute write, so two processes during a handoff both count.
func (s *Store) SpendKeyGrant(id int64, usd float64) (float64, error) {
	var spent float64
	err := s.db.QueryRow(`UPDATE key_grant SET spent = spent + ? WHERE id = ? RETURNING spent`, usd, id).Scan(&spent)
	return spent, err
}

// UpdateKeyGrant changes a grant's amount, expiry and note.
func (s *Store) UpdateKeyGrant(id int64, usd float64, expiresMS int64, note string) error {
	_, err := s.db.Exec(`UPDATE key_grant SET usd = ?, expires_at = ?, note = ? WHERE id = ?`, usd, expiresMS, note, id)
	return err
}

// DeleteKeyGrant removes a grant.
func (s *Store) DeleteKeyGrant(id int64) error {
	_, err := s.db.Exec(`DELETE FROM key_grant WHERE id = ?`, id)
	return err
}

// LoadKeyGrants returns every grant, oldest first.
func (s *Store) LoadKeyGrants() ([]KeyGrant, error) {
	rows, err := s.db.Query(`SELECT id, key, usd, model, spent, expires_at, created_at, note FROM key_grant ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KeyGrant
	for rows.Next() {
		var g KeyGrant
		if err := rows.Scan(&g.ID, &g.Key, &g.USD, &g.Model, &g.Spent, &g.ExpiresMS, &g.CreatedMS, &g.Note); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
