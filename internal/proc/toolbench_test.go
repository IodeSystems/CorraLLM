package proc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Best-of: the first long request after a load pays one-time costs, so a
// single slow run must not be what the build is judged on.
func TestBenchRunsKeepsTheBestRateAndSendsGreedyUncached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/completion" {
			t.Errorf("path %q", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["cache_prompt"] != false || body["temperature"] != float64(0) || body["ignore_eos"] != true {
			t.Errorf("request is not greedy+uncached+fixed-length: %v", body)
		}
		calls++
		tp, tg := []float64{0, 900, 2000}[min(calls-1, 2)], []float64{0, 81, 79}[min(calls-1, 2)]
		_, _ = w.Write([]byte(`{"timings":{"prompt_per_second":` + jsonNum(tp) + `,"predicted_per_second":` + jsonNum(tg) + `}}`))
	}))
	defer srv.Close()

	got, err := benchRuns(context.Background(), srv.URL, benchPrompt(1000))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("%d requests, want a warm-up and two measured", calls)
	}
	if got.Prefill != 2000 || got.Decode != 81 {
		t.Errorf("got %+v, want the best of each rate (2000, 81)", got)
	}
}

func TestBenchRunsRefusesABackendWithoutTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"hi"}`))
	}))
	defer srv.Close()
	if _, err := benchRuns(context.Background(), srv.URL, "x"); err == nil || !strings.Contains(err.Error(), "timings") {
		t.Errorf("err = %v", err)
	}
}

func TestBenchPromptScalesWithTheRequestedSize(t *testing.T) {
	small, big := benchPrompt(1000), benchPrompt(16000)
	if n := strings.Count(big, "Section "); n != 160 {
		t.Errorf("16000 tokens -> %d sections, want 160 (~100 tokens each)", n)
	}
	if len(big) < 15*len(small) {
		t.Errorf("prompt does not scale: %d vs %d bytes", len(big), len(small))
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

// An evicted process leaves the table at once but holds its port until it
// exits. The benchmark must wait for the exit, or it health-checks the dying
// production process (box1, 2026-10-07).
func TestFullyStoppedWaitsForTheExit(t *testing.T) {
	mgr := NewManager(deadPortModel(t))
	defer mgr.Shutdown()
	if !mgr.fullyStopped("A") {
		t.Fatal("a model with no process reads as running")
	}
	mgr.mu.Lock()
	mgr.stopping = map[string]chan struct{}{"A": make(chan struct{})}
	mgr.mu.Unlock()
	if mgr.StateOf("A") != StateAbsent {
		t.Fatal("setup: StateOf should already say absent — that is the trap")
	}
	if mgr.fullyStopped("A") {
		t.Error("fullyStopped while the old process is still exiting")
	}
}
