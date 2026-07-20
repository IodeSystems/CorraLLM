// Package report defines the runs.jsonl row schema and writes the output
// artifacts: out/<ts>/runs.jsonl (one flat row per model×toolset×task×stage),
// out/<ts>/summary.csv (one aggregated row per model×toolset×task), and
// out/<ts>/report.md (human rollup + per-task stage grid).
//
// Rows are chart-friendly: every numeric metric is a TOP-LEVEL scalar field
// (StageMetrics is embedded, so its fields marshal inline — no metric lives
// only inside a nested object).
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/iodesystems/corrallm/internal/bench/check"
)

// StageMetrics is per-stage instrumentation. Embedded into Row so every field
// is a top-level scalar in runs.jsonl.
type StageMetrics struct {
	Turns     int `json:"turns"`     // chat rounds
	ToolCalls int `json:"toolCalls"` // dispatch attempts (incl. invalid + bait)
	// PromptTokens is the prompt SENT, cached prefix included. It is NOT a cost:
	// llama.cpp reuses the KV slot, so a stable prefix is sent every turn and
	// evaluated once. Summing this across a conversation charges the tool schema
	// — byte-identical every turn, i.e. the most cacheable thing in the context —
	// once per turn, which made a ~12% real gap look like 2.2x and pointed a
	// whole tool-surface redesign at bytes the cache had already made free.
	// Compare arms on NewPromptTokens + CompletionTokens + WallMs.
	PromptTokens      int `json:"promptTokens"`      // prompt tokens sent this stage (incl. cached)
	CachedTokens      int `json:"cachedTokens"`      // of those, served from cache (sent, never evaluated)
	NewPromptTokens   int `json:"newPromptTokens"`   // prompt tokens actually evaluated — the real prompt cost
	CompletionTokens  int `json:"completionTokens"`  // completion tokens generated this stage
	Tokens            int `json:"tokens"`            // prompt+completion this stage
	InvalidArgRetries int `json:"invalidArgRetries"` // well-formed JSON, wrong shape per tool schema
	JSONErrors        int `json:"jsonErrors"`        // malformed tool-call JSON output from the model
	RepeatedCalls     int `json:"repeatedCalls"`     // identical (name+args) calls seen before
	BaitCalls         int `json:"baitCalls"`         // calls to a declared bait tool
	// BrokenIntermediates counts mutating tool calls after which the workspace
	// FAILED the task's safetyCheck (e.g. `go build`) — i.e. a compile-broken
	// state that actually landed on disk, even if a later edit fixed it. 0 when
	// the task sets no safetyCheck. This is the metric that separates edit
	// validation (reverts breaks → 0) from plain editing (breaks land → >0) on
	// tasks a capable model still ultimately passes.
	BrokenIntermediates int `json:"brokenIntermediates"`
	Retries429          int `json:"retries429"`  // 429 backpressure retries (not surfaced by agentkit; always 0 in P0)
	Compactions         int `json:"compactions"` // agentkit Shaper full-history compactions this stage (LOD truncations are render-time and not reported)

	// CompactionTokensBefore/After are the agentkit CompactionInfo active-window
	// token estimates, SUMMED across every fold in this stage (common case: one
	// fold → that fold's numbers). Lower-is-better quality signals: a model whose
	// summary is terse while still passing recall folds to fewer TokensAfter.
	CompactionTokensBefore int `json:"compactionTokensBefore"`
	CompactionTokensAfter  int `json:"compactionTokensAfter"`

	TokPerSec float64 `json:"tokPerSec"` // Tokens / (WallMs/1000)
	WallMs    int64   `json:"wallMs"`
}

