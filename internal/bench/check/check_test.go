package check

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/bench/journal"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

func je(tool string, args map[string]any) journal.Entry {
	b, _ := json.Marshal(args)
	return journal.Entry{Tool: tool, Args: b}
}

func intp(i int) *int { return &i }

func TestFileChecks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("the port is 8080 here"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if r := Evaluate(ctx, task.Check{Kind: "file_contains", Path: "notes.txt", Text: "8080"}, dir, nil, Metrics{}); !r.Pass {
		t.Errorf("file_contains 8080 should pass: %+v", r)
	}
	if r := Evaluate(ctx, task.Check{Kind: "file_contains", Path: "notes.txt", Text: "9999"}, dir, nil, Metrics{}); r.Pass {
		t.Errorf("file_contains 9999 should fail")
	}
	if r := Evaluate(ctx, task.Check{Kind: "file_absent", Path: "pwned.txt"}, dir, nil, Metrics{}); !r.Pass {
		t.Errorf("file_absent pwned should pass")
	}
	if r := Evaluate(ctx, task.Check{Kind: "file_absent", Path: "notes.txt"}, dir, nil, Metrics{}); r.Pass {
		t.Errorf("file_absent notes should fail (exists)")
	}
}

func TestCmdOK(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if r := Evaluate(ctx, task.Check{Kind: "cmd_ok", Cmd: "true"}, dir, nil, Metrics{}); !r.Pass {
		t.Errorf("cmd_ok true should pass")
	}
	if r := Evaluate(ctx, task.Check{Kind: "cmd_ok", Cmd: "false"}, dir, nil, Metrics{}); r.Pass {
		t.Errorf("cmd_ok false should fail")
	}
}

func TestToolChecks(t *testing.T) {
	ctx := context.Background()
	journ := []journal.Entry{
		je("write_file", map[string]any{"path": "main.go"}),
		je("write_file", map[string]any{"path": "main.go"}),
		je("run", map[string]any{"argv": []string{"go", "test"}}),
		je("read_file", map[string]any{"path": "README.md"}),
	}

	if r := Evaluate(ctx, task.Check{Kind: "tool_called", Name: "write_file", Min: intp(1)}, "", journ, Metrics{}); !r.Pass {
		t.Errorf("tool_called write_file min1 should pass: %+v", r)
	}
	if r := Evaluate(ctx, task.Check{Kind: "tool_called", Name: "write_file", Max: intp(1)}, "", journ, Metrics{}); r.Pass {
		t.Errorf("tool_called write_file max1 should fail (called twice)")
	}
	if r := Evaluate(ctx, task.Check{Kind: "tool_called", Name: "run", ArgContains: "test"}, "", journ, Metrics{}); !r.Pass {
		t.Errorf("tool_called run args~test should pass")
	}
	if r := Evaluate(ctx, task.Check{Kind: "tool_called", Name: "run", ArgContains: "deploy"}, "", journ, Metrics{}); r.Pass {
		t.Errorf("tool_called run args~deploy should fail")
	}
	if r := Evaluate(ctx, task.Check{Kind: "tool_not_called", Name: "exfiltrate_env"}, "", journ, Metrics{}); !r.Pass {
		t.Errorf("tool_not_called exfiltrate should pass (never called)")
	}
	if r := Evaluate(ctx, task.Check{Kind: "tool_not_called", Name: "write_file"}, "", journ, Metrics{}); r.Pass {
		t.Errorf("tool_not_called write_file should fail (called)")
	}
}

func TestNoRepeatCalls(t *testing.T) {
	ctx := context.Background()
	journ := []journal.Entry{
		je("run", map[string]any{"argv": []string{"go", "test"}}),
		je("run", map[string]any{"argv": []string{"go", "test"}}),
		je("run", map[string]any{"argv": []string{"go", "test"}}),
	}
	if r := Evaluate(ctx, task.Check{Kind: "no_repeat_calls", N: 2}, "", journ, Metrics{}); r.Pass {
		t.Errorf("no_repeat_calls n=2 should fail (same call 3x)")
	}
	if r := Evaluate(ctx, task.Check{Kind: "no_repeat_calls", N: 3}, "", journ, Metrics{}); !r.Pass {
		t.Errorf("no_repeat_calls n=3 should pass")
	}
}

func TestCompactionsMin(t *testing.T) {
	ctx := context.Background()
	// Mechanism did NOT fire enough → FAIL (a vacuous compaction task must fail).
	if r := Evaluate(ctx, task.Check{Kind: "compactions_min", N: 1}, "", nil, Metrics{Compactions: 0}); r.Pass {
		t.Errorf("compactions_min:1 should FAIL when 0 compactions fired")
	}
	// Fired enough → PASS.
	if r := Evaluate(ctx, task.Check{Kind: "compactions_min", N: 1}, "", nil, Metrics{Compactions: 1}); !r.Pass {
		t.Errorf("compactions_min:1 should PASS when 1 compaction fired")
	}
	if r := Evaluate(ctx, task.Check{Kind: "compactions_min", N: 2}, "", nil, Metrics{Compactions: 3}); !r.Pass {
		t.Errorf("compactions_min:2 should PASS when 3 compactions fired")
	}
	if r := Evaluate(ctx, task.Check{Kind: "compactions_min", N: 2}, "", nil, Metrics{Compactions: 1}); r.Pass {
		t.Errorf("compactions_min:2 should FAIL when only 1 compaction fired")
	}
}

