package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/corrallm/internal/bench/judge"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

// scriptedResp is one fake model turn: optional content and/or tool calls.
type scriptedResp struct {
	content string
	calls   []llm.ToolCall
}

// fakeRunner is an agent.LLMRunner that replays a fixed script — no network,
// no live model. Each ChatStream call consumes the next scripted response.
type fakeRunner struct {
	mu   sync.Mutex
	i    int
	resp []scriptedResp
}

func (f *fakeRunner) ChatStream(ctx context.Context, _ []llm.Message, tools []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	// Mirror a real backend: a canceled stage context fails the in-flight (or
	// next) request. This is how the loop-breaker / budget-cancel abort
	// actually terminates a Turn — cancel() alone doesn't stop anything until
	// the runner observes it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The Shaper's summarize() calls the runner with tools==nil. Answer those
	// with a canned summary WITHOUT advancing the main script.
	if len(tools) == 0 {
		ch := make(chan llm.StreamChunk, 4)
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Content: "SUMMARY: earlier files were surveyed."}
			ch <- llm.StreamChunk{Done: true, Usage: &llm.Usage{PromptTokens: 3, CompletionTokens: 3, TotalTokens: 6}}
		}()
		return ch, nil
	}

	f.mu.Lock()
	var r scriptedResp
	if f.i < len(f.resp) {
		r = f.resp[f.i]
	}
	f.i++
	f.mu.Unlock()

	ch := make(chan llm.StreamChunk, 8)
	go func() {
		defer close(ch)
		if r.content != "" {
			ch <- llm.StreamChunk{Content: r.content}
		}
		for i := range r.calls {
			tc := r.calls[i]
			ch <- llm.StreamChunk{ToolCall: &tc}
		}
		ch <- llm.StreamChunk{Done: true, Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	}()
	return ch, nil
}

func toolCall(id, name string, args any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return rawToolCall(id, name, string(b))
}

// rawToolCall builds a call with verbatim (possibly malformed) argument text —
// used to exercise the malformed-JSON (jsonErrors) path.
func rawToolCall(id, name, rawArgs string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = rawArgs
	return tc
}

const fixedMathx = `package mathx

func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
`

// buildMcp compiles llm-bench-mcp to a temp binary (offline).
func buildMcp(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "llm-bench-mcp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/iodesystems/corrallm/cmd/llm-bench-mcp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build llm-bench-mcp: %v\n%s", err, out)
	}
	return bin
}

