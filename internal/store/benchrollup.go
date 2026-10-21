package store

import (
	"context"
	"database/sql"
)

// BenchRollup is what the proxy itself recorded for one model's bench run:
// every request it forwarded under a bench key inside the run's window.
//
// It is the proxy's view rather than llm-bench's because the proxy sees the
// wire: usage as the backend billed it, the backend's thinking-token count and
// its timings. One run at a time and one minted `bench-` key per run is what
// makes (key prefix, model, window) identify the run's traffic.
type BenchRollup struct {
	Requests  int // every request, refused ones included
	Turns     int // requests answered 200: model calls that produced a turn
	Refused   int // 429 and other non-200
	Prompt    int64
	Cached    int64
	Generated int64
	// Thinking sums the rows where it was measured; ThinkingRows says how many
	// that was, so a partial count is not read as a total.
	Thinking     int64
	ThinkingRows int
	PeakCtx      int64 // largest prompt+completion of one request
	QueuedMS     int64
	// Prefill and generation seconds, summed only over rows with a measured
	// speed, together with the tokens those rows carried — so a speed is
	// tokens ÷ time over the same rows, never mixed.
	PrefillS, GenS         float64
	PrefillNew, PrefillAll int64 // uncached / all prompt tokens on rows with a prefill speed
	GenTokens              int64 // completion tokens on rows with a generation speed
}

// BenchRollupFor sums the proxy's rows for one model in [fromMS, toMS].
func (s *Store) BenchRollupFor(ctx context.Context, model string, fromMS, toMS int64) (BenchRollup, error) {
	var r BenchRollup
	var thinking sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(status = 200), 0),
       COALESCE(SUM(status <> 200), 0),
       COALESCE(SUM(prompt_tokens), 0),
       COALESCE(SUM(cached_tokens), 0),
       COALESCE(SUM(completion_tokens), 0),
       SUM(CASE WHEN reasoning_tokens >= 0 AND status = 200 THEN reasoning_tokens END),
       COALESCE(SUM(reasoning_tokens >= 0 AND status = 200), 0),
       COALESCE(MAX(prompt_tokens + completion_tokens), 0),
       COALESCE(SUM(queued_ms), 0),
       COALESCE(SUM(CASE WHEN prompt_per_sec > 0 THEN MAX(prompt_tokens - cached_tokens, 0) / prompt_per_sec END), 0),
       COALESCE(SUM(CASE WHEN prompt_per_sec > 0 THEN MAX(prompt_tokens - cached_tokens, 0) END), 0),
       COALESCE(SUM(CASE WHEN prompt_per_sec > 0 THEN prompt_tokens END), 0),
       COALESCE(SUM(CASE WHEN predicted_per_sec > 0 THEN completion_tokens / predicted_per_sec END), 0),
       COALESCE(SUM(CASE WHEN predicted_per_sec > 0 THEN completion_tokens END), 0)
FROM activity
WHERE ts BETWEEN ? AND ? AND served = ? AND key LIKE 'bench-%'`, fromMS, toMS, model).
		Scan(&r.Requests, &r.Turns, &r.Refused, &r.Prompt, &r.Cached, &r.Generated,
			&thinking, &r.ThinkingRows, &r.PeakCtx, &r.QueuedMS,
			&r.PrefillS, &r.PrefillNew, &r.PrefillAll, &r.GenS, &r.GenTokens)
	r.Thinking = thinking.Int64
	return r, err
}

// BenchRequestSample returns one captured request body from the run, or ""
// when payloads were pruned. It shows what the bench asked for — a reasoning
// effort, a thinking switch — which no column records.
func (s *Store) BenchRequestSample(ctx context.Context, model string, fromMS, toMS int64) (string, error) {
	var body sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT p.req_body FROM activity a JOIN activity_payload p ON p.activity_id = a.id
WHERE a.ts BETWEEN ? AND ? AND a.served = ? AND a.key LIKE 'bench-%' AND a.status = 200
  AND p.req_body <> ''
ORDER BY a.ts LIMIT 1`, fromMS, toMS, model).Scan(&body)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return body.String, err
}
