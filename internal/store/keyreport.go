package store

import (
	"fmt"
	"strings"
)

// KeyReport is the per-(key, served model) aggregation that backs the
// /v1/keys/report endpoint. Each row answers: "this key made this many calls
// to this model in the window, with these tokens, this much time, and this
// much cost."
//
// The caller passes the list of keys (parent + all its children) so the
// query is a single IN(...) scan on idx_activity_key_rollup; the GROUP BY
// key, served then splits the result naturally.
type KeyReport struct {
	Key              string
	Served           string
	Calls            int64
	Errors           int64 // requests with status >= 400
	DwellMS          int64
	QueuedMS         int64
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64 // only rows where reasoning_tokens >= 0 (measured)
	CostUSD          float64
}

// KeyReport aggregates activity grouped by (key, served) over a window,
// narrowed to the given set of keys. An empty keys slice returns an empty
// slice (not an error): the caller has no children and no self-traffic to
// report.
func (s *Store) KeyReport(w Window, keys []string) ([]KeyReport, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	wWhere, wArgs := w.ts()
	// Build the IN placeholder list.
	placeholders := make([]string, len(keys))
	args := make([]any, 0, len(wArgs)+len(keys))
	args = append(args, wArgs...)
	for i, k := range keys {
		placeholders[i] = "?"
		args = append(args, k)
	}
	inClause := "key IN (" + strings.Join(placeholders, ",") + ")"

	rows, err := s.db.Query(
		`SELECT key,
		        COALESCE(served, ''),
		        COUNT(*),
		        COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(dwell_ms), 0),
		        COALESCE(SUM(queued_ms), 0),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(CASE WHEN reasoning_tokens >= 0 THEN reasoning_tokens ELSE 0 END), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM activity
		 WHERE `+wWhere+` AND `+inClause+`
		 GROUP BY key, served
		 ORDER BY key, served`, args...)
	if err != nil {
		return nil, fmt.Errorf("keyreport: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []KeyReport
	for rows.Next() {
		var r KeyReport
		if err := rows.Scan(&r.Key, &r.Served, &r.Calls, &r.Errors, &r.DwellMS,
			&r.QueuedMS, &r.PromptTokens, &r.CachedTokens, &r.CompletionTokens,
			&r.ReasoningTokens, &r.CostUSD); err != nil {
			return nil, fmt.Errorf("keyreport scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
