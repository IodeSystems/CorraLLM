package store

import "sort"

// ServiceSpan is one served request as the GPU saw it: when its backend was
// working on it, and how its time to first token splits.
//
// activity.ts is stamped when the request ENDS (proxy.log), so the busy span is
// [ts - service, ts], with service = dwell - queued - load: the time a backend
// held the request after admission and after any cold load.
type ServiceSpan struct {
	Served    string
	Placement string
	StartMS   int64 // backend began work (end - service)
	EndMS     int64 // request ended (activity.ts)
	QueuedMS  int64
	LoadMS    int64
	// ActiveTTFTMS is ttfb - queued - load: prompt processing up to the first
	// byte, with corrallm's queue and the cold load taken out. -1 when there is
	// no first-token figure: no ttfb, or a non-streamed reply, whose first byte
	// is the whole answer (ttfb within 100 ms of dwell).
	ActiveTTFTMS int64
	FirstByteMS  int64 // when the first byte reached the caller (epoch ms); 0 when ActiveTTFTMS is -1
}

// ServiceSpans returns every served request in the window (status < 400, some
// backend time), oldest first.
func (s *Store) ServiceSpans(w Window) ([]ServiceSpan, error) {
	wWhere, wArgs := w.ts()
	rows, err := s.db.Query(`
		SELECT served, placement, ts, dwell_ms, queued_ms, load_ms, ttfb_ms
		  FROM activity
		 WHERE `+wWhere+` AND status < 400 AND dwell_ms - queued_ms - load_ms > 0
		 ORDER BY ts`, wArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ServiceSpan
	for rows.Next() {
		var sp ServiceSpan
		var ts, dwell, ttfb int64
		if err := rows.Scan(&sp.Served, &sp.Placement, &ts, &dwell, &sp.QueuedMS, &sp.LoadMS, &ttfb); err != nil {
			return nil, err
		}
		sp.EndMS = ts
		sp.StartMS = ts - (dwell - sp.QueuedMS - sp.LoadMS)
		sp.ActiveTTFTMS = -1
		if ttfb > 0 && dwell-ttfb >= 100 {
			sp.ActiveTTFTMS = max(0, ttfb-sp.QueuedMS-sp.LoadMS)
			sp.FirstByteMS = ts - dwell + ttfb
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// BusyFractions unions the spans and returns, for each of n buckets starting at
// start, the fraction of the bucket at least one span covered, plus the busy
// milliseconds in total. A bucket that runs past now is measured over its
// elapsed part only, so the current bucket is not read as half idle.
//
// The union is what makes this "the GPU was busy", not "requests summed":
// two models on one card serving at once count once.
func BusyFractions(spans []ServiceSpan, start, bucketMS int64, n int, now int64) (frac []float64, busyMS int64) {
	frac = make([]float64, n)
	if n == 0 || bucketMS <= 0 {
		return frac, 0
	}
	iv := make([][2]int64, 0, len(spans))
	for _, sp := range spans {
		if sp.EndMS > sp.StartMS {
			iv = append(iv, [2]int64{sp.StartMS, sp.EndMS})
		}
	}
	sort.Slice(iv, func(i, j int) bool { return iv[i][0] < iv[j][0] })
	end := min(start+int64(n)*bucketMS, now)
	busy := make([]int64, n)
	add := func(a, b int64) {
		a, b = max(a, start), min(b, end)
		for a < b {
			i := (a - start) / bucketMS
			be := start + (i+1)*bucketMS
			seg := min(b, be) - a
			busy[i] += seg
			busyMS += seg
			a += seg
		}
	}
	var cur [2]int64
	open := false
	for _, v := range iv {
		if open && v[0] <= cur[1] {
			cur[1] = max(cur[1], v[1])
			continue
		}
		if open {
			add(cur[0], cur[1])
		}
		cur, open = v, true
	}
	if open {
		add(cur[0], cur[1])
	}
	for i := range busy {
		span := min(bucketMS, end-(start+int64(i)*bucketMS))
		if span > 0 {
			frac[i] = float64(busy[i]) / float64(span)
		}
	}
	return frac, busyMS
}
