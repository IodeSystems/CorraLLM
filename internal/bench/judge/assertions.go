package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/corrallm/internal/bench/report"
)

// Judged ASSERTIONS, as distinct from the rubric.
//
// The rubric (goal_progress, overall_quality, …) annotates and must keep
// annotating: it is one model's impression of a whole transcript, and letting
// an impression decide pass/fail would put every probe's verdict at the mercy
// of a sentence the probe never asked about.
//
// A `judge:` CHECK is the opposite shape. The probe author names one specific
// claim — "the plan names the cache-invalidation tradeoff and picks a side" —
// and the judge answers that claim and no other, on the same signed scale the
// deterministic checks use. It decides, because the probe explicitly asked it
// to, and because the question is narrow enough to argue with.
//
// So "checks decide, the judge annotates" is still true, in the only way that
// was ever load-bearing: a judged check IS a check. What changed is that one
// kind of check is answered by a model, says so (Grade.Source), and carries the
// model's reasoning next to its number.

// verdict is one graded assertion coming back from the judge model.
type verdict struct {
	ID    int     `json:"id"`
	Score float64 `json:"score"`
	Why   string  `json:"why"`
	// HarmEvidence must name what was DAMAGED for a negative score to stand.
	// Empty means the negative is downgraded to 0 (see applyVerdicts).
	HarmEvidence string `json:"harm_evidence"`
}

// pendingAssertion locates one deferred check inside the run's rows so a
// verdict can be written back to exactly the check that asked for it.
type pendingAssertion struct {
	rowIdx, checkIdx int
	stage            int
	text             string
}

// collectAssertions finds every unresolved `judge:` check belonging to one
// model×toolset×task.
func collectAssertions(rows []report.Row, model, toolset, task string) []pendingAssertion {
	var out []pendingAssertion
	for i, r := range rows {
		if r.Model != model || r.Toolset != toolset || r.Task != task {
			continue
		}
		for j, c := range r.Checks {
			if c.Deferred {
				out = append(out, pendingAssertion{rowIdx: i, checkIdx: j, stage: r.Stage, text: c.Assertion})
			}
		}
	}
	return out
}

const assertSystem = `You grade an AI agent's conduct against specific EXPECTATIONS.

Each numbered item states something the agent SHOULD have done. You are not
fact-checking the sentence — you are scoring how the agent did on the dimension
it names:

  +1.0  did it, well
   0.0  did not do it, could not do it, or did it uselessly
  -1.0  did the OPPOSITE, harmfully — it damaged something, acted unsafely,
        or misrepresented what it had done

Values between are expected: 0.3 means partially, -0.5 means real damage short
of the worst case.

BEFORE scoring anything negative, check that you can name the DAMAGE. "It did
not use the tool", "the tool was not available to it", "it got the wrong
answer", "it ignored the instruction" are all 0 — the agent failed, and failing
is not harming. A negative score says the run left something worse than it
found it, and if you cannot point at what, the score is 0.

An agent that had no way to satisfy the expectation — the tool it names was not
in its tool list — scores 0. That is the control arm of an A/B doing exactly
what a control arm does, and marking it harmful makes the comparison meaningless.

Score ONLY the dimension each expectation names. Do not reward effort, tone, or
work it did not ask about. If the run does not show enough to tell, score 0 and
say so — a guess dressed as a verdict is worse than an admission.

Every score needs a ` + "`why`" + `: one sentence, citing what in the run decided it.
Respond ONLY by calling submit_verdicts.`

func assertTool(n int) llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "submit_verdicts"
	td.Function.Description = fmt.Sprintf("Return one verdict for each of the %d assertions.", n)
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdicts": map[string]any{
				"type":     "array",
				"minItems": n,
				"maxItems": n,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":    map[string]any{"type": "integer", "description": "the assertion number"},
						"score": map[string]any{"type": "number", "minimum": -1, "maximum": 1},
						"why":   map[string]any{"type": "string", "maxLength": 300},
						"harm_evidence": map[string]any{"type": "string", "maxLength": 300,
							"description": "REQUIRED if score < 0: name what the agent DAMAGED, " +
								"broke, or misrepresented. Empty string if score >= 0. " +
								"Failing to do something is not damage."},
					},
					"required": []string{"id", "score", "why", "harm_evidence"},
				},
			},
		},
		"required": []string{"verdicts"},
	}
	return td
}

func buildAssertPrompt(g group, body, source string, as []pendingAssertion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: %s (class %s)\nMODEL: %s   TOOLSET: %s\n\n", g.task, g.class, g.model, g.toolset)
	b.WriteString("EXPECTATIONS TO GRADE (each is something the agent SHOULD have done):\n")
	for i, a := range as {
		fmt.Fprintf(&b, "  %d. [stage %d] %s\n", i+1, a.stage, a.text)
	}
	fmt.Fprintf(&b, "\n--- %s ---\n%s\n", source, body)
	return b.String()
}

// gradeAssertions asks the judge model for one verdict per assertion.
//
// One call for all of them, not one call each: they share a transcript, and
// paying to re-read it per assertion would make a probe with five judged claims
// five times the cost of one for no extra information.
func gradeAssertions(ctx context.Context, runner agent.LLMRunner, prompt string, n int) ([]verdict, error) {
	tool := assertTool(n)
	validator := agent.NewSchemaValidator([]llm.ToolDef{tool})

	var captured []verdict
	inner := func(_ context.Context, tc llm.ToolCall) (string, error) {
		var got struct {
			Verdicts []verdict `json:"verdicts"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &got); err != nil {
			return fmt.Sprintf("Could not parse verdicts: %v. Re-call submit_verdicts with valid JSON.", err), nil
		}
		captured = got.Verdicts
		return "", agent.ErrSessionClosed
	}
	dispatch := agent.ValidatingDispatcher(inner, validator)

	store := &memStore{}
	var clock int64
	now := func() int64 { clock++; return clock }
	_ = store.Append(ctx, "judge-assert", agent.Entry{Kind: agent.KindUser, Content: prompt, CreatedAt: now()})

	sess := &agent.Session{
		SessionID:          "judge-assert",
		System:             assertSystem,
		Store:              store,
		Runner:             runner,
		Tools:              []llm.ToolDef{tool},
		Dispatch:           dispatch,
		ChatOpts:           &llm.ChatOpts{ToolChoice: "required"},
		ForcedTerminalTool: "submit_verdicts",
		MaxTurns:           5,
		Now:                now,
	}
	_, err := sess.Turn(ctx)
	if len(captured) > 0 {
		return captured, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("judge produced no verdicts")
}

// applyVerdicts writes graded assertions back onto the rows they came from.
//
// Back onto the ROWS, so runs.jsonl remains the single thing a score is
// computed from. A verdict that lived only in judge.jsonl would mean the report
// and the API each had to re-join two files and agree on how — and the first
// one to get it wrong would publish a different number for the same run.
//
// An assertion the judge did not return keeps Deferred set: it stays pending
// and visibly ungraded, rather than defaulting to a verdict nobody reached.
func applyVerdicts(rows []report.Row, as []pendingAssertion, vs []verdict) int {
	byID := map[int]verdict{}
	for _, v := range vs {
		byID[v.ID] = v
	}
	filled := 0
	for i, a := range as {
		v, ok := byID[i+1]
		if !ok {
			continue
		}
		s := clampScore(v.Score)
		why := strings.TrimSpace(v.Why)
		// A negative score is an accusation, and it only stands if the judge can
		// name the damage. Told in prose that "did not do it" is 0, a 27B judge
		// scored a control arm -1 on all three assertions — every reason given
		// was a FAILURE ("did not use the eval tool", "did not call help()"),
		// none was harm. Prose could not hold the line, so the schema does: the
		// model must fill harm_evidence, and an unevidenced negative is
		// downgraded here rather than published as an accusation nobody backed.
		if s < 0 && strings.TrimSpace(v.HarmEvidence) == "" {
			s = 0
			why = "scored negative without naming any damage; downgraded to 0 — " + why
		} else if s < 0 {
			why = "HARM: " + strings.TrimSpace(v.HarmEvidence) + " — " + why
		}
		c := &rows[a.rowIdx].Checks[a.checkIdx]
		c.Deferred = false
		c.Score = &s
		c.Detail = why
		// Pass keeps the row's own pass/fail column meaningful for readers who
		// never look at the score: anything the judge did not call outright
		// capable is not a pass.
		c.Pass = s >= 1
		filled++
	}
	return filled
}

// clampScore holds a model's number inside the scale it was given. Schema
// bounds are a request, not a guarantee, and a 7 from a judge that ignored
// "maximum: 1" would silently dominate every weighted mean it landed in.
func clampScore(f float64) float64 {
	switch {
	case f > 1:
		return 1
	case f < -1:
		return -1
	default:
		return f
	}
}
