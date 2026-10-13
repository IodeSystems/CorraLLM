// Package power reads measured watts from Prometheus.
package power

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Series is one Prometheus range series: its labels and its samples.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// Sample is one value at one instant, unix milliseconds.
type Sample struct {
	TS int64
	V  float64
}

var client = &http.Client{Timeout: 10 * time.Second}

// QueryRange runs a PromQL range query from..to at step and returns its series.
func QueryRange(ctx context.Context, base, query string, from, to time.Time, step time.Duration) ([]Series, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(from.Unix(), 10))
	q.Set("end", strconv.FormatInt(to.Unix(), 10))
	q.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus %s: %w", resp.Status, err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s %s", resp.Status, body.Error)
	}
	out := make([]Series, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s := Series{Labels: r.Metric}
		for _, v := range r.Values {
			ts, ok := v[0].(float64)
			str, ok2 := v[1].(string)
			if !ok || !ok2 {
				continue
			}
			f, err := strconv.ParseFloat(str, 64)
			if err != nil {
				continue
			}
			s.Samples = append(s.Samples, Sample{TS: int64(ts * 1000), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}

// SameCard reports whether a Prometheus uuid label names the card a pool's
// device selector does: a GPU UUID or a unique prefix of one, with or without
// nvidia-smi's "GPU-" prefix. A PCI bus id selector never matches.
func SameCard(selector, uuid string) bool {
	norm := func(s string) string { return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "gpu-") }
	sel, id := norm(selector), norm(uuid)
	return sel != "" && id != "" && strings.HasPrefix(id, sel)
}