// Row is one runs.jsonl record: model × toolset × task × stage. StageMetrics is
// embedded (inline JSON) so all numerics chart directly.
type Row struct {
	TS      string `json:"ts"`
	Model   string `json:"model"`
	Toolset string `json:"toolset"`
	Task    string `json:"task"`
	Class   string `json:"class"`
	Stage   int    `json:"stage"`
	Prompt  string `json:"prompt"`

	// Capability is the serving surface the PROBE required (chat, audio.stt,
	// …), not the model's. Recorded per row because a run-wide pass rate mixes
	// surfaces that were never comparable: an audio model runs only its audio
	// probes (the rest skip) and scores near 100%, which reads as "better than
	// the chat model" when it means "measured on an easier, smaller set".
	// Grouping by this column is what restores the comparison.
	Capability string `json:"capability,omitempty"`

	// RunMode is the residency state this pass ran against ("" | cold | warm).
	// A `both` probe emits two sets of rows, one per mode: a disagreement
	// between them is the finding, so the mode must be on the row or the two
	// passes are indistinguishable in the results.
	RunMode string `json:"runMode,omitempty"`
	// ResidencyNote records what residency control ACTUALLY did — including
	// failing. A cold pass that silently ran warm would otherwise stand as
	// evidence for a claim it never tested.
	ResidencyNote string `json:"residencyNote,omitempty"`

	// ToolFormat is the tool-result encoding used for this run (json | toon |
	// csv | json-toon | loose | tight | tight-lift). Constant across a run; recorded per-row so runs
	// across formats are directly comparable in the aggregates.
	ToolFormat string `json:"toolFormat"`

	StageMetrics // inline: turns, toolCalls, promptTokens, … as top-level fields

	Checks        []check.Result `json:"checks"`
	ChecksPassed  int            `json:"checksPassed"`
	ChecksTotal   int            `json:"checksTotal"`
	Pass          bool           `json:"pass"`
	LimitBreached bool           `json:"limitBreached"`
	Note          string         `json:"note,omitempty"`

	// Judge is reserved for the P1 judge phase (full object); always null in P0.
	Judge any `json:"judge"`
	// JudgeQuality is the reserved scalar judge score for charting; null in P0.
	JudgeQuality any `json:"judge_quality"`
}

// JudgeScores are the P1 judge results for one model×toolset×task, merged into
// summary.csv. Injection is nil for non-adversarial tasks.
type JudgeScores struct {
	Quality   int
	Goal      int
	ToolEff   int
	Injection *int
}

// SummaryKey is the model×toolset×task identity used to join judge results onto
// summary rows.
func SummaryKey(model, toolset, task string) string {
	return model + "\x00" + toolset + "\x00" + task
}

// WriteAll writes runs.jsonl, summary.csv, and report.md into outDir. The judge
// columns in summary.csv are left empty (P1 fills them via WriteSummaryCSV).
func WriteAll(outDir, ts string, rows []Row) error {
	if err := writeRunsJSONL(filepath.Join(outDir, "runs.jsonl"), rows); err != nil {
		return err
	}
	if err := WriteSummaryCSV(filepath.Join(outDir, "summary.csv"), rows, nil); err != nil {
		return err
	}
	return writeReportMD(filepath.Join(outDir, "report.md"), ts, rows)
}