// writeSmokeTask lays down a temp tasks dir with one buggy Go module task.
func writeSmokeTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tdir := filepath.Join(root, "smoke")
	fx := filepath.Join(tdir, "fixture")
	if err := os.MkdirAll(fx, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(tdir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fixture/go.mod", "module mathx\n\ngo 1.21\n")
	write("fixture/mathx.go", "package mathx\n\nfunc Max(a, b int) int {\n\tif a < b {\n\t\treturn a\n\t}\n\treturn b\n}\n")
	write("fixture/mathx_test.go", "package mathx\n\nimport \"testing\"\n\nfunc TestMax(t *testing.T) {\n\tif Max(3, 7) != 7 {\n\t\tt.Fatal(\"bug\")\n\t}\n}\n")
	write("task.yaml", `name: smoke
class: coding
workspace: fixture/
limits: { maxTurnsPerStage: 6, maxToolCallsPerStage: 12 }
stages:
  - prompt: "Run the tests."
    checks:
      - tool_called: { name: run, argContains: "test" }
  - prompt: "Fix the bug and verify."
    checks:
      - cmd_ok: "go test ./..."
      - tool_called: { name: write_file, min: 1 }
`)
	return root
}

func TestRunSmoke(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	mcpBin := buildMcp(t)
	tasksDir := writeSmokeTask(t)

	fake := &fakeRunner{resp: []scriptedResp{
		// stage 0: emit MALFORMED tool-call JSON (exercises jsonErrors), then a
		// valid run, then report.
		{calls: []llm.ToolCall{rawToolCall("cbad", "run", "{not valid json")}},
		{calls: []llm.ToolCall{toolCall("c0", "run", map[string]any{"argv": []string{"go", "test", "./..."}})}},
		{content: "The test fails: Max returns the smaller value."},
		// stage 1: write the fix, re-run tests, then report
		{calls: []llm.ToolCall{toolCall("c1", "write_file", map[string]any{"path": "mathx.go", "content": fixedMathx})}},
		{calls: []llm.ToolCall{toolCall("c2", "run", map[string]any{"argv": []string{"go", "test", "./..."}})}},
		{content: "Fixed and passing."},
	}}

	opts := Options{
		Config: Config{
			LLM:      LLMConfig{BaseURL: "http://unused.invalid"},
			Models:   []string{"fake"},
			Toolsets: OrderedToolsets{{Name: "baseline"}},
		},
		TasksDir:  tasksDir,
		Out:       t.TempDir(),
		McpBin:    mcpBin,
		NewRunner: func(string) agent.LLMRunner { return fake },
	}

	rows, outDir, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 stage rows, got %d", len(rows))
	}
	if !rows[0].Pass {
		t.Errorf("stage 0 should pass (run test called): %+v", rows[0].Checks)
	}
	if !rows[1].Pass {
		t.Errorf("stage 1 should pass (fix + go test green): %+v", rows[1].Checks)
	}
	if rows[0].ToolCalls < 1 {
		t.Errorf("stage 0 should record >=1 tool call, got %d", rows[0].ToolCalls)
	}
	// Malformed tool-call JSON in stage 0 must be counted as a jsonErrors event
	// (distinct from schema/invalid-arg errors).
	if rows[0].JSONErrors < 1 {
		t.Errorf("stage 0 should record >=1 jsonErrors (malformed call), got %d", rows[0].JSONErrors)
	}
	if rows[0].InvalidArgRetries != 0 {
		t.Errorf("stage 0 should have 0 invalid-arg retries (malformed is a distinct metric), got %d", rows[0].InvalidArgRetries)
	}
	// Token split must be populated per stage (metered runner observed Usage).
	for i, r := range rows {
		if r.PromptTokens <= 0 || r.CompletionTokens <= 0 {
			t.Errorf("stage %d token split not populated: prompt=%d completion=%d", i, r.PromptTokens, r.CompletionTokens)
		}
		if r.Tokens != r.PromptTokens+r.CompletionTokens {
			t.Errorf("stage %d tokens (%d) != prompt+completion (%d)", i, r.Tokens, r.PromptTokens+r.CompletionTokens)
		}
	}
	if rows[1].BaitCalls != 0 {
		t.Errorf("no bait tools in smoke task, got %d bait calls", rows[1].BaitCalls)
	}
	for _, f := range []string{"runs.jsonl", "summary.csv", "report.md"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("expected output %s: %v", f, err)
		}
	}
	// P1 prerequisite: per-run transcript + journal persisted under out/ for the
	// judge phase (they no longer die with the scratch workspace).
	combo := judge.ComboName("fake", "baseline", "smoke")
	for _, p := range []string{
		filepath.Join(outDir, "transcripts", combo+".jsonl"),
		filepath.Join(outDir, "journals", combo+".jsonl"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected persisted artifact %s: %v", p, err)
		}
	}
	// summary.csv must carry the reserved judge_quality column.
	csvBytes, err := os.ReadFile(filepath.Join(outDir, "summary.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(csvBytes), "judge_quality") {
		t.Errorf("summary.csv missing judge_quality column:\n%s", csvBytes)
	}
}

