package store

// weatherSchema caches hourly outdoor temperatures per location, filled by a
// background job (cmd/corrallm runWeather) so no dashboard request waits on
// the network. Keyed by location as well as hour: moving the configured
// location must not reuse another place's weather.
const weatherSchema = `CREATE TABLE IF NOT EXISTS weather_hourly (
  loc     TEXT    NOT NULL,   -- "lat,lon" at 2 decimals, e.g. "47.61,-122.33"
  hour_ms INTEGER NOT NULL,   -- the hour's start, unix ms UTC
  temp_f  REAL    NOT NULL,
  PRIMARY KEY (loc, hour_ms)
)`

// UpsertWeather stores hourly temperatures for loc, replacing any it had for
// the same hours (a forecast hour becomes an observed-ish one as time passes).
func (s *Store) UpsertWeather(loc string, hours map[int64]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for h, f := range hours {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO weather_hourly (loc, hour_ms, temp_f) VALUES (?, ?, ?)`, loc, h, f); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WeatherBetween returns loc's cached hourly temperatures with hour starts in
// [fromMS, toMS), keyed by hour start.
func (s *Store) WeatherBetween(loc string, fromMS, toMS int64) (map[int64]float64, error) {
	rows, err := s.db.Query(`SELECT hour_ms, temp_f FROM weather_hourly WHERE loc = ? AND hour_ms >= ? AND hour_ms < ?`, loc, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]float64{}
	for rows.Next() {
		var h int64
		var f float64
		if err := rows.Scan(&h, &f); err != nil {
			return nil, err
		}
		out[h] = f
	}
	return out, rows.Err()
}

// PruneWeather drops cached hours older than beforeMS.
func (s *Store) PruneWeather(beforeMS int64) error {
	_, err := s.db.Exec(`DELETE FROM weather_hourly WHERE hour_ms < ?`, beforeMS)
	return err
}

// HourCost is one served model's metered cost in one hour.
type HourCost struct {
	Served  string
	HourMS  int64
	CostUSD float64
}

// CostByHour sums cost_usd per served model per UTC hour over a window — the
// hourly energy a thermal cost is priced against when the outdoor temperature
// changes hour to hour. cost_usd sits before the captured payload, so this
// reads no overflow pages.
func (s *Store) CostByHour(w Window) ([]HourCost, error) {
	wWhere, wArgs := w.ts()
	rows, err := s.db.Query(
		`SELECT served, (ts / 3600000) * 3600000 AS hour, SUM(cost_usd)
		 FROM activity WHERE `+wWhere+` AND cost_usd > 0
		 GROUP BY served, hour`, wArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HourCost
	for rows.Next() {
		var h HourCost
		if err := rows.Scan(&h.Served, &h.HourMS, &h.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
