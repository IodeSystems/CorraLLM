package store

import (
	"strings"
)

// providerBillingSchema holds what a provider's own bill says next to what
// corrallm metered, per hour and upstream model. Registered in `migrations`
// (CREATE ... IF NOT EXISTS is idempotent there, as conversationSchema is).
//
// A table rather than a recomputation because the provider's figure is only
// available over its billing API, which is slow, rate-limited and revised for
// a few hours after the fact. Keeping the last answer lets the dashboard show
// it, and the alert compare against it, without a network call.
const providerBillingSchema = `
CREATE TABLE IF NOT EXISTS provider_billing (
    provider     TEXT    NOT NULL,           -- provider name in the config
    credential   TEXT    NOT NULL,           -- the account billed
    upstream     TEXT    NOT NULL,           -- the provider's model id, as on its bill
    served       TEXT    NOT NULL DEFAULT '', -- corrallm model(s) mapped to it, comma-separated; '' = none configured
    hour         INTEGER NOT NULL,           -- unix millis, start of the UTC hour
    billed_usd   REAL    NOT NULL DEFAULT 0, -- what the provider billed for that hour
    recorded_usd REAL    NOT NULL DEFAULT 0, -- sum of activity.cost_usd for the mapped models that hour
    incomplete   INTEGER NOT NULL DEFAULT 0, -- 1 = the provider may still revise this hour
    fetched_at   INTEGER NOT NULL DEFAULT 0, -- unix millis of the fetch that wrote it
    PRIMARY KEY (provider, credential, upstream, hour)
)`

// providerBillingFetchSchema is one row per (provider, credential): when the
// last fetch ran and whether it worked. A failure leaves provider_billing as it
// was, so the error and the stale numbers are both visible.
const providerBillingFetchSchema = `
CREATE TABLE IF NOT EXISTS provider_billing_fetch (
    provider   TEXT    NOT NULL,
    credential TEXT    NOT NULL,
    fetched_at INTEGER NOT NULL DEFAULT 0, -- unix millis of the last attempt
    ok_at      INTEGER NOT NULL DEFAULT 0, -- unix millis of the last success; 0 = never
    error      TEXT    NOT NULL DEFAULT '', -- the last attempt's error, '' = ok
    incomplete INTEGER NOT NULL DEFAULT 0,  -- the last success still had unsettled hours
    PRIMARY KEY (provider, credential)
)`

// BillingRow is one hour of one upstream model: billed vs recorded.
type BillingRow struct {
	Provider, Credential, Upstream, Served string
	HourMS                                 int64
	BilledUSD, RecordedUSD                 float64
	Incomplete                             bool
}

// BillingFetch is the outcome of the latest reconciliation of one credential.
type BillingFetch struct {
	Provider, Credential string
	FetchedMS, OKMS      int64
	Error                string
	Incomplete           bool
}

// ReplaceBilling swaps one credential's rows from sinceMS onward for rows, in
// one transaction, and records the fetch as a success. Older hours are kept:
// they are settled history the month view no longer refetches.
func (s *Store) ReplaceBilling(provider, credential string, sinceMS, nowMS int64, rows []BillingRow, incomplete bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM provider_billing WHERE provider=? AND credential=? AND hour>=?`,
		provider, credential, sinceMS); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO provider_billing
			(provider, credential, upstream, served, hour, billed_usd, recorded_usd, incomplete, fetched_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			provider, credential, r.Upstream, r.Served, r.HourMS, r.BilledUSD, r.RecordedUSD, b2i(r.Incomplete), nowMS); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO provider_billing_fetch (provider, credential, fetched_at, ok_at, error, incomplete)
		VALUES (?,?,?,?,'',?)
		ON CONFLICT(provider, credential) DO UPDATE SET fetched_at=excluded.fetched_at, ok_at=excluded.ok_at,
		  error='', incomplete=excluded.incomplete`,
		provider, credential, nowMS, nowMS, b2i(incomplete)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordBillingError notes a failed fetch without touching the stored rows.
func (s *Store) RecordBillingError(provider, credential string, nowMS int64, msg string) error {
	_, err := s.db.Exec(`INSERT INTO provider_billing_fetch (provider, credential, fetched_at, error)
		VALUES (?,?,?,?)
		ON CONFLICT(provider, credential) DO UPDATE SET fetched_at=excluded.fetched_at, error=excluded.error`,
		provider, credential, nowMS, msg)
	return err
}

// BillingRows returns every stored hour from sinceMS on, oldest first.
func (s *Store) BillingRows(sinceMS int64) ([]BillingRow, error) {
	rows, err := s.db.Query(`SELECT provider, credential, upstream, served, hour, billed_usd, recorded_usd, incomplete
		FROM provider_billing WHERE hour>=? ORDER BY provider, credential, hour, upstream`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BillingRow
	for rows.Next() {
		var r BillingRow
		var inc int
		if err := rows.Scan(&r.Provider, &r.Credential, &r.Upstream, &r.Served, &r.HourMS, &r.BilledUSD, &r.RecordedUSD, &inc); err != nil {
			return nil, err
		}
		r.Incomplete = inc != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// BillingFetches returns the latest fetch outcome per credential.
func (s *Store) BillingFetches() ([]BillingFetch, error) {
	rows, err := s.db.Query(`SELECT provider, credential, fetched_at, ok_at, error, incomplete
		FROM provider_billing_fetch ORDER BY provider, credential`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BillingFetch
	for rows.Next() {
		var f BillingFetch
		var inc int
		if err := rows.Scan(&f.Provider, &f.Credential, &f.FetchedMS, &f.OKMS, &f.Error, &inc); err != nil {
			return nil, err
		}
		f.Incomplete = inc != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// RecordedCostByHour sums activity.cost_usd per served model and UTC hour in
// [fromMS, toMS): what corrallm metered, the figure the provider's bill is
// checked against. Keyed served -> hour(unix millis) -> dollars.
func (s *Store) RecordedCostByHour(served []string, fromMS, toMS int64) (map[string]map[int64]float64, error) {
	out := map[string]map[int64]float64{}
	if len(served) == 0 {
		return out, nil
	}
	args := []any{fromMS, toMS}
	for _, n := range served {
		args = append(args, n)
	}
	rows, err := s.db.Query(`SELECT served, (ts/3600000)*3600000 AS hour, COALESCE(SUM(cost_usd),0)
		FROM activity WHERE ts>=? AND ts<? AND cost_usd>0 AND served IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(served)), ",")+`)
		GROUP BY served, hour`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var hour int64
		var usd float64
		if err := rows.Scan(&name, &hour, &usd); err != nil {
			return nil, err
		}
		if out[name] == nil {
			out[name] = map[int64]float64{}
		}
		out[name][hour] = usd
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
