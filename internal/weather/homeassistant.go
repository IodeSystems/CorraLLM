package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FetchHAHourly reads a Home Assistant temperature sensor's history between
// from and to and returns its TIME-WEIGHTED mean per UTC hour, in °F.
//
// Home Assistant records state CHANGES, not a regular series: a value holds
// until the next change. So each hour's figure weights every value by how long
// it held inside that hour — a reading that sat at 76°F for 50 minutes counts
// five times a 10-minute blip at 78°F. "unavailable"/"unknown" states end the
// previous value without contributing one. A sensor reporting °C is converted.
//
// baseURL is the instance root (https://ha.example.com); token is a long-lived
// access token.
func FetchHAHourly(ctx context.Context, client *http.Client, baseURL, token, entity string, from, to time.Time) ([]Hour, error) {
	u := strings.TrimRight(baseURL, "/") + "/api/history/period/" + url.PathEscape(from.UTC().Format(time.RFC3339)) +
		"?" + url.Values{"filter_entity_id": {entity}, "end_time": {to.UTC().Format(time.RFC3339)}, "minimal_response": {""}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", UserAgent)
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("home assistant: HTTP %d for %s", resp.StatusCode, entity)
	}
	var series [][]struct {
		State       string    `json:"state"`
		LastChanged time.Time `json:"last_changed"`
		Attributes  struct {
			Unit string `json:"unit_of_measurement"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		return nil, fmt.Errorf("home assistant: %w", err)
	}
	if len(series) == 0 || len(series[0]) == 0 {
		return nil, nil
	}
	celsius := strings.Contains(series[0][0].Attributes.Unit, "C")

	type point struct {
		at time.Time
		v  float64
		ok bool
	}
	pts := make([]point, 0, len(series[0]))
	for _, s := range series[0] {
		v, err := strconv.ParseFloat(s.State, 64)
		if err == nil && celsius {
			v = v*9/5 + 32
		}
		pts = append(pts, point{at: s.LastChanged, v: v, ok: err == nil})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })

	sum, dur := map[int64]float64{}, map[int64]float64{}
	for i, p := range pts {
		if !p.ok {
			continue
		}
		start, end := p.at, to
		if i+1 < len(pts) {
			end = pts[i+1].at
		}
		if start.Before(from) {
			start = from
		}
		for start.Before(end) {
			hour := start.Truncate(time.Hour)
			next := hour.Add(time.Hour)
			if next.After(end) {
				next = end
			}
			secs := next.Sub(start).Seconds()
			key := hour.UnixMilli()
			sum[key] += p.v * secs
			dur[key] += secs
			start = next
		}
	}
	out := make([]Hour, 0, len(sum))
	for h, s := range sum {
		if dur[h] > 0 {
			out = append(out, Hour{HourMS: h, TempF: s / dur[h]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HourMS < out[j].HourMS })
	return out, nil
}