// writeStageScopeTask lays down a 2-stage task whose SECOND stage asserts
// tool_called: write_file. Only stage 0 ever calls write_file, so a correct
// runner fails stage 1.
func writeStageScopeTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tdir := filepath.Join(root, "scoped")
	fx := filepath.Join(tdir, "fixture")
	if err := os.MkdirAll(fx, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx, "keep.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, "task.yaml"), []byte(`name: scoped
class: tooluse
workspace: fixture/
limits: { maxTurnsPerStage: 4, maxToolCallsPerStage: 8 }
stages:
  - prompt: "Write the file."
    checks:
      - tool_called: { name: write_file, min: 1 }
  - prompt: "Say hello. Do not call anything."
    checks:
      - tool_called: { name: write_file, min: 1 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// A stage's checks must see only THAT stage's tool calls. llm-bench-mcp writes
// one append-only journal per task with no stage attribution, so the runner
// slices it by position; before that, every stage saw every other stage's
// calls. The observable bug: a stage making zero calls still passed
// `tool_called`, and `tool_called: X` and `tool_not_called: X` both passed on
// the same stage — a check and its exact negation, simultaneously true.
func TestChecksAreScopedToTheirStage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	mcpBin := buildMcp(t)
	tasksDir := writeStageScopeTask(t)

	fake := &fakeRunner{resp: []scriptedResp{
		// stage 0: calls write_file, then reports.
		{calls: []llm.ToolCall{toolCall("w0", "write_file", map[string]any{"path": "keep.txt", "content": "y\n"})}},
		{content: "wrote it"},
		// stage 1: no tool calls at all.
		{content: "hello"},
	}}

	rows, _, err := Run(context.Background(), Options{
		Config: Config{
			LLM:      LLMConfig{BaseURL: "http://unused.invalid"},
			Models:   []string{"fake"},
			Toolsets: OrderedToolsets{{Name: "baseline"}},
		},
		TasksDir:  tasksDir,
		Out:       t.TempDir(),
		McpBin:    mcpBin,
		NewRunner: func(string) agent.LLMRunner { return fake },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 stage rows, got %d", len(rows))
	}
	if !rows[0].Pass {
		t.Errorf("stage 0 called write_file and must pass: %+v", rows[0].Checks)
	}
	if rows[1].ToolCalls != 0 {
		t.Fatalf("test is miswired: stage 1 should make no calls, got %d", rows[1].ToolCalls)
	}
	if rows[1].Pass {
		t.Errorf("stage 1 made ZERO tool calls yet tool_called: write_file passed — "+
			"it is seeing stage 0's journal entries: %+v", rows[1].Checks)
	}
}

// ── P0.5: toolset binary resolution + startup validation ────────────

func TestResolveCmd(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveCmd(binDir, "mytool"); got != filepath.Join(binDir, "mytool") {
		t.Errorf("bare name present in binDir should resolve there, got %q", got)
	}
	if got := resolveCmd(binDir, "notthere"); got != "notthere" {
		t.Errorf("bare name absent from binDir should fall through to PATH, got %q", got)
	}
	if got := resolveCmd(binDir, "/usr/bin/env"); got != "/usr/bin/env" {
		t.Errorf("path-bearing cmd should be verbatim, got %q", got)
	}
}

func TestValidateToolsetBins(t *testing.T) {
	ts := []Toolset{{Name: "broken", Servers: []ServerSpec{{Cmd: "definitely-not-a-real-binary-xyz"}}}}
	if err := validateToolsetBins(ts, ""); err == nil {
		t.Error("missing toolset binary should error")
	}
	if err := validateToolsetBins([]Toolset{{Name: "baseline"}}, ""); err != nil {
		t.Errorf("baseline (no servers) should validate: %v", err)
	}
}

// TestRunStartupValidationFailsFast proves a missing toolset binary errors
// BEFORE any combo runs (no output dir side effects needed).
func TestRunStartupValidationFailsFast(t *testing.T) {
	tasksDir := writeSmokeTask(t)
	out := t.TempDir()
	opts := Options{
		Config: Config{
			LLM:    LLMConfig{BaseURL: "http://unused.invalid"},
			Models: []string{"fake"},
			Toolsets: OrderedToolsets{
				{Name: "broken", Servers: []ServerSpec{{Cmd: "definitely-not-a-real-binary-xyz", Args: []string{"mcp"}}}},
			},
		},
		TasksDir:  tasksDir,
		Out:       out,
		McpBin:    "/does/not/matter",
		NewRunner: func(string) agent.LLMRunner { return &fakeRunner{} },
	}
	_, _, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected startup validation error, got %v", err)
	}
	// Fail-fast: no timestamped run dir should have been created.
	ents, _ := os.ReadDir(out)
	if len(ents) != 0 {
		t.Errorf("startup failure should not create a run dir, found %d entries", len(ents))
	}
}

// ── P0.5: combo errors are non-fatal + incremental flush ────────────

func writeFailTasks(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"task-a", "task-b"} {
		tdir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(tdir, "fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tdir, "fixture", "seed.txt"), []byte("seed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		yaml := "name: " + name + `
class: tooluse
workspace: fixture/
stages:
  - prompt: "stage one"
    checks:
      - tool_called: { name: read_file, min: 1 }
  - prompt: "stage two"
    checks:
      - file_contains: { path: out.txt, text: "x" }
`
		if err := os.WriteFile(filepath.Join(tdir, "task.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestComboFailureContinuesAndFlushes(t *testing.T) {
	tasksDir := writeFailTasks(t)
	out := t.TempDir()

	var mu sync.Mutex
	var lineCounts []int
	opts := Options{
		Config: Config{
			LLM:      LLMConfig{BaseURL: "http://unused.invalid"},
			Models:   []string{"fake"},
			Toolsets: OrderedToolsets{{Name: "baseline"}},
		},
		TasksDir: tasksDir,
		Out:      out,
		// Missing binary → every combo's MCP spawn fails → runOne errors, but the
		// matrix must complete with synthesized failed rows.
		McpBin:    filepath.Join(t.TempDir(), "no-such-llm-bench-mcp"),
		NewRunner: func(string) agent.LLMRunner { return &fakeRunner{} },
		OnFlush: func(runsPath string) {
			b, _ := os.ReadFile(runsPath)
			mu.Lock()
			lineCounts = append(lineCounts, strings.Count(string(b), "\n"))
			mu.Unlock()
		},
	}
	rows, outDir, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected a non-nil error summarizing failed combos")
	}
	if !strings.Contains(err.Error(), "combo(s) failed") {
		t.Errorf("error should summarize failed combos, got %v", err)
	}
	// 2 tasks × 2 stages = 4 synthesized failed rows; matrix completed.
	if len(rows) != 4 {
		t.Fatalf("expected 4 synthesized rows (matrix completed), got %d", len(rows))
	}
	for _, r := range rows {
		if r.Pass {
			t.Errorf("failed combo rows must be pass=false: %+v", r)
		}
		if r.Note == "" {
			t.Errorf("failed combo rows must carry the error note")
		}
	}
	// Incremental flush: two combos → two flushes with growing line counts.
	mu.Lock()
	got := append([]int(nil), lineCounts...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("incremental flush line counts should grow 2→4 per combo, got %v", got)
	}
	// Reports still written from the full row set.
	for _, f := range []string{"runs.jsonl", "summary.csv", "report.md"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("expected output %s despite combo failures: %v", f, err)
		}
	}
	// Completed rows are on disk (crash-resilience).
	b, _ := os.ReadFile(filepath.Join(outDir, "runs.jsonl"))
	if strings.Count(string(b), "\n") != 4 {
		t.Errorf("runs.jsonl should hold all 4 rows, got %d lines", strings.Count(string(b), "\n"))
	}
}

// ── P0.5: compaction metric via the REAL Shaper ─────────────────────

// nBigFiles is how many distinct big*.txt fixtures TestCompactionMetric reads
// (one read call each — DIFFERENT args — so the identical-call loop-breaker
// never trips while still accumulating enough tokens to force compaction).
const nBigFiles = 8

func writeBigFileTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tdir := filepath.Join(root, "compact")
	if err := os.MkdirAll(filepath.Join(tdir, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ~2.5 KiB each; several reads overflow a tiny Shaper budget. Distinct
	// filenames keep each read_file call's args unique.
	big := strings.Repeat("the production port is 7443 and the primary region is us-west-2. ", 40)
	for i := 0; i < nBigFiles; i++ {
		name := fmt.Sprintf("big%d.txt", i)
		if err := os.WriteFile(filepath.Join(tdir, "fixture", name), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	yaml := `name: compact
class: tooluse
workspace: fixture/
limits: { maxTurnsPerStage: 16, maxToolCallsPerStage: 20 }
stages:
  - prompt: "Read the big*.txt files to survey them."
    checks:
      - compactions_min: 1
`
	if err := os.WriteFile(filepath.Join(tdir, "task.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCompactionMetric(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	mcpBin := buildMcp(t)
	tasksDir := writeBigFileTask(t)

	// Script reads of DIFFERENT big files (distinct args, so the
	// identical-call loop-breaker doesn't fire) so older tool results fall
	// outside the pristine tail AND total exceeds the tiny budget → the
	// Shaper compacts.
	var resp []scriptedResp
	for i := 0; i < nBigFiles; i++ {
		resp = append(resp, scriptedResp{calls: []llm.ToolCall{
			toolCall(fmt.Sprintf("r%d", i), "read_file", map[string]any{"path": fmt.Sprintf("big%d.txt", i)}),
		}})
	}
	resp = append(resp, scriptedResp{content: "surveyed."})
	fake := &fakeRunner{resp: resp}

	opts := Options{
		Config: Config{
			// Tiny global budget forces LOD + compaction. (Task contextBudget must
			// be >=2000; the global has no floor, so drive it here.)
			LLM:      LLMConfig{BaseURL: "http://unused.invalid", ContextBudget: 300},
			Models:   []string{"fake"},
			Toolsets: OrderedToolsets{{Name: "baseline"}},
		},
		TasksDir:  tasksDir,
		Out:       t.TempDir(),
		McpBin:    mcpBin,
		NewRunner: func(string) agent.LLMRunner { return fake },
	}
	rows, _, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 stage row, got %d", len(rows))
	}
	if rows[0].Compactions < 1 {
		t.Fatalf("expected the Shaper to compact at least once, got %d", rows[0].Compactions)
	}
	// The compactions_min:1 check must therefore PASS (mechanism proven to fire).
	if !rows[0].Pass {
		t.Errorf("compactions_min:1 should pass when compaction fired: %+v", rows[0].Checks)
	}
	// Compaction SIZE metric: agentkit CompactionInfo before/after carried through
	// to the flat row. A real fold has a positive active window; folding does not
	// grow it, so after <= before.
	if rows[0].CompactionTokensBefore <= 0 {
		t.Errorf("expected compactionTokensBefore > 0 when a fold fired, got %d", rows[0].CompactionTokensBefore)
	}
	if rows[0].CompactionTokensAfter <= 0 {
		t.Errorf("expected compactionTokensAfter > 0 when a fold fired, got %d", rows[0].CompactionTokensAfter)
	}
	if rows[0].CompactionTokensAfter > rows[0].CompactionTokensBefore {
		t.Errorf("compactionTokensAfter (%d) should not exceed before (%d)",
			rows[0].CompactionTokensAfter, rows[0].CompactionTokensBefore)
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	base := &task.Task{}
	if got := buildSystemPrompt(base); got != systemPrompt {
		t.Errorf("no systemAppend should return the base prompt unchanged")
	}
	appended := &task.Task{SystemAppend: "Defend the codex."}
	got := buildSystemPrompt(appended)
	if !strings.Contains(got, systemPrompt) {
		t.Errorf("appended prompt must still contain the base system prompt")
	}
	if !strings.Contains(got, "Defend the codex.") {
		t.Errorf("appended prompt must contain the systemAppend text")
	}
	if !strings.Contains(got, "\n\n"+"Defend the codex.") {
		t.Errorf("systemAppend must follow a blank line after the base prompt")
	}
}

// ── identical-call loop-breaker ──────────────────────────────────────

// writeIdLoopTask lays down a one-stage task with a very high
// maxToolCallsPerStage, so the ONLY thing that can abort the stage is the
// identical-call loop-breaker, not the tool-call budget.
func writeIdLoopTask(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tdir := filepath.Join(root, "idloop")
	fx := filepath.Join(tdir, "fixture")
	if err := os.MkdirAll(fx, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(fx, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	yaml := `name: idloop
class: tooluse
workspace: fixture/
limits: { maxTurnsPerStage: 6, maxToolCallsPerStage: 100 }
stages:
  - prompt: "read a file."
`
	if err := os.WriteFile(filepath.Join(tdir, "task.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func idLoopOpts(t *testing.T, fake *fakeRunner) Options {
	t.Helper()
	mcpBin := buildMcp(t)
	return Options{
		Config: Config{
			LLM:      LLMConfig{BaseURL: "http://unused.invalid"},
			Models:   []string{"fake"},
			Toolsets: OrderedToolsets{{Name: "baseline"}},
		},
		TasksDir:  writeIdLoopTask(t),
		Out:       t.TempDir(),
		McpBin:    mcpBin,
		NewRunner: func(string) agent.LLMRunner { return fake },
	}
}

// TestIdenticalCallLoopBreaker proves the runner aborts a stage as soon as
// the SAME (name+args) tool call repeats identicalCallLimit (3) times —
// BEFORE the (very high, 100) tool-call budget would ever trip — and that
// the row's note names the loop, not "tool-call budget exceeded".
func TestIdenticalCallLoopBreaker(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	fake := &fakeRunner{resp: []scriptedResp{
		// One round, three IDENTICAL read_file calls back to back.
		{calls: []llm.ToolCall{
			toolCall("c0", "read_file", map[string]any{"path": "a.txt"}),
			toolCall("c1", "read_file", map[string]any{"path": "a.txt"}),
			toolCall("c2", "read_file", map[string]any{"path": "a.txt"}),
		}},
	}}

	rows, _, err := Run(context.Background(), idLoopOpts(t, fake))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 stage row, got %d", len(rows))
	}
	r := rows[0]
	if r.Pass {
		t.Errorf("stage should fail (loop-broken), got Pass=true: %+v", r)
	}
	if !r.LimitBreached {
		t.Errorf("stage should be flagged LimitBreached, got false")
	}
	if !strings.Contains(r.Note, "identical-call loop") {
		t.Errorf("note should mention the identical-call loop, got %q", r.Note)
	}
	if strings.Contains(r.Note, "budget exceeded") {
		t.Errorf("note should NOT be the generic budget-exceeded note, got %q", r.Note)
	}
	// Fired on the 3rd identical call — well before the 100-call budget.
	if r.ToolCalls != 3 {
		t.Errorf("expected exactly 3 tool calls (aborted at the limit), got %d", r.ToolCalls)
	}
}

// The edit-verify cycle must NOT read as a loop. `run go test` → edit → `run
// go test` → edit → `run go test` is three identical run calls, and it is
// exactly what this harness's system prompt asks for ("verify it with the run
// tool"). Only BACK-TO-BACK repeats are a loop: if a different call happened in
// between, state changed, so asking again is a new question.
//
// Regression: the breaker counted identical calls cumulatively AND across
// stages, so fix-failing-test (run-tests → fix → re-run-tests) tripped on the
// third legitimate `go test` and failed for every toolset — while its own
// checks reported the bug correctly fixed.
func TestIdenticalCallLoopBreakerAllowsInterleavedRepeats(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	rd := func(id string) llm.ToolCall { return toolCall(id, "read_file", map[string]any{"path": "a.txt"}) }
	wr := func(id, s string) llm.ToolCall {
		return toolCall(id, "write_file", map[string]any{"path": "a.txt", "content": s})
	}
	fake := &fakeRunner{resp: []scriptedResp{
		// Same read repeated 3×, but each separated by a write: the file
		// changed underneath, so every read is a genuinely new question.
		{calls: []llm.ToolCall{rd("r0"), wr("w0", "one"), rd("r1"), wr("w1", "two"), rd("r2")}},
		{content: "done"},
	}}

	rows, _, err := Run(context.Background(), idLoopOpts(t, fake))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 stage row, got %d", len(rows))
	}
	r := rows[0]
	if r.LimitBreached {
		t.Errorf("interleaved repeats tripped the loop breaker: note=%q", r.Note)
	}
	if strings.Contains(r.Note, "identical-call loop") {
		t.Errorf("edit-verify cycle misreported as a loop: %q", r.Note)
	}
	if r.ToolCalls != 5 {
		t.Errorf("all 5 calls should have run, got %d", r.ToolCalls)
	}
	// The `repeated` metric still counts them — observing a repeat and
	// ABORTING on it are different jobs, and only the latter changed.
	if r.RepeatedCalls != 2 {
		t.Errorf("want 2 repeated reads recorded, got %d", r.RepeatedCalls)
	}
}

// TestIdenticalCallLoopBreakerIgnoresVariedCalls proves 3 DIFFERENT
// (name+args) calls do NOT trip the loop-breaker — only identical repeats do.
func TestIdenticalCallLoopBreakerIgnoresVariedCalls(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	fake := &fakeRunner{resp: []scriptedResp{
		{calls: []llm.ToolCall{
			toolCall("c0", "read_file", map[string]any{"path": "a.txt"}),
			toolCall("c1", "read_file", map[string]any{"path": "b.txt"}),
			toolCall("c2", "read_file", map[string]any{"path": "c.txt"}),
		}},
		{content: "surveyed all three files."},
	}}

	rows, _, err := Run(context.Background(), idLoopOpts(t, fake))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 stage row, got %d", len(rows))
	}
	r := rows[0]
	if r.LimitBreached {
		t.Errorf("varied calls must not breach any limit, got LimitBreached=true note=%q", r.Note)
	}
	if strings.Contains(r.Note, "identical-call loop") {
		t.Errorf("varied calls must not trip the loop-breaker, got note %q", r.Note)
	}
	if r.ToolCalls != 3 {
		t.Errorf("expected all 3 varied calls to go through, got %d", r.ToolCalls)
	}
}

// buildSystemPrompt composes system (replace) then systemAppend (append).
//
// Replacement exists because appending cannot RETRACT. The base prompt says
// "do not ask the user questions"; codex-plan-3-violation requires
// ask_user_question. Its systemAppend told the model to escalate, the base told
// it not to, and the model obeyed the base — so that check failed 8/8 across
// every arm and run. A check nothing can pass looks like a hard task, not a
// broken one.
func TestBuildSystemPromptReplaceAndAppend(t *testing.T) {
	base := buildSystemPrompt(&task.Task{})
	if !strings.Contains(base, "autonomous software engineer") {
		t.Fatalf("empty task should get the base prompt, got: %q", base)
	}

	// systemAppend alone: base survives, append follows.
	got := buildSystemPrompt(&task.Task{SystemAppend: "PERSONA"})
	if !strings.Contains(got, "autonomous software engineer") || !strings.HasSuffix(got, "PERSONA") {
		t.Errorf("append should keep the base and follow it, got: %q", got)
	}

	// system alone: base is GONE. This is the whole point — a task must be able
	// to drop a base rule it needs to contradict.
	got = buildSystemPrompt(&task.Task{System: "REPLACED"})
	if got != "REPLACED" {
		t.Errorf("system should replace the base entirely, got: %q", got)
	}
	if strings.Contains(got, "do not ask the user questions") {
		t.Error("replacement must not leave the base rule behind")
	}

	// Both compose, replacement first.
	got = buildSystemPrompt(&task.Task{System: "REPLACED", SystemAppend: "PERSONA"})
	if got != "REPLACED\n\nPERSONA" {
		t.Errorf("system then systemAppend, got: %q", got)
	}
}

// classifyErr must name the ACTUAL limit. It used to check stageCtx.Err()
// first and return "tool-call budget exceeded" for every cancellation — so a
// stage that hit max turns (10/10 turns, 10/20 calls) and one aborted by the
// 10-minute combo timeout (18/20 calls, nothing breached) BOTH reported a
// budget breach. That note sent three separate investigations after the wrong
// limit in a single session.
func TestClassifyErrNamesTheRealLimit(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	boom := fmt.Errorf("boom")

	t.Run("no error", func(t *testing.T) {
		if b, _ := classifyErr(nil, live, "", ""); b {
			t.Error("nil error must not be a breach")
		}
	})
	t.Run("loop breaker wins", func(t *testing.T) {
		_, n := classifyErr(boom, dead, "identical-call loop: run ×3", "budget!")
		if n != "identical-call loop: run ×3" {
			t.Errorf("got %q", n)
		}
	})
	t.Run("budget is reported only when the dispatcher says so", func(t *testing.T) {
		_, n := classifyErr(boom, dead, "", "tool-call budget exceeded (21 > 20)")
		if n != "tool-call budget exceeded (21 > 20)" {
			t.Errorf("got %q", n)
		}
	})
	t.Run("max turns is not a budget breach", func(t *testing.T) {
		_, n := classifyErr(fmt.Errorf("max turns reached"), live, "", "")
		if n != "max turns exceeded" {
			t.Errorf("max turns misreported as %q", n)
		}
	})
	t.Run("combo timeout is not a budget breach", func(t *testing.T) {
		// The killer case: the parent's DEADLINE passed. Production builds the
		// combo ctx with context.WithTimeout, so a real combo timeout always
		// arrives as an EXPIRED DEADLINE — not a bare cancel.
		expired, xcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer xcancel()
		_, n := classifyErr(boom, expired, "", "")
		if strings.Contains(n, "budget") {
			t.Errorf("combo timeout reported as a budget breach: %q", n)
		}
		if !strings.Contains(n, "combo timeout") {
			t.Errorf("combo timeout should say so, got %q", n)
		}
	})
	t.Run("a bare cancel must NOT claim the combo timeout", func(t *testing.T) {
		// Regression: this branch used to assert "combo timeout" for ANY parent
		// cancellation. A Qwen3-6-27B-MPT stage aborted 9.6s into a 10-MINUTE
		// budget and still blamed the wall clock, hiding the real cause. With no
		// deadline on the ctx there is no evidence of a timeout, so don't claim
		// one — and carry the underlying error so the reader can see what did.
		_, n := classifyErr(boom, dead, "", "")
		if strings.Contains(n, "combo timeout") {
			t.Errorf("bare cancel misreported as the combo timeout: %q", n)
		}
		if !strings.Contains(n, "boom") {
			t.Errorf("underlying error dropped from the note: %q", n)
		}
	})
}