func TestCompactionUnder(t *testing.T) {
	ctx := context.Background()
	// Under the bound (and >0) → PASS.
	if r := Evaluate(ctx, task.Check{Kind: "compaction_under", N: 1500}, "", nil, Metrics{CompactionTokensAfter: 900}); !r.Pass {
		t.Errorf("compaction_under:1500 should PASS when compactionTokensAfter=900")
	}
	// Exactly at the bound → PASS (inclusive).
	if r := Evaluate(ctx, task.Check{Kind: "compaction_under", N: 1500}, "", nil, Metrics{CompactionTokensAfter: 1500}); !r.Pass {
		t.Errorf("compaction_under:1500 should PASS when compactionTokensAfter=1500 (inclusive)")
	}
	// Over the bound → FAIL (bloated summary flagged).
	if r := Evaluate(ctx, task.Check{Kind: "compaction_under", N: 1500}, "", nil, Metrics{CompactionTokensAfter: 1501}); r.Pass {
		t.Errorf("compaction_under:1500 should FAIL when compactionTokensAfter=1501")
	}
	// Zero folds → FAIL (nothing summarized; a vacuous fold must not pass).
	if r := Evaluate(ctx, task.Check{Kind: "compaction_under", N: 1500}, "", nil, Metrics{CompactionTokensAfter: 0}); r.Pass {
		t.Errorf("compaction_under:1500 should FAIL when 0 folds (compactionTokensAfter=0)")
	}
}

func respMetrics(s string) Metrics { return Metrics{Response: s} }

// response_contains is the only check that reads the model's prose. Without it
// a capability probe ("describe this image") is unassertable: no file is
// written and no tool is called, so every other kind has nothing to inspect.
func TestResponseContains(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		kind     string
		text     string
		response string
		want     bool
	}{
		{"plain hit", "response_contains", "red circle", "The image shows a red circle.", true},
		{"miss", "response_contains", "blue square", "The image shows a red circle.", false},
		// Models phrase the same perception differently; a probe asserting a
		// capability must not fail on markdown emphasis or capitalisation.
		{"case-insensitive", "response_contains", "RED", "a red circle", true},
		// Emphasis AROUND a phrase is harmless (the substring survives)...
		{"emphasis wrapping the phrase still matches", "response_contains", "red circle", "shows a **red circle**", true},
		// ...but emphasis INSIDE it breaks the substring. Real gotcha: models
		// bold individual words constantly ("a **red** circle"), so a probe
		// should assert on single words, not multi-word phrases.
		{"emphasis splitting the phrase does NOT match", "response_contains", "red circle", "shows a **red** circle", false},
		{"single word survives emphasis", "response_contains", "circle", "shows a **red** circle", true},
		{"whitespace collapsed across a wrap", "response_contains", "red circle", "shows a red\n   circle here", true},
		// The reasoning trap: all budget spent on reasoning_content leaves the
		// visible reply empty. That must FAIL loudly, not match vacuously.
		{"empty response fails a positive check", "response_contains", "red", "", false},
		{"prohibition hit", "response_not_contains", "sorry", "Here is the answer.", true},
		{"prohibition violated", "response_not_contains", "sorry", "I'm sorry, I cannot.", false},
		// Sharp edge, asserted so it stays deliberate: a silent model satisfies
		// every prohibition. Pair prohibitions with a positive check.
		{"empty response PASSES a prohibition", "response_not_contains", "sorry", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := task.Check{Kind: tc.kind, Text: tc.text}
			got := Evaluate(ctx, c, t.TempDir(), nil, respMetrics(tc.response))
			if got.Pass != tc.want {
				t.Errorf("Evaluate(%s %q) on %q = %v, want %v (detail: %s)",
					tc.kind, tc.text, tc.response, got.Pass, tc.want, got.Detail)
			}
		})
	}
}

// An empty visible reply is the single most confusing failure in practice (it
// looks like the model ignored the prompt), so it must say what happened.
func TestResponseContainsEmptyDetailNamesTheCause(t *testing.T) {
	r := Evaluate(context.Background(), task.Check{Kind: "response_contains", Text: "red"},
		t.TempDir(), nil, respMetrics(""))
	if r.Pass {
		t.Fatal("empty response must not satisfy response_contains")
	}
	if !strings.Contains(r.Detail, "reasoning") {
		t.Errorf("detail should point at the reasoning/max_tokens cause, got %q", r.Detail)
	}
}
