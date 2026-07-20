package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type probePayload struct {
	RunID   string `json:"runId"`
	Results []struct {
		Model        string `json:"model"`
		Probe        string `json:"probe"`
		Class        string `json:"class"`
		Capability   string `json:"capability"`
		RunMode      string `json:"runMode"`
		Stages       int    `json:"stages"`
		StagesPassed int    `json:"stagesPassed"`
		ChecksPassed int    `json:"checksPassed"`
		ChecksTotal  int    `json:"checksTotal"`
		Pass         bool   `json:"pass"`
		WallMS       int64  `json:"wallMs"`
		Skipped      bool   `json:"skipped"`
		SkipReason   string `json:"skipReason"`
		Note         string `json:"note"`
	} `json:"results"`
}

func capturePublish(t *testing.T, rows []Row, skips []Skip) probePayload {
	t.Helper()
	var got probePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/measurements/probes" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	c := newResidencyClient(cfgFor(srv.URL))
	if c == nil {
		t.Fatal("client should exist when a token is configured")
	}
	PublishProbeResults(context.Background(), c, "r1", rows, skips)
	return got
}

// Stage rows fold to one record per probe: the probe is the unit a reader
// reasons about, and per-stage records would put the drill-in back at the
// granularity nobody asked about.
func TestPublishProbeResults_FoldsStagesIntoProbes(t *testing.T) {
	rows := []Row{
		{Model: "m", Task: "p1", Class: "coding", Capability: "chat", Stage: 0, Pass: true, ChecksPassed: 2, ChecksTotal: 2},
		{Model: "m", Task: "p1", Class: "coding", Capability: "chat", Stage: 1, Pass: false, ChecksPassed: 1, ChecksTotal: 3, Note: "build failed"},
		{Model: "m", Task: "p2", Class: "coding", Capability: "chat", Stage: 0, Pass: true, ChecksPassed: 1, ChecksTotal: 1},
	}
	rows[0].WallMs, rows[1].WallMs, rows[2].WallMs = 100, 200, 50

	got := capturePublish(t, rows, nil)
	if got.RunID != "r1" || len(got.Results) != 2 {
		t.Fatalf("want 2 probe records, got %+v", got)
	}
	byProbe := map[string]int{}
	for i, r := range got.Results {
		byProbe[r.Probe] = i
	}
	p1 := got.Results[byProbe["p1"]]
	if p1.Stages != 2 || p1.StagesPassed != 1 || p1.Pass {
		t.Errorf("p1 should be 1/2 and not passing: %+v", p1)
	}
	if p1.ChecksTotal != 5 || p1.ChecksPassed != 3 || p1.WallMS != 300 {
		t.Errorf("p1 counters should sum across stages: %+v", p1)
	}
	// The first failing stage explains the probe; later ones fail downstream.
	if p1.Note != "build failed" {
		t.Errorf("want the first failing stage's note, got %q", p1.Note)
	}
	if p2 := got.Results[byProbe["p2"]]; !p2.Pass {
		t.Errorf("p2 passed every stage and should be marked passing: %+v", p2)
	}
}

// Cold and warm stay separate records — a disagreement between them is the
// finding, and folding them would report a probe that half-works as passing.
func TestPublishProbeResults_SplitsByRunMode(t *testing.T) {
	rows := []Row{
		{Model: "m", Task: "vision", Capability: "chat", RunMode: "cold", Pass: false},
		{Model: "m", Task: "vision", Capability: "chat", RunMode: "warm", Pass: true},
	}
	got := capturePublish(t, rows, nil)
	if len(got.Results) != 2 {
		t.Fatalf("want a record per run mode, got %+v", got.Results)
	}
	byMode := map[string]bool{}
	for _, r := range got.Results {
		byMode[r.RunMode] = r.Pass
	}
	if byMode["cold"] || !byMode["warm"] {
		t.Errorf("cold/warm verdicts must not be merged: %+v", byMode)
	}
}

// A skipped probe ships with zero counts and its reason, so the console can
// distinguish "not applicable" from "no data yet" — the ambiguity that made the
// aggregate score misleading.
func TestPublishProbeResults_IncludesSkipsWithZeroCounts(t *testing.T) {
	skips := []Skip{{
		Model: "stt", Task: "codex-plan-0", Class: "coding", Capability: "chat",
		Reason: `probe needs capability "chat", model serves "audio.stt"`,
	}}
	rows := []Row{{Model: "stt", Task: "capability-stt", Capability: "audio.stt", Pass: true, ChecksPassed: 1, ChecksTotal: 1}}

	got := capturePublish(t, rows, skips)
	if len(got.Results) != 2 {
		t.Fatalf("want the ran probe and the skipped one, got %+v", got.Results)
	}
	var skipped, ran int
	for _, r := range got.Results {
		if r.Skipped {
			skipped++
			if r.Stages != 0 || r.StagesPassed != 0 || r.Pass {
				t.Errorf("a skip must carry no score: %+v", r)
			}
			if r.SkipReason == "" || r.Capability != "chat" {
				t.Errorf("a skip must say which capability it belonged to and why: %+v", r)
			}
			continue
		}
		ran++
		if r.Capability != "audio.stt" {
			t.Errorf("ran probe kept the wrong capability: %+v", r)
		}
	}
	if skipped != 1 || ran != 1 {
		t.Errorf("want 1 skip and 1 ran, got %d/%d", skipped, ran)
	}
}

func TestPublishProbeResults_NoRowsNoRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	t.Setenv("TEST_ADMIN_TOKEN", "tok")
	PublishProbeResults(context.Background(), newResidencyClient(cfgFor(srv.URL)), "r1", nil, nil)
	if called {
		t.Error("an empty run must not POST an empty batch")
	}
	// A nil client (no admin token) must be a no-op, not a panic: plain quality
	// runs never configure one.
	PublishProbeResults(context.Background(), nil, "r1", []Row{{Model: "m", Task: "p"}}, nil)
}
