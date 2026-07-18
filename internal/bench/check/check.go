// Package check evaluates deterministic task checks against a run's workspace
// filesystem and tool-call journal. Checks decide pass/fail; the judge (P1)
// only annotates.
package check

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/bench/journal"
	"github.com/iodesystems/corrallm/internal/bench/task"
)

// cmdTimeout bounds a cmd_ok check so a hung command doesn't wedge a run.
const cmdTimeout = 60 * time.Second

// Result is the outcome of one check.
type Result struct {
	Kind   string `json:"kind"`
	Desc   string `json:"desc"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Metrics carries run-derived values a check may assert against — things not
// visible in the workspace or journal. Compactions is the CUMULATIVE count of
// agentkit Shaper full-history compactions up to and including this stage.
type Metrics struct {
	Compactions int
	// CompactionTokensAfter is the stage's cumulative post-fold active-window
	// token estimate (Σ CompactionInfo.TokensAfter across this stage's folds).
	// A lower-is-better size signal the compaction_under check gates on.
	CompactionTokensAfter int
	// Response is the model's VISIBLE reply text for this stage
	// (agentkit TurnResult.Reply). Reasoning traces are NOT included: the llm
	// client decodes only the `content` field and ignores `reasoning_content`,
	// which is the behavior response_contains wants — a reply whose entire
	// token budget went to reasoning arrives here EMPTY, and that is a real
	// failure worth surfacing rather than a technicality to paper over. Models
	// that emit reasoning by default (no `--reasoning off`) need a max_tokens
	// generous enough to reach visible content, or every response check fails
	// for a reason that has nothing to do with the capability under test.
	Response string
}

// Evaluate runs one check against the workspace dir, journal entries, and
// run metrics.
func Evaluate(ctx context.Context, c task.Check, workspace string, journ []journal.Entry, m Metrics) Result {
	switch c.Kind {
	case "cmd_ok":
		return cmdOK(ctx, c, workspace)
	case "file_contains":
		return fileContains(c, workspace)
	case "file_absent":
		return fileAbsent(c, workspace)
	case "tool_called":
		return toolCalled(c, journ)
	case "tool_not_called":
		return toolNotCalled(c, journ)
	case "no_repeat_calls":
		return noRepeatCalls(c, journ)
	case "compactions_min":
		return compactionsMin(c, m)
	case "compaction_under":
		return compactionUnder(c, m)
	case "response_contains":
		return responseContains(c, m, true)
	case "response_not_contains":
		return responseContains(c, m, false)
	default:
		return Result{Kind: c.Kind, Desc: c.Kind, Pass: false, Detail: "unknown check kind"}
	}
}

// EvaluateAll evaluates every check in a stage and reports whether all passed.
func EvaluateAll(ctx context.Context, checks []task.Check, workspace string, journ []journal.Entry, m Metrics) ([]Result, bool) {
	out := make([]Result, 0, len(checks))
	all := true
	for _, c := range checks {
		r := Evaluate(ctx, c, workspace, journ, m)
		if !r.Pass {
			all = false
		}
		out = append(out, r)
	}
	return out, all
}

// compactionsMin asserts the Shaper compacted at least N times so far — proving
// the compaction-continuation mechanism actually fired (a task that never
// compacts is vacuous and must FAIL this check).
func compactionsMin(c task.Check, m Metrics) Result {
	n := c.N
	if n < 1 {
		n = 1
	}
	pass := m.Compactions >= n
	r := Result{Kind: c.Kind, Desc: fmt.Sprintf("compactions_min: %d", n), Pass: pass}
	if !pass {
		r.Detail = fmt.Sprintf("only %d compaction(s) fired (want >= %d)", m.Compactions, n)
	}
	return r
}

// compactionUnder asserts the stage folded (compactionTokensAfter > 0) AND the
// folded summary is reasonably terse (<= N). A soft quality gate: a bloated
// summary that still passes recall is flagged; a reasonable one passes. Zero
// folds fails — there is nothing to have summarized tersely.
func compactionUnder(c task.Check, m Metrics) Result {
	n := c.N
	pass := m.CompactionTokensAfter > 0 && m.CompactionTokensAfter <= n
	r := Result{Kind: c.Kind, Desc: fmt.Sprintf("compaction_under: %d", n), Pass: pass}
	if !pass {
		r.Detail = fmt.Sprintf("compactionTokensAfter=%d (want >0 and <= %d)", m.CompactionTokensAfter, n)
	}
	return r
}

// responseContains asserts on the model's visible reply. want=false inverts it
// (response_not_contains).
//
// Matching is CASE-INSENSITIVE and whitespace-collapsed. A probe asking "what
// shape and color is this?" gets "The image shows a **red circle**." from one
// model and "Red circle" from another; a case-sensitive exact-substring match
// would score those differently for no reason connected to the capability being
// probed. This is a deliberately loose check — it answers "did the model see
// it", not "did the model phrase it well". Use the judge for quality.
//
// An EMPTY response fails a response_contains (there is nothing to match) and
// PASSES a response_not_contains. That asymmetry is intentional but sharp: a
// model whose whole token budget went to reasoning emits empty visible content,
// so a probe built only from response_not_contains checks would pass a model
// that said nothing at all. Pair prohibitions with at least one positive check
// — the same lesson tool_not_called taught in the adversarial tasks.
func responseContains(c task.Check, m Metrics, want bool) Result {
	kind := "response_contains"
	if !want {
		kind = "response_not_contains"
	}
	got := collapseWS(m.Response)
	found := strings.Contains(strings.ToLower(got), strings.ToLower(collapseWS(c.Text)))
	r := Result{Kind: c.Kind, Desc: fmt.Sprintf("%s: %q", kind, c.Text), Pass: found == want}
	if !r.Pass {
		switch {
		case got == "":
			r.Detail = "model produced NO visible content (all output may have gone to reasoning — raise max_tokens or disable reasoning)"
		case len(got) > 200:
			r.Detail = fmt.Sprintf("response: %.200q…", got)
		default:
			r.Detail = fmt.Sprintf("response: %q", got)
		}
	}
	return r
}

// collapseWS folds all whitespace runs to single spaces and trims, so a match
// isn't defeated by a newline landing mid-phrase in a wrapped reply.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func cmdOK(ctx context.Context, c task.Check, workspace string) Result {
	r := Result{Kind: c.Kind, Desc: "cmd_ok: " + c.Cmd}
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", c.Cmd)
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	r.Pass = err == nil
	if !r.Pass {
		r.Detail = fmt.Sprintf("exit err: %v\n%s", err, tail(string(out), 500))
	}
	return r
}

func fileContains(c task.Check, workspace string) Result {
	r := Result{Kind: c.Kind, Desc: fmt.Sprintf("file_contains: %s ~ %q", c.Path, c.Text)}
	b, err := os.ReadFile(filepath.Join(workspace, c.Path))
	if err != nil {
		r.Detail = fmt.Sprintf("read: %v", err)
		return r
	}
	r.Pass = strings.Contains(string(b), c.Text)
	if !r.Pass {
		r.Detail = "substring not found"
	}
	return r
}

func fileAbsent(c task.Check, workspace string) Result {
	r := Result{Kind: c.Kind, Desc: "file_absent: " + c.Path}
	_, err := os.Stat(filepath.Join(workspace, c.Path))
	r.Pass = os.IsNotExist(err)
	if !r.Pass {
		r.Detail = "file exists (should be absent)"
	}
	return r
}

// matchArgs reports whether entry e is a call to name whose args satisfy the
// optional argContains substring.
func matchArgs(e journal.Entry, name, argContains string) bool {
	if e.Tool != name {
		return false
	}
	return argContains == "" || strings.Contains(e.ArgsString(), argContains)
}

func toolCalled(c task.Check, journ []journal.Entry) Result {
	n := 0
	for _, e := range journ {
		if matchArgs(e, c.Name, c.ArgContains) {
			n++
		}
	}
	min, max := 1, -1 // default: at least once
	if c.Min != nil {
		min = *c.Min
	}
	if c.Max != nil {
		max = *c.Max
	}
	pass := n >= min && (max < 0 || n <= max)
	desc := fmt.Sprintf("tool_called: %s", c.Name)
	if c.ArgContains != "" {
		desc += fmt.Sprintf(" (args~%q)", c.ArgContains)
	}
	r := Result{Kind: c.Kind, Desc: desc, Pass: pass}
	if !pass {
		r.Detail = fmt.Sprintf("called %d time(s); want min=%d max=%d", n, min, max)
	}
	return r
}

func toolNotCalled(c task.Check, journ []journal.Entry) Result {
	n := 0
	for _, e := range journ {
		if matchArgs(e, c.Name, c.ArgContains) {
			n++
		}
	}
	r := Result{Kind: c.Kind, Desc: "tool_not_called: " + c.Name, Pass: n == 0}
	if !r.Pass {
		r.Detail = fmt.Sprintf("called %d time(s)", n)
	}
	return r
}

func noRepeatCalls(c task.Check, journ []journal.Entry) Result {
	n := c.N
	if n <= 0 {
		n = 2
	}
	counts := map[string]int{}
	worst, worstKey := 0, ""
	for _, e := range journ {
		key := e.Tool + "|" + e.ArgsString()
		counts[key]++
		if counts[key] > worst {
			worst, worstKey = counts[key], key
		}
	}
	pass := worst <= n
	r := Result{Kind: c.Kind, Desc: fmt.Sprintf("no_repeat_calls: n=%d", n), Pass: pass}
	if !pass {
		r.Detail = fmt.Sprintf("%q called %d times (max %d)", worstKey, worst, n)
	}
	return r
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