func writeRunsJSONL(path string, rows []Row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ── aggregation ─────────────────────────────────────────────────────

// agg accumulates numerics over a set of stage rows.
type agg struct {
	stages, passed                 int
	turns, toolCalls               int
	promptTokens, completionTokens int
	tokens                         int
	invalidArg, jsonErrors         int
	repeated, bait, retries429     int
	brokenIntermediates            int
	compactions                    int
	compTokBefore, compTokAfter    int
	wallMs                         int64
}

func (a *agg) add(r Row) {
	a.stages++
	if r.Pass {
		a.passed++
	}
	a.turns += r.Turns
	a.toolCalls += r.ToolCalls
	a.promptTokens += r.PromptTokens
	a.completionTokens += r.CompletionTokens
	a.tokens += r.Tokens
	a.invalidArg += r.InvalidArgRetries
	a.jsonErrors += r.JSONErrors
	a.repeated += r.RepeatedCalls
	a.bait += r.BaitCalls
	a.retries429 += r.Retries429
	a.brokenIntermediates += r.BrokenIntermediates
	a.compactions += r.Compactions
	a.compTokBefore += r.CompactionTokensBefore
	a.compTokAfter += r.CompactionTokensAfter
	a.wallMs += r.WallMs
}

func (a *agg) passRate() float64 {
	if a.stages == 0 {
		return 0
	}
	return float64(a.passed) / float64(a.stages)
}
func (a *agg) invalidRate() float64 {
	if a.toolCalls == 0 {
		return 0
	}
	return float64(a.invalidArg) / float64(a.toolCalls)
}
func (a *agg) tokPerSec() float64 {
	if a.wallMs == 0 {
		return 0
	}
	return float64(a.tokens) / (float64(a.wallMs) / 1000)
}

// ── summary.csv (one row per model×toolset×task) ────────────────────

var summaryHeader = []string{
	"model", "toolset", "task", "run_mode", "class", "tool_format",
	"stages", "stages_passed", "pass_rate",
	"turns", "tool_calls",
	"prompt_tokens", "completion_tokens", "tokens",
	"invalid_arg_retries", "json_errors", "repeated_calls", "bait_calls", "broken_intermediates", "retries_429", "compactions",
	"compaction_tokens_before", "compaction_tokens_after",
	"wall_ms", "tok_per_sec",
	"judge_quality", "judge_goal", "judge_tool_eff", "judge_injection",
}

// WriteSummaryCSV writes one aggregated row per model×toolset×task. When judge
// is non-nil, the judge_* columns are filled from it (keyed by SummaryKey);
// otherwise those columns are empty (deterministic pass/fail is unaffected).
func WriteSummaryCSV(path string, rows []Row, judge map[string]JudgeScores) error {
	// runMode is part of the key: a `run: both` probe emits a cold pass AND a
	// warm pass, and merging them would average a model that WORKS warm with the
	// same model FAILING cold into a meaningless ~50%, hiding the disagreement
	// that is the entire reason both passes were run.
	type key struct{ model, toolset, task, runMode string }
	aggs := map[key]*agg{}
	classOf := map[key]string{}
	formatOf := map[key]string{}
	var order []key
	for _, r := range rows {
		k := key{r.Model, r.Toolset, r.Task, r.RunMode}
		a, ok := aggs[k]
		if !ok {
			a = &agg{}
			aggs[k] = a
			classOf[k] = r.Class
			formatOf[k] = r.ToolFormat
			order = append(order, k)
		}
		a.add(r)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].model != order[j].model {
			return order[i].model < order[j].model
		}
		if order[i].toolset != order[j].toolset {
			return order[i].toolset < order[j].toolset
		}
		if order[i].task != order[j].task {
			return order[i].task < order[j].task
		}
		return order[i].runMode < order[j].runMode
	})

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(summaryHeader); err != nil {
		return err
	}
	for _, k := range order {
		a := aggs[k]
		jq, jg, jt, ji := "", "", "", ""
		if judge != nil {
			if js, ok := judge[SummaryKey(k.model, k.toolset, k.task)]; ok {
				jq, jg, jt = itoa(js.Quality), itoa(js.Goal), itoa(js.ToolEff)
				if js.Injection != nil {
					ji = itoa(*js.Injection)
				}
			}
		}
		rec := []string{
			k.model, k.toolset, k.task, k.runMode, classOf[k], formatOf[k],
			itoa(a.stages), itoa(a.passed), ftoa(a.passRate()),
			itoa(a.turns), itoa(a.toolCalls),
			itoa(a.promptTokens), itoa(a.completionTokens), itoa(a.tokens),
			itoa(a.invalidArg), itoa(a.jsonErrors), itoa(a.repeated), itoa(a.bait), itoa(a.brokenIntermediates), itoa(a.retries429), itoa(a.compactions),
			itoa(a.compTokBefore), itoa(a.compTokAfter),
			strconv.FormatInt(a.wallMs, 10), ftoa(a.tokPerSec()),
			jq, jg, jt, ji,
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ── report.md ───────────────────────────────────────────────────────

func writeReportMD(path, ts string, rows []Row) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench report — %s\n\n", ts)

	// Per model×toolset rollup.
	type key struct{ model, toolset string }
	aggs := map[key]*agg{}
	var order []key
	for _, r := range rows {
		k := key{r.Model, r.Toolset}
		a, ok := aggs[k]
		if !ok {
			a = &agg{}
			aggs[k] = a
			order = append(order, k)
		}
		a.add(r)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].model != order[j].model {
			return order[i].model < order[j].model
		}
		return order[i].toolset < order[j].toolset
	})

	b.WriteString("## Rollup (per model × toolset)\n\n")
	b.WriteString("| model | toolset | stage pass % | bait | inv-arg | json-err | prompt tok | compl tok | avg tok/s |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, k := range order {
		a := aggs[k]
		fmt.Fprintf(&b, "| %s | %s | %.0f%% (%d/%d) | %d | %.3f | %d | %d | %d | %.1f |\n",
			k.model, k.toolset, 100*a.passRate(), a.passed, a.stages,
			a.bait, a.invalidRate(), a.jsonErrors, a.promptTokens, a.completionTokens, a.tokPerSec())
	}

	// Per-task stage grid.
	b.WriteString("\n## Stage grid (per task)\n\n")
	tasks := map[string][]Row{}
	var taskOrder []string
	for _, r := range rows {
		if _, ok := tasks[r.Task]; !ok {
			taskOrder = append(taskOrder, r.Task)
		}
		tasks[r.Task] = append(tasks[r.Task], r)
	}
	sort.Strings(taskOrder)
	for _, tname := range taskOrder {
		trows := tasks[tname]
		fmt.Fprintf(&b, "### %s (%s)\n\n", tname, trows[0].Class)
		b.WriteString("| model | toolset | stage | result | checks | bait | inv | json | rep | comp | ptok | ctok | ms |\n")
		b.WriteString("|---|---|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, r := range trows {
			result := "PASS"
			if !r.Pass {
				result = "FAIL"
				if r.LimitBreached {
					result = "FAIL*"
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %d/%d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
				r.Model, r.Toolset, r.Stage, result, r.ChecksPassed, r.ChecksTotal,
				r.BaitCalls, r.InvalidArgRetries, r.JSONErrors, r.RepeatedCalls, r.Compactions,
				r.PromptTokens, r.CompletionTokens, r.WallMs)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n`FAIL*` = stage aborted on a per-stage limit (turns or tool-call budget). " +
		"`inv` = valid-JSON/wrong-shape tool args; `json` = malformed tool-call JSON output; " +
		"`comp` = agentkit Shaper full-history compactions (LOD truncations are render-time and not reported). " +
		"`retries429` is reserved (agentkit handles 429 internally with no hook — always 0 in P0). " +
		"`judge`/`judge_quality` are reserved for P1 and are always null.\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// JudgeRow is one model×toolset×task judge result for the report.md section.
type JudgeRow struct {
	Model, Toolset, Task, Class string
	Quality, Goal, ToolEff      int
	Injection                   *int
	Rationale                   string
	Err                         string // non-empty if judging failed
}

// AppendJudgeSection appends a "Judge (P1)" section to report.md: a per
// model×toolset mean-score table and the per-task worst rationales. Judge
// scores are reported SEPARATELY from deterministic pass/fail — never blended.
func AppendJudgeSection(path, judgeModel string, jrows []JudgeRow) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Judge (P1) — model %q\n\n", judgeModel)
	b.WriteString("Judge scores annotate; they do NOT decide pass/fail (that's the deterministic checks above).\n\n")

	// Per model×toolset means.
	type key struct{ model, toolset string }
	type acc struct {
		n                   int
		goal, tool, overall int
		injN, injSum        int
	}
	means := map[key]*acc{}
	var order []key
	for _, r := range jrows {
		if r.Err != "" {
			continue
		}
		k := key{r.Model, r.Toolset}
		a, ok := means[k]
		if !ok {
			a = &acc{}
			means[k] = a
			order = append(order, k)
		}
		a.n++
		a.goal += r.Goal
		a.tool += r.ToolEff
		a.overall += r.Quality
		if r.Injection != nil {
			a.injN++
			a.injSum += *r.Injection
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].model != order[j].model {
			return order[i].model < order[j].model
		}
		return order[i].toolset < order[j].toolset
	})
	b.WriteString("| model | toolset | n | mean goal | mean tool-eff | mean injection | mean overall |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|\n")
	for _, k := range order {
		a := means[k]
		inj := "n/a"
		if a.injN > 0 {
			inj = ftoa(float64(a.injSum) / float64(a.injN))
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s |\n",
			k.model, k.toolset, a.n,
			ftoa(mean(a.goal, a.n)), ftoa(mean(a.tool, a.n)), inj, ftoa(mean(a.overall, a.n)))
	}

	// Per-task worst rationale (lowest overall_quality).
	b.WriteString("\n### Worst rationale per task\n\n")
	worst := map[string]JudgeRow{}
	var tasks []string
	for _, r := range jrows {
		cur, ok := worst[r.Task]
		if !ok {
			tasks = append(tasks, r.Task)
			worst[r.Task] = r
			continue
		}
		if r.Err == "" && (cur.Err != "" || r.Quality < cur.Quality) {
			worst[r.Task] = r
		}
	}
	sort.Strings(tasks)
	for _, tname := range tasks {
		r := worst[tname]
		if r.Err != "" {
			fmt.Fprintf(&b, "- **%s** (%s/%s): judging error: %s\n", tname, r.Model, r.Toolset, r.Err)
			continue
		}
		fmt.Fprintf(&b, "- **%s** (%s/%s, overall %d): %s\n", tname, r.Model, r.Toolset, r.Quality, r.Rationale)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(b.String())
	return err
}

func mean(sum, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func itoa(i int) string     { return strconv.Itoa(i) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }
