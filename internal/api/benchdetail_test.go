package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/corrallm/internal/store"
)

// The A/B bug: two arms of the same probe must not fold into one record, or the
// comparison the arms were run for is averaged away before anyone sees it.
func TestBenchProbesByCapability_KeepsArmsSeparate(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "m", Probe: "p", Capability: "chat", Toolset: "baseline",
			ToolFormat: "json", RunMode: "warm", Stages: 10, StagesPassed: 9},
		BenchProbePublish{Model: "m", Probe: "p", Capability: "chat", Toolset: "baseline",
			ToolFormat: "toon", RunMode: "warm", Stages: 10, StagesPassed: 6},
	)
	out, err := h.BenchProbesByCapability(ctx, &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	probes := out.Body.Capabilities[0].Probes
	if len(probes) != 1 {
		t.Fatalf("both arms belong to ONE probe: %+v", probes)
	}
	if len(probes[0].Arms) != 2 {
		t.Fatalf("want 2 arms, got %+v", probes[0].Arms)
	}
	// Headline score is the baseline arm's alone, not the 15/20 pooled average.
	if probes[0].Score != 0.9 {
		t.Errorf("probe score should come from the baseline arm: %v", probes[0].Score)
	}
	var toon BenchArmView
	for _, a := range probes[0].Arms {
		if a.ToolFormat == "toon" {
			toon = a
		}
	}
	if toon.IsBaseline {
		t.Error("json should outrank toon as baseline")
	}
	if d := toon.ScoreDelta; d > -0.29 || d < -0.31 {
		t.Errorf("toon delta should be about -0.3, got %v", d)
	}
	// The capability score must also be the baseline's, so adding an arm to a
	// run cannot move a model's headline number.
	if out.Body.Capabilities[0].Score != 0.9 {
		t.Errorf("capability score should not pool arms: %v", out.Body.Capabilities[0].Score)
	}
}

func TestBenchProbesByCapability_FlagsArmDisagreement(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "m", Probe: "vision", Capability: "chat", RunMode: "warm",
			Stages: 1, StagesPassed: 1, Pass: true},
		BenchProbePublish{Model: "m", Probe: "vision", Capability: "chat", RunMode: "cold",
			Stages: 1, StagesPassed: 0},
	)
	out, err := h.BenchProbesByCapability(ctx, &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p := out.Body.Capabilities[0].Probes[0]
	if !p.Disagreement {
		t.Error("warm passing while cold fails is the finding; it must be flagged")
	}
	// Warm is the baseline, so the headline reads as passing — the cold failure
	// shows up as a delta rather than silently halving the score.
	if !p.Pass {
		t.Errorf("baseline (warm) passed: %+v", p)
	}
}

func TestPickBaseline_PrefersWarmBaselineJSON(t *testing.T) {
	arms := []armKey{
		{"exp", "toon", "cold"},
		{"baseline", "json", "warm"},
		{"baseline", "json", "cold"},
	}
	if got := pickBaseline(arms); got != (armKey{"baseline", "json", "warm"}) {
		t.Errorf("got %+v", got)
	}
}

// With no preferred arm present the choice must still be deterministic: an
// unstable baseline silently redefines the score between requests.
func TestPickBaseline_DeterministicWithoutPreferredArm(t *testing.T) {
	arms := []armKey{{"zeta", "toon", "cold"}, {"alpha", "tight", "cold"}}
	first := pickBaseline(arms)
	for i := 0; i < 20; i++ {
		if got := pickBaseline([]armKey{{"alpha", "tight", "cold"}, {"zeta", "toon", "cold"}}); got != first {
			t.Fatalf("baseline is order-dependent: %+v vs %+v", got, first)
		}
	}
}

func TestBenchProbeDetail_ReturnsStagesAndChecks(t *testing.T) {
	h, ctx := probeHandlers(t)
	in := &BenchProbeResultsInput{}
	in.Body.RunID = "r1"
	in.Body.At = 100
	in.Body.Results = []BenchProbePublish{{Model: "m", Probe: "p", Capability: "chat", Stages: 2, StagesPassed: 1}}
	in.Body.Stages = []BenchStagePublish{
		{Model: "m", Probe: "p", Stage: 0, Prompt: "do the thing", Pass: true, Turns: 3, ToolCalls: 5},
		{Model: "m", Probe: "p", Stage: 1, Prompt: "now verify", Note: "build failed", BaitCalls: 1},
	}
	in.Body.Checks = []BenchCheckPublish{
		{Model: "m", Probe: "p", Stage: 1, Idx: 0, Kind: "cmd_ok", Desc: "go build", Detail: "exit 2"},
		{Model: "m", Probe: "p", Stage: 1, Idx: 1, Kind: "file_contains", Desc: "answers.txt", Pass: true},
	}
	if _, err := h.PublishBenchProbeResults(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchProbeDetail(ctx, &BenchProbeDetailInput{RunID: "r1", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Arms) != 1 || len(out.Body.Arms[0].Stages) != 2 {
		t.Fatalf("want one arm with two stages: %+v", out.Body.Arms)
	}
	s1 := out.Body.Arms[0].Stages[1]
	if s1.Note != "build failed" || s1.BaitCalls != 1 {
		t.Errorf("stage metrics lost: %+v", s1)
	}
	if len(s1.Checks) != 2 {
		t.Fatalf("want both checks on stage 1: %+v", s1.Checks)
	}
	// The failure detail is the whole point of the drill-in.
	if s1.Checks[0].Detail != "exit 2" || s1.Checks[0].Pass {
		t.Errorf("failing check detail lost: %+v", s1.Checks[0])
	}
	if s0 := out.Body.Arms[0].Stages[0]; s0.Turns != 3 || s0.Prompt != "do the thing" {
		t.Errorf("stage 0 detail lost: %+v", s0)
	}
}

func TestBenchTranscript_ReadsRecordedArtifacts(t *testing.T) {
	h, ctx := probeHandlers(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ComboName sanitizes to model_toolset_probe.
	body := `{"kind":"user","content":"hi","createdAt":1}
{"kind":"tool_call","toolName":"read_file","content":"{\"path\":\"README.md\"}","createdAt":2}
`
	if err := os.WriteFile(filepath.Join(dir, "transcripts", "m_baseline_p.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if err := h.Store.SaveBenchRun(ctx, store.BenchRun{RunID: "r1", OutDir: dir, Host: host, At: 1}); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchTranscript(ctx, &BenchArtifactInput{RunID: "r1", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.Available || len(out.Body.Entries) != 2 {
		t.Fatalf("want 2 entries, got %+v (%s)", out.Body.Entries, out.Body.Reason)
	}
	if out.Body.Entries[1].ToolName != "read_file" {
		t.Errorf("tool call detail lost: %+v", out.Body.Entries[1])
	}
}

// An unreadable artifact must say why. An empty transcript with no reason reads
// as "the model said nothing" instead of "this box cannot see that file".
func TestBenchTranscript_ExplainsWhyUnavailable(t *testing.T) {
	h, ctx := probeHandlers(t)
	out, err := h.BenchTranscript(ctx, &BenchArtifactInput{RunID: "nope", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Available || out.Body.Reason == "" {
		t.Errorf("want an explicit reason: %+v", out.Body)
	}
	if err := h.Store.SaveBenchRun(ctx, store.BenchRun{
		RunID: "elsewhere", OutDir: "/tmp/whatever", Host: "some-other-box", At: 1,
	}); err != nil {
		t.Fatal(err)
	}
	out, err = h.BenchTranscript(ctx, &BenchArtifactInput{RunID: "elsewhere", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Available || out.Body.Reason == "" {
		t.Fatalf("a run from another host must say so: %+v", out.Body)
	}
}

// The filename is built server-side from the model/toolset/probe names. A name
// carrying traversal must not escape the run directory.
func TestBenchArtifact_RejectsTraversalInNames(t *testing.T) {
	h, ctx := probeHandlers(t)
	dir := t.TempDir()
	host, _ := os.Hostname()
	if err := h.Store.SaveBenchRun(ctx, store.BenchRun{RunID: "r1", OutDir: dir, Host: host, At: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.jsonl"), []byte(`{"kind":"user"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchTranscript(ctx, &BenchArtifactInput{
		RunID: "r1", Model: "../secret", Probe: "p", Toolset: "..",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Available {
		t.Fatalf("traversal must not resolve to a readable file: %+v", out.Body)
	}
}

func TestBenchJournal_ReadsToolCalls(t *testing.T) {
	h, ctx := probeHandlers(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "journals"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"ts":1,"tool":"read_file","args":{"path":"README.md"},"resultBytes":484,"poisoned":true}
{"ts":2,"tool":"exfiltrate","args":{},"bait":true}
`
	if err := os.WriteFile(filepath.Join(dir, "journals", "m_baseline_p.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if err := h.Store.SaveBenchRun(ctx, store.BenchRun{RunID: "r1", OutDir: dir, Host: host, At: 1}); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchJournal(ctx, &BenchArtifactInput{RunID: "r1", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.Available || len(out.Body.Entries) != 2 {
		t.Fatalf("want 2 entries: %+v (%s)", out.Body.Entries, out.Body.Reason)
	}
	// Poisoned and bait are what adversarial probes are actually scored on.
	if !out.Body.Entries[0].Poisoned || !out.Body.Entries[1].Bait {
		t.Errorf("adversarial flags lost: %+v", out.Body.Entries)
	}
	if out.Body.Entries[0].Args == "" {
		t.Error("tool args should survive as raw JSON")
	}
}

func TestBenchArtifacts_NoStoreIsEmptyNotNil(t *testing.T) {
	h := &Handlers{}
	tr, err := h.BenchTranscript(context.Background(), &BenchArtifactInput{RunID: "r", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Body.Entries == nil {
		t.Error("entries must serialize as [] not null")
	}
	jr, err := h.BenchJournal(context.Background(), &BenchArtifactInput{RunID: "r", Model: "m", Probe: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if jr.Body.Entries == nil {
		t.Error("entries must serialize as [] not null")
	}
}

// The cross-model question ("does toon help at all") is not the per-model one,
// and reading it off unpaired data is how an arm that only ever ran against
// strong models looks like an improvement it never made.
func TestBenchArmMatrix_PairsArmAgainstItsOwnBaseline(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		// a: toon beats json by 20 points.
		BenchProbePublish{Model: "a", Probe: "p1", Capability: "chat", Toolset: "baseline",
			ToolFormat: "json", RunMode: "warm", Stages: 10, StagesPassed: 6, NewPromptTokens: 1000},
		BenchProbePublish{Model: "a", Probe: "p1", Capability: "chat", Toolset: "baseline",
			ToolFormat: "toon", RunMode: "warm", Stages: 10, StagesPassed: 8, NewPromptTokens: 700},
		// b: toon loses by 10 points.
		BenchProbePublish{Model: "b", Probe: "p1", Capability: "chat", Toolset: "baseline",
			ToolFormat: "json", RunMode: "warm", Stages: 10, StagesPassed: 9, NewPromptTokens: 1000},
		BenchProbePublish{Model: "b", Probe: "p1", Capability: "chat", Toolset: "baseline",
			ToolFormat: "toon", RunMode: "warm", Stages: 10, StagesPassed: 8, NewPromptTokens: 800},
	)
	out, err := h.BenchArmMatrix(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Capabilities) != 1 {
		t.Fatalf("want one capability: %+v", out.Body.Capabilities)
	}
	arms := out.Body.Capabilities[0].Arms
	if len(arms) != 1 || arms[0].ToolFormat != "toon" {
		t.Fatalf("only the non-baseline arm is a comparison: %+v", arms)
	}
	a := arms[0]
	if a.Models != 2 || a.Probes != 2 {
		t.Errorf("paired sample sizes wrong: %+v", a)
	}
	// (+0.2 and -0.1) / 2 = +0.05
	if a.MeanScoreDelta < 0.049 || a.MeanScoreDelta > 0.051 {
		t.Errorf("mean delta should be +0.05, got %v", a.MeanScoreDelta)
	}
	if a.Wins != 1 || a.Losses != 1 {
		t.Errorf("want 1 win 1 loss: %+v", a)
	}
	// toon is cheaper for both: (-300 + -200)/2 = -250
	if a.MeanTokenDelta != -250 {
		t.Errorf("want -250 mean token delta, got %d", a.MeanTokenDelta)
	}
	if len(a.ByModel) != 2 || a.ByModel[0].Model != "a" {
		t.Fatalf("per-model rollup wrong: %+v", a.ByModel)
	}
	if got := a.ByModel[0].BaselineScore; got != 0.6 {
		t.Errorf("baseline score should be filled: %v", got)
	}
	if got := a.ByModel[0].ArmScore; got != 0.8 {
		t.Errorf("arm score should be filled: %v", got)
	}
}

// An arm on a probe whose baseline never ran has nothing to be measured
// against. Crediting it anyway is exactly the selection bias pairing prevents.
func TestBenchArmMatrix_IgnoresProbesWithNoBaseline(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "a", Probe: "paired", Capability: "chat", ToolFormat: "json",
			RunMode: "warm", Stages: 10, StagesPassed: 5},
		BenchProbePublish{Model: "a", Probe: "paired", Capability: "chat", ToolFormat: "toon",
			RunMode: "warm", Stages: 10, StagesPassed: 7},
		// Only toon ran here — it becomes its OWN baseline and must not be
		// counted as a win for toon.
		BenchProbePublish{Model: "a", Probe: "solo", Capability: "chat", ToolFormat: "toon",
			RunMode: "warm", Stages: 10, StagesPassed: 10},
	)
	out, err := h.BenchArmMatrix(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := out.Body.Capabilities[0].Arms[0]
	if a.Probes != 1 {
		t.Fatalf("only the paired probe counts, got %d: %+v", a.Probes, a)
	}
	if a.MeanScoreDelta < 0.19 || a.MeanScoreDelta > 0.21 {
		t.Errorf("unpaired probe must not inflate the delta: %v", a.MeanScoreDelta)
	}
}

// The median exists so one pathological probe cannot carry a verdict the rest
// of the evidence does not support.
func TestBenchArmMatrix_MedianResistsOneOutlier(t *testing.T) {
	h, ctx := probeHandlers(t)
	recs := []BenchProbePublish{}
	for i, passed := range []int{5, 5, 5} {
		probe := string(rune('a' + i))
		recs = append(recs,
			BenchProbePublish{Model: "m", Probe: probe, Capability: "chat", ToolFormat: "json",
				RunMode: "warm", Stages: 10, StagesPassed: passed},
			BenchProbePublish{Model: "m", Probe: probe, Capability: "chat", ToolFormat: "toon",
				RunMode: "warm", Stages: 10, StagesPassed: passed},
		)
	}
	// One probe where toon runs away with it.
	recs = append(recs,
		BenchProbePublish{Model: "m", Probe: "outlier", Capability: "chat", ToolFormat: "json",
			RunMode: "warm", Stages: 10, StagesPassed: 0},
		BenchProbePublish{Model: "m", Probe: "outlier", Capability: "chat", ToolFormat: "toon",
			RunMode: "warm", Stages: 10, StagesPassed: 10},
	)
	publish(t, h, ctx, "r1", recs...)
	a := (func() BenchArmComparisonView {
		out, err := h.BenchArmMatrix(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out.Body.Capabilities[0].Arms[0]
	})()
	if a.MeanScoreDelta <= a.MedianScoreDelta {
		t.Errorf("the outlier should pull the mean above the median: mean=%v median=%v",
			a.MeanScoreDelta, a.MedianScoreDelta)
	}
	if a.MedianScoreDelta != 0 {
		t.Errorf("three tied probes should hold the median at 0, got %v", a.MedianScoreDelta)
	}
}

func TestBenchArmMatrix_NoStoreIsEmptyNotNil(t *testing.T) {
	out, err := (&Handlers{}).BenchArmMatrix(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Capabilities == nil {
		t.Error("capabilities must serialize as [] not null")
	}
}
