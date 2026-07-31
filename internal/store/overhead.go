package store

import "context"

// Overhead is the time corrallm itself added to a window of requests: waiting
// rather than computing.
type Overhead struct {
	// Requests is how many rows were summed. Zero means the window matched
	// nothing, which is different from "matched requests that never waited" —
	// a caller cannot tell those apart from the durations alone.
	Requests int   `json:"requests"`
	QueuedMS int64 `json:"queuedMs"`
	LoadMS   int64 `json:"loadMs"`
}

// OverheadFor sums the queue and load time corrallm imposed on one caller's
// requests to one model within a time window.
//
// It exists so a benchmark can subtract corrallm's own latency from its wall
// clock. The alternative — reading it off each response — cannot work through
// agentkit's client, which does not surface response headers, and would need
// summing across an agent loop's many calls anyway. This answers that in one
// query.
//
// Scoped by caller key AND model because the bench holds one key for a whole
// run: without the model, a stage would be charged for every other combo's
// queueing too. Models are benched sequentially, so within one model the only
// overlap is that model's own concurrent probes.
//
// tsFrom/tsTo are unix millis, matching activity.ts. The range is inclusive:
// a request that logged exactly at the boundary belongs to the stage that was
// running, and losing it under-reports overhead — which inflates the execution
// time the caller computes.
func (s *Store) OverheadFor(ctx context.Context, key, served string, tsFrom, tsTo int64) (Overhead, error) {
	var o Overhead
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(queued_ms), 0), COALESCE(SUM(load_ms), 0)
FROM activity
WHERE key = ? AND served = ? AND ts >= ? AND ts <= ?`,
		key, served, tsFrom, tsTo).Scan(&o.Requests, &o.QueuedMS, &o.LoadMS)
	return o, err
}
