package store

// LaneRoute is how many requests that asked for one name (a lane or alias)
// a given model answered in a window.
type LaneRoute struct {
	Requested string
	Served    string
	N         int64
}

// LaneRoutes counts, per (requested, served) pair, the requests a model
// answered (status < 400) under a name other than its own — what a lane routed
// to each rung. A request nobody answered keeps served = requested and is not
// a route; it is counted by RequestedTotals instead.
func (s *Store) LaneRoutes(w Window) ([]LaneRoute, error) {
	wWhere, wArgs := w.ts()
	rows, err := s.db.Query(`
		SELECT requested, served, COUNT(*)
		  FROM activity
		 WHERE `+wWhere+` AND requested <> '' AND requested <> served AND status < 400
		 GROUP BY requested, served`, wArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LaneRoute
	for rows.Next() {
		var r LaneRoute
		if err := rows.Scan(&r.Requested, &r.Served, &r.N); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestedTotals counts every request per requested name in a window,
// answered or not — the "asked for" total of a lane.
func (s *Store) RequestedTotals(w Window) (map[string]int64, error) {
	wWhere, wArgs := w.ts()
	rows, err := s.db.Query(`
		SELECT requested, COUNT(*) FROM activity
		 WHERE `+wWhere+` AND requested <> ''
		 GROUP BY requested`, wArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}
