package store

import (
	"context"
	"testing"
)

func TestBenchRollupSumsOnlyTheRunsBenchTraffic(t *testing.T) {
	s := openTestStore(t)
	add := func(ts int64, served, key string, status, prompt, cached, compl int, pps, gps float64, think int) {
		t.Helper()
		if err := s.InsertActivity(Activity{TS: ts, Served: served, Key: key, Status: status,
			PromptTokens: prompt, CachedTokens: cached, CompletionTokens: compl,
			PromptPerSec: pps, PredictedPerSec: gps,
			ReasoningTokens: think, ReasoningMeasured: think >= 0}); err != nil {
			t.Fatal(err)
		}
	}
	add(1000, "m", "bench-a", 200, 1000, 600, 100, 400, 50, 40)   // prefill 1 s, gen 2 s
	add(2000, "m", "bench-a", 200, 3000, 2200, 300, 800, 100, -1) // prefill 1 s, gen 3 s
	add(2500, "m", "bench-a", 429, 0, 0, 0, 0, 0, -1)
	add(2600, "m", "sk-user", 200, 9999, 0, 9999, 1, 1, 9999) // not the bench
	add(2700, "other", "bench-a", 200, 9999, 0, 9999, 1, 1, 9999)
	add(9000, "m", "bench-b", 200, 9999, 0, 9999, 1, 1, 9999) // after the window

	r, err := s.BenchRollupFor(context.Background(), "m", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Requests != 3 || r.Turns != 2 || r.Refused != 1 {
		t.Errorf("counts = %d/%d/%d", r.Requests, r.Turns, r.Refused)
	}
	if r.Prompt != 4000 || r.Cached != 2800 || r.Generated != 400 {
		t.Errorf("tokens = %d/%d/%d", r.Prompt, r.Cached, r.Generated)
	}
	if r.Thinking != 40 || r.ThinkingRows != 1 {
		t.Errorf("thinking = %d over %d rows", r.Thinking, r.ThinkingRows)
	}
	if r.PeakCtx != 3300 {
		t.Errorf("peak = %d", r.PeakCtx)
	}
	if r.PrefillS != 2 || r.PrefillNew != 1200 || r.PrefillAll != 4000 || r.GenS != 5 || r.GenTokens != 400 {
		t.Errorf("speeds = %+v", r)
	}
}
