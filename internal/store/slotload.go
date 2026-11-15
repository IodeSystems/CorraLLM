package store

// LoadSpan is one finished request reduced to the four moments the slot-load
// view needs. The activity row is written at COMPLETION (ts is the end), so
// every other moment is derived backwards from it:
//
//	arrival  = EndMS - DwellMS
//	admitted = arrival + QueuedMS        (queue wait ends)
//	service  = admitted + LoadMS → EndMS (the slot is held; same span as ServiceStats)
//
// PredictedPerSec is the backend's own decode rate, kept so the generated
// tokens can be placed on the decode span (EndMS - tokens/rate → EndMS) rather
// than spread over prefill too.
type LoadSpan struct {
	Served           string
	EndMS            int64
	DwellMS          int64
	QueuedMS         int64
	LoadMS           int64
	CompletionTokens int64
	PredictedPerSec  float64
}

// LoadSpans returns every request that ENDED at or after fromMS, so any request
// that overlaps [fromMS, now] is included however long ago it arrived. Served
// on idx_activity_ts; the 15-minute window it is called with is a few hundred
// rows on box1.
func (s *Store) LoadSpans(fromMS int64) ([]LoadSpan, error) {
	rows, err := s.db.Query(
		`SELECT served, ts, dwell_ms, queued_ms, load_ms, completion_tokens, predicted_per_sec
		   FROM activity
		  WHERE ts >= ? AND served <> ''`, fromMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LoadSpan
	for rows.Next() {
		var l LoadSpan
		if err := rows.Scan(&l.Served, &l.EndMS, &l.DwellMS, &l.QueuedMS, &l.LoadMS, &l.CompletionTokens, &l.PredictedPerSec); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
