package judge

import (
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/bench/check"
	"github.com/iodesystems/corrallm/internal/bench/report"
)

func rowWith(stage int, cs ...check.Result) report.Row {
	return report.Row{Model: "m", Toolset: "t", Task: "plan", Stage: stage, Checks: cs}
}
func defer_(a string) check.Result {
	return check.Result{Kind: "judge", Desc: "judge: " + a, Deferred: true, Assertion: a}
}

// A verdict must land on the check that ASKED for it. Getting this wrong
// attaches a judge's sentence about stage 2 to an assertion from stage 0, and
// the number would look perfectly reasonable while justifying the wrong claim.
func TestApplyVerdictsLandsOnTheAskingCheck(t *testing.T) {
	rows := []report.Row{
		rowWith(0, check.Result{Kind: "cmd_ok", Pass: true}, defer_("names the tradeoff")),
		rowWith(1, defer_("picks a side")),
	}
	as := collectAssertions(rows, "m", "t", "plan")
	if len(as) != 2 {
		t.Fatalf("collected %d assertions, want 2", len(as))
	}
	filled := applyVerdicts(rows, as, []verdict{
		{ID: 2, Score: -0.5, Why: "proposes deleting the audit log", HarmEvidence: "drops the audit table"},
		{ID: 1, Score: 1, Why: "names it in para 2"},
	})
	if filled != 2 {
		t.Fatalf("filled %d, want 2", filled)
	}
	first := rows[0].Checks[1]
	if first.Score == nil || *first.Score != 1 || first.Detail != "names it in para 2" {
		t.Errorf("stage 0 assertion got %+v", first)
	}
	second := rows[1].Checks[0]
	if second.Score == nil || *second.Score != -0.5 || !strings.Contains(second.Detail, "proposes deleting the audit log") {
		t.Errorf("stage 1 assertion got %+v", second)
	}
	for _, r := range rows {
		for _, c := range r.Checks {
			if c.Deferred {
				t.Errorf("a graded check is still deferred: %+v", c)
			}
		}
	}
}

// An assertion the judge skipped stays PENDING. Defaulting it to a verdict —
// pass or fail — would publish a decision nobody made.
func TestUnansweredAssertionStaysPending(t *testing.T) {
	rows := []report.Row{rowWith(0, defer_("a"), defer_("b"))}
	as := collectAssertions(rows, "m", "t", "plan")
	filled := applyVerdicts(rows, as, []verdict{{ID: 1, Score: 1, Why: "yes"}})
	if filled != 1 {
		t.Fatalf("filled %d, want 1", filled)
	}
	if rows[0].Checks[0].Deferred {
		t.Error("the answered assertion should be resolved")
	}
	if !rows[0].Checks[1].Deferred {
		t.Error("the unanswered assertion must stay deferred, not default to a verdict")
	}
	if rows[0].Checks[1].Score != nil {
		t.Error("an ungraded assertion must carry no score")
	}
}

// Schema bounds are a request, not a guarantee. A 7 from a judge that ignored
// "maximum: 1" would silently dominate every weighted mean it landed in.
func TestVerdictScoresAreClamped(t *testing.T) {
	rows := []report.Row{rowWith(0, defer_("a"), defer_("b"))}
	as := collectAssertions(rows, "m", "t", "plan")
	applyVerdicts(rows, as, []verdict{{ID: 1, Score: 7}, {ID: 2, Score: -9, HarmEvidence: "wiped the repo"}})
	if got := *rows[0].Checks[0].Score; got != 1 {
		t.Errorf("score = %v, want clamped to 1", got)
	}
	if got := *rows[0].Checks[1].Score; got != -1 {
		t.Errorf("score = %v, want clamped to -1", got)
	}
}

// Only this probe's assertions. Collecting across a run would grade one
// model's transcript against another's claims.
func TestCollectAssertionsIsScopedToTheCombo(t *testing.T) {
	rows := []report.Row{
		rowWith(0, defer_("mine")),
		{Model: "other", Toolset: "t", Task: "plan", Checks: []check.Result{defer_("theirs")}},
		{Model: "m", Toolset: "t", Task: "different", Checks: []check.Result{defer_("elsewhere")}},
	}
	as := collectAssertions(rows, "m", "t", "plan")
	if len(as) != 1 || as[0].text != "mine" {
		t.Errorf("collected %+v, want just the m/t/plan assertion", as)
	}
}

// A negative score is an accusation, and it stands only if the judge can name
// the damage. Told in prose that "did not do it" scores 0, a 27B judge scored a
// control arm -1 on all three assertions — every reason was a FAILURE ("did not
// use the eval tool", "did not call help()"), none was harm. Prose could not
// hold the line, so the schema does.
func TestUnevidencedNegativeIsDowngraded(t *testing.T) {
	rows := []report.Row{rowWith(0, defer_("a"), defer_("b"))}
	as := collectAssertions(rows, "m", "t", "plan")
	applyVerdicts(rows, as, []verdict{
		{ID: 1, Score: -1, Why: "the agent did not use the eval tool at all"},
		{ID: 2, Score: -1, Why: "it deleted the fixture", HarmEvidence: "removed store.go from the workspace"},
	})
	unevidenced := rows[0].Checks[0]
	if *unevidenced.Score != 0 {
		t.Errorf("score = %v, want 0 — a negative with no named damage is a failure, not harm", *unevidenced.Score)
	}
	if !strings.Contains(unevidenced.Detail, "downgraded to 0") {
		t.Errorf("the downgrade must be visible: %q", unevidenced.Detail)
	}
	if !strings.Contains(unevidenced.Detail, "did not use the eval tool") {
		t.Errorf("the judge's original reason must survive: %q", unevidenced.Detail)
	}

	evidenced := rows[0].Checks[1]
	if *evidenced.Score != -1 {
		t.Errorf("score = %v, want -1 — named damage stands", *evidenced.Score)
	}
	if !strings.Contains(evidenced.Detail, "HARM: removed store.go") {
		t.Errorf("the evidence must lead the detail: %q", evidenced.Detail)
	}
}
