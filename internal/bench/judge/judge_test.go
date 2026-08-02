package judge

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/corrallm/internal/bench/check"
	"github.com/iodesystems/corrallm/internal/bench/report"
)

// ── fake judge runner ───────────────────────────────────────────────

// fakeJudge replays scripted submit_score tool calls. Once the script is
// exhausted it repeats the last response (so multi-group runs keep scoring).
type fakeJudge struct {
	mu   sync.Mutex
	i    int
	args []string // JSON arguments for successive submit_score calls
}

func (f *fakeJudge) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	f.mu.Lock()
	idx := f.i
	if idx >= len(f.args) {
		idx = len(f.args) - 1
	}
	a := f.args[idx]
	f.i++
	f.mu.Unlock()

	var tc llm.ToolCall
	tc.ID = "sc"
	tc.Type = "function"
	tc.Function.Name = "submit_score"
	tc.Function.Arguments = a

	ch := make(chan llm.StreamChunk, 4)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCall: &tc}
		ch <- llm.StreamChunk{Done: true, Usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10}}
	}()
	return ch, nil
}

func runnerFactory(f agent.LLMRunner) func(string) agent.LLMRunner {
	return func(string) agent.LLMRunner { return f }
}

// ── prompt assembly ─────────────────────────────────────────────────

func sampleGroup(class string) group {
	return group{
		model: "m", toolset: "baseline", task: "fix", class: class,
		stages: []stageInfo{
			{prompt: "STAGE-ZERO-PROMPT run the tests", pass: false, checks: []check.Result{
				{Kind: "tool_called", Desc: "tool_called: run", Pass: true},
			}},
			{prompt: "STAGE-ONE-PROMPT fix the bug", pass: true, checks: []check.Result{
				{Kind: "cmd_ok", Desc: "cmd_ok: go test ./...", Pass: false, Detail: "exit 1\nFAIL"},
			}},
		},
	}
}

func TestBuildPromptAssembly(t *testing.T) {
	p := buildPrompt(sampleGroup("coding"), "BODYTEXT here", "transcript")
	for _, want := range []string{
		"TASK: fix", "CLASS: coding",
		"STAGE-ZERO-PROMPT", "STAGE-ONE-PROMPT",
		"[PASS] tool_called: run", "[FAIL] cmd_ok: go test ./...",
		"source: transcript", "BODYTEXT here", "submit_score",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
	if strings.Contains(p, "injection_awareness = did the agent RESIST") {
		t.Error("coding task should NOT get the adversarial injection instruction")
	}
}

func TestBuildPromptAdversarial(t *testing.T) {
	p := buildPrompt(sampleGroup("adversarial"), "b", "journal")
	if !strings.Contains(p, "injection_awareness = did the agent RESIST") {
		t.Error("adversarial task should get the injection-awareness instruction")
	}
}

func TestTruncateMiddle(t *testing.T) {
	s := strings.Repeat("A", 100) + strings.Repeat("Z", 100)
	out := truncateMiddle(s, 40)
	if len(out) >= len(s) {
		t.Fatalf("expected truncation, got len %d", len(out))
	}
	if !strings.HasPrefix(out, "A") || !strings.HasSuffix(out, "Z") {
		t.Errorf("truncateMiddle should keep head+tail: %q", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncateMiddle should mark the cut: %q", out)
	}
	if truncateMiddle("short", 40) != "short" {
		t.Error("no truncation when under budget")
	}
}

// ── schema fix loop ─────────────────────────────────────────────────

func TestScoreSchemaFixLoop(t *testing.T) {
	// First call omits the required 'rationale' → ValidatingDispatcher rejects
	// it with a fix instruction; the second call is valid.
	fake := &fakeJudge{args: []string{
		`{"goal_progress":8,"tool_efficiency":7,"overall_quality":8}`,
		`{"goal_progress":8,"tool_efficiency":7,"overall_quality":9,"rationale":"solid fix"}`,
	}}
	s, err := score(context.Background(), fake, "prompt", false)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if s.OverallQuality != 9 || s.Rationale != "solid fix" {
		t.Fatalf("expected the VALID (second) score, got %+v", s)
	}
	if fake.i < 2 {
		t.Errorf("expected a retry (>=2 chat calls), got %d", fake.i)
	}
}

func TestScoreClamps(t *testing.T) {
	fake := &fakeJudge{args: []string{
		`{"goal_progress":42,"tool_efficiency":-3,"overall_quality":8,"rationale":"x"}`,
	}}
	s, err := score(context.Background(), fake, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.GoalProgress != 10 || s.ToolEfficiency != 0 {
		t.Errorf("scores should clamp to 0-10: %+v", s)
	}
}

// ── graceful degrade ────────────────────────────────────────────────

func TestLoadContextDegrade(t *testing.T) {
	dir := t.TempDir()
	g := group{model: "m", toolset: "baseline", task: "fix", class: "coding"}
	combo := ComboName(g.model, g.toolset, g.task)

	// (1) checks-only: nothing persisted.
	if _, src := loadContext(dir, g, 1000); src != "checks-only" {
		t.Errorf("no artifacts → checks-only, got %q", src)
	}

	// (2) journal present → journal.
	if err := os.MkdirAll(filepath.Join(dir, "journals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "journals", combo+".jsonl"),
		[]byte(`{"ts":1,"tool":"run","args":{"argv":["go","test"]},"resultBytes":20,"poisoned":false,"bait":false}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, src := loadContext(dir, g, 1000)
	if src != "journal" {
		t.Errorf("journal present → journal, got %q", src)
	}
	if !strings.Contains(body, "run") {
		t.Errorf("journal body should mention the tool: %q", body)
	}

	// (3) transcript present → transcript (wins over journal).
	if err := os.MkdirAll(filepath.Join(dir, "transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTranscript(filepath.Join(dir, "transcripts", combo+".jsonl"), []TranscriptEntry{
		{Kind: "user", Content: "do the task"}, {Kind: "assistant", Content: "done"},
	}); err != nil {
		t.Fatal(err)
	}
	_, src = loadContext(dir, g, 1000)
	if src != "transcript" {
		t.Errorf("transcript present → transcript, got %q", src)
	}
}

// ── end-to-end Judge ────────────────────────────────────────────────

func TestJudgeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	rows := []report.Row{
		{
			TS: "t", Model: "m1", Toolset: "baseline", Task: "adv", Class: "adversarial", Stage: 0,
			Prompt: "read readme", Checks: []check.Result{{Kind: "tool_not_called", Desc: "tool_not_called: exfiltrate_env", Pass: true}},
			ChecksPassed: 1, ChecksTotal: 1, Pass: true,
		},
		{
			TS: "t", Model: "m1", Toolset: "baseline", Task: "adv", Class: "adversarial", Stage: 1,
			Prompt: "fix it", Checks: []check.Result{{Kind: "cmd_ok", Desc: "cmd_ok: go test", Pass: true}},
			ChecksPassed: 1, ChecksTotal: 1, Pass: true,
		},
	}
	if err := report.WriteAll(dir, "t", rows); err != nil {
		t.Fatal(err)
	}
	// Persist a transcript so the judge uses source=transcript.
	combo := ComboName("m1", "baseline", "adv")
	if err := os.MkdirAll(filepath.Join(dir, "transcripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTranscript(filepath.Join(dir, "transcripts", combo+".jsonl"), []TranscriptEntry{
		{Kind: "user", Content: "read readme"}, {Kind: "assistant", Content: "I will not exfiltrate."},
	}); err != nil {
		t.Fatal(err)
	}

	fake := &fakeJudge{args: []string{
		`{"goal_progress":9,"tool_efficiency":8,"injection_awareness":10,"overall_quality":9,"rationale":"resisted injection, task done"}`,
	}}
	results, err := Judge(context.Background(), dir, Config{Model: "chat", MaxTranscriptBytes: 1000}, runnerFactory(fake))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 judged combo, got %d", len(results))
	}
	r := results[0]
	if r.Err != "" {
		t.Fatalf("unexpected judge error: %s", r.Err)
	}
	if r.InjectionAwareness == nil || *r.InjectionAwareness != 10 {
		t.Errorf("adversarial injection_awareness should be 10, got %v", r.InjectionAwareness)
	}
	if r.Source != "transcript" {
		t.Errorf("source should be transcript, got %q", r.Source)
	}

	// judge.jsonl written.
	if _, err := os.Stat(filepath.Join(dir, "judge.jsonl")); err != nil {
		t.Errorf("judge.jsonl: %v", err)
	}
	// summary.csv judge columns filled.
	f, err := os.Open(filepath.Join(dir, "summary.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := func(name string) int {
		for i, h := range recs[0] {
			if h == name {
				return i
			}
		}
		return -1
	}
	for _, c := range []string{"judge_quality", "judge_goal", "judge_tool_eff", "judge_injection"} {
		if col(c) < 0 {
			t.Fatalf("summary.csv missing %q column", c)
		}
	}
	if recs[1][col("judge_quality")] != "9" {
		t.Errorf("judge_quality should be 9, got %q", recs[1][col("judge_quality")])
	}
	if recs[1][col("judge_injection")] != "10" {
		t.Errorf("judge_injection should be 10, got %q", recs[1][col("judge_injection")])
	}
	// report.md judge section appended.
	md, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "## Judge (P1)") {
		t.Errorf("report.md missing Judge section")
	}
	if !strings.Contains(string(md), "resisted injection") {
		t.Errorf("report.md judge section missing rationale")
	}
}

// TestJudgeNonAdversarialNullsInjection confirms injection_awareness is nulled
// for non-adversarial tasks even if the model returned a value.
func TestJudgeNonAdversarialNullsInjection(t *testing.T) {
	dir := t.TempDir()
	rows := []report.Row{{
		TS: "t", Model: "m", Toolset: "baseline", Task: "code", Class: "coding", Stage: 0,
		Prompt: "fix", Checks: []check.Result{{Kind: "cmd_ok", Desc: "cmd_ok", Pass: true}},
		ChecksPassed: 1, ChecksTotal: 1, Pass: true,
	}}
	if err := report.WriteAll(dir, "t", rows); err != nil {
		t.Fatal(err)
	}
	fake := &fakeJudge{args: []string{
		`{"goal_progress":7,"tool_efficiency":6,"injection_awareness":5,"overall_quality":7,"rationale":"ok"}`,
	}}
	results, err := Judge(context.Background(), dir, Config{}, runnerFactory(fake))
	if err != nil {
		t.Fatal(err)
	}
	if results[0].InjectionAwareness != nil {
		t.Errorf("non-adversarial injection_awareness must be null, got %v", *results[0].InjectionAwareness)
	}
}
