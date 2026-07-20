package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/corrallm/internal/bench/judge"
	"github.com/iodesystems/corrallm/internal/store"
)

// Drill-in: the evidence behind a probe's score.
//
// A pass rate says a probe went badly, never why. The why is already produced —
// per-stage metrics and per-check verdicts in the DB, transcripts and tool-call
// journals in out/<ts>/ — it just had no way to reach a reader. These endpoints
// are that path.
//
// The split is deliberate: checks and stage metrics are small and structured, so
// they live in SQLite and survive out/ being pruned; transcripts and journals are
// bulky replay data, so they stay as files and are read on demand.

// --- A/B arms ---------------------------------------------------------------
//
// An arm is (toolset, toolFormat, runMode). A run varies these ON PURPOSE to
// compare them, so they are part of a result's identity, never something to
// average over.

// armKey identifies one arm of one probe.
type armKey struct {
	toolset, format, runMode string
}

// baselineRank orders arms so one can be designated the baseline. Lower wins.
//
// The capability score is taken from the baseline arm rather than pooled across
// all arms, so the headline number means the same thing from run to run. Pooling
// makes a model's score move when an arm is added or dropped, which reads as a
// quality change that never happened.
func baselineRank(k armKey) (int, int, int) {
	mode := 3
	switch k.runMode {
	case "warm":
		mode = 0 // steady state: what serving actually looks like
	case "":
		mode = 1 // residency uncontrolled
	case "cold":
		mode = 2 // the deliberately adversarial pass; a delta, not the headline
	}
	toolset := 1
	if k.toolset == "baseline" || k.toolset == "" {
		toolset = 0
	}
	format := 1
	if k.format == "json" || k.format == "" {
		format = 0
	}
	return mode, toolset, format
}

// pickBaseline returns the baseline arm among those present.
//
// Falls back through the rank tuple and finally to a lexicographic tiebreak, so
// the choice is deterministic even for arm sets none of the preferences match —
// an unstable baseline would silently redefine the score between requests.
func pickBaseline(arms []armKey) armKey {
	if len(arms) == 0 {
		return armKey{}
	}
	best := arms[0]
	for _, a := range arms[1:] {
		am, at, af := baselineRank(a)
		bm, bt, bf := baselineRank(best)
		switch {
		case am != bm:
			if am < bm {
				best = a
			}
		case at != bt:
			if at < bt {
				best = a
			}
		case af != bf:
			if af < bf {
				best = a
			}
		default:
			if a.toolset+"\x00"+a.format+"\x00"+a.runMode <
				best.toolset+"\x00"+best.format+"\x00"+best.runMode {
				best = a
			}
		}
	}
	return best
}

// armLabel renders an arm for display: "baseline/json/warm".
func armLabel(k armKey) string {
	part := func(s, dflt string) string {
		if s == "" {
			return dflt
		}
		return s
	}
	return part(k.toolset, "default") + "/" + part(k.format, "json") + "/" + part(k.runMode, "any")
}

// BenchArmView is one A/B arm's outcome for a probe.
type BenchArmView struct {
	Toolset    string `json:"toolset"`
	ToolFormat string `json:"toolFormat"`
	RunMode    string `json:"runMode"`
	Label      string `json:"label" doc:"Display form, e.g. \"baseline/json/warm\"."`
	IsBaseline bool   `json:"isBaseline" doc:"The arm the probe's headline score comes from."`

	Stages       int     `json:"stages"`
	StagesPassed int     `json:"stagesPassed"`
	Score        float64 `json:"score"`
	// ScoreDelta is this arm's score minus the baseline arm's — the number an
	// A/B is actually read for. Zero on the baseline itself.
	ScoreDelta       float64 `json:"scoreDelta"`
	ChecksPassed     int     `json:"checksPassed"`
	ChecksTotal      int     `json:"checksTotal"`
	Pass             bool    `json:"pass"`
	WallMS           int64   `json:"wallMs"`
	NewPromptTokens  int     `json:"newPromptTokens" doc:"Prompt tokens actually evaluated; the comparable token cost between arms."`
	CompletionTokens int     `json:"completionTokens"`
	Note             string  `json:"note,omitempty"`
	Skipped          bool    `json:"skipped,omitempty"`
	SkipReason       string  `json:"skipReason,omitempty"`
}

// BenchProbeView is one probe across all the arms it was run under.
type BenchProbeView struct {
	Probe string `json:"probe"`
	Class string `json:"class,omitempty"`
	// Score/Stages come from the baseline arm only.
	Score        float64        `json:"score"`
	Stages       int            `json:"stages"`
	StagesPassed int            `json:"stagesPassed"`
	Pass         bool           `json:"pass"`
	Skipped      bool           `json:"skipped,omitempty"`
	SkipReason   string         `json:"skipReason,omitempty"`
	Note         string         `json:"note,omitempty"`
	Arms         []BenchArmView `json:"arms"`
	// Disagreement flags arms that reached different verdicts on the same probe.
	// This is the finding a single pooled score hides — a probe that works warm
	// and fails cold is the bug, not an average.
	Disagreement bool `json:"disagreement"`
}

// armsFor folds a probe's rows into arm views, designating a baseline.
func armsFor(rows []store.BenchProbeResult) []BenchArmView {
	keys := make([]armKey, 0, len(rows))
	byKey := map[armKey]store.BenchProbeResult{}
	for _, r := range rows {
		k := armKey{r.Toolset, r.ToolFormat, r.RunMode}
		if _, seen := byKey[k]; !seen {
			keys = append(keys, k)
		}
		byKey[k] = r
	}
	base := pickBaseline(keys)
	baseScore := 0.0
	if r, ok := byKey[base]; ok && r.Stages > 0 {
		baseScore = float64(r.StagesPassed) / float64(r.Stages)
	}
	out := make([]BenchArmView, 0, len(keys))
	for _, k := range keys {
		r := byKey[k]
		score := 0.0
		if r.Stages > 0 {
			score = float64(r.StagesPassed) / float64(r.Stages)
		}
		v := BenchArmView{
			Toolset: k.toolset, ToolFormat: k.format, RunMode: k.runMode,
			Label: armLabel(k), IsBaseline: k == base,
			Stages: r.Stages, StagesPassed: r.StagesPassed, Score: score,
			ChecksPassed: r.ChecksPassed, ChecksTotal: r.ChecksTotal, Pass: r.Pass,
			WallMS: r.WallMS, NewPromptTokens: r.NewPromptTokens,
			CompletionTokens: r.CompletionTokens, Note: r.Note,
			Skipped: r.Skipped, SkipReason: r.SkipReason,
		}
		if !v.IsBaseline && !r.Skipped {
			v.ScoreDelta = score - baseScore
		}
		out = append(out, v)
	}
	// Baseline first, then worst-to-best by delta so the biggest regression is
	// adjacent to the number it regressed from.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsBaseline != out[j].IsBaseline {
			return out[i].IsBaseline
		}
		return out[i].ScoreDelta < out[j].ScoreDelta
	})
	return out
}

// --- cross-model arm comparison ----------------------------------------------

// BenchArmModelView is one model's verdict on one arm, averaged over the probes
// where that arm and its baseline both ran.
type BenchArmModelView struct {
	Model string `json:"model"`
	// Probes is the PAIRED count: probes where this arm and the baseline both
	// ran. It is the sample size behind the delta, not the model's probe total.
	Probes        int     `json:"probes"`
	BaselineScore float64 `json:"baselineScore"`
	ArmScore      float64 `json:"armScore"`
	ScoreDelta    float64 `json:"scoreDelta"`
	TokenDelta    int     `json:"tokenDelta" doc:"Evaluated prompt + completion tokens, arm minus baseline. Negative is cheaper."`
	WallDeltaMS   int64   `json:"wallDeltaMs"`
	Wins          int     `json:"wins" doc:"Probes where the arm beat the baseline."`
	Losses        int     `json:"losses"`
	Ties          int     `json:"ties"`
}

// BenchArmComparisonView is one arm judged across every model that ran it.
type BenchArmComparisonView struct {
	Label      string `json:"label"`
	Toolset    string `json:"toolset"`
	ToolFormat string `json:"toolFormat"`
	RunMode    string `json:"runMode"`

	// Models/Probes are the paired sample sizes the verdict rests on.
	Models int `json:"models"`
	Probes int `json:"probes"`
	// MeanScoreDelta averages over PROBES, not models, so a model benched on
	// twenty probes carries more weight than one benched on two — the pairing is
	// per probe and that is where the evidence actually is.
	MeanScoreDelta   float64 `json:"meanScoreDelta"`
	MedianScoreDelta float64 `json:"medianScoreDelta" doc:"Robust to one pathological probe dominating the mean."`
	MeanTokenDelta   int     `json:"meanTokenDelta"`
	Wins             int     `json:"wins"`
	Losses           int     `json:"losses"`
	Ties             int     `json:"ties"`

	ByModel []BenchArmModelView `json:"byModel"`
}

// BenchCapabilityArmsView groups arm comparisons under one capability.
type BenchCapabilityArmsView struct {
	Capability string `json:"capability"`
	// BaselineLabels lists the baseline arms these deltas are measured against.
	// Usually one; more than one means probes disagreed on which arm was the
	// baseline (e.g. some ran warm-only), and the deltas are per-probe pairings
	// rather than against a single global reference.
	BaselineLabels []string                 `json:"baselineLabels"`
	Arms           []BenchArmComparisonView `json:"arms"`
}

// BenchArmMatrixOutput is the cross-model A/B view.
type BenchArmMatrixOutput struct {
	Body struct {
		Capabilities []BenchCapabilityArmsView `json:"capabilities"`
	}
}

// armPair is one probe's paired observation: an arm against its own baseline.
type armPair struct {
	model      string
	baseScore  float64
	armScore   float64
	scoreDelta float64
	tokenDelta int
	wallDelta  int64
}

// BenchArmMatrix compares A/B arms across every model that ran them.
//
// The per-model view answers "did toon help THIS model"; this answers "does
// toon help at all". They are different questions and the second cannot be read
// off the first without pairing: an arm that only ever ran against the two
// models that happen to score well would otherwise look like an improvement it
// never made.
//
// So comparisons are PAIRED per probe — an arm is only credited on probes where
// its baseline also ran — and the paired counts are reported alongside the
// delta so a verdict resting on three probes cannot pass for one resting on
// sixty.
func (h *Handlers) BenchArmMatrix(ctx context.Context, _ *struct{}) (*BenchArmMatrixOutput, error) {
	out := &BenchArmMatrixOutput{}
	out.Body.Capabilities = []BenchCapabilityArmsView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.LatestBenchProbeResults(ctx)
	if err != nil {
		return out, err
	}
	type probeKey struct{ capability, model, probe string }
	byProbe := map[probeKey][]store.BenchProbeResult{}
	var probeOrder []probeKey
	for _, r := range rows {
		capName := r.Capability
		if capName == "" {
			capName = "chat"
		}
		k := probeKey{capName, r.Model, r.Probe}
		if _, seen := byProbe[k]; !seen {
			probeOrder = append(probeOrder, k)
		}
		byProbe[k] = append(byProbe[k], r)
	}

	// capability -> arm label -> paired observations
	pairs := map[string]map[string][]armPair{}
	meta := map[string]map[string]armKey{}
	baselines := map[string]map[string]bool{}
	var capOrder []string
	for _, k := range probeOrder {
		arms := armsFor(byProbe[k])
		var base *BenchArmView
		for i := range arms {
			if arms[i].IsBaseline && !arms[i].Skipped {
				base = &arms[i]
				break
			}
		}
		// No baseline ran for this probe, so nothing here can be paired against
		// one. Counting these as wins for whatever DID run is the selection bias
		// this pairing exists to prevent.
		if base == nil {
			continue
		}
		if pairs[k.capability] == nil {
			pairs[k.capability] = map[string][]armPair{}
			meta[k.capability] = map[string]armKey{}
			baselines[k.capability] = map[string]bool{}
			capOrder = append(capOrder, k.capability)
		}
		baselines[k.capability][base.Label] = true
		baseTokens := base.NewPromptTokens + base.CompletionTokens
		for i := range arms {
			a := arms[i]
			if a.IsBaseline || a.Skipped {
				continue
			}
			pairs[k.capability][a.Label] = append(pairs[k.capability][a.Label], armPair{
				model:      k.model,
				baseScore:  base.Score,
				armScore:   a.Score,
				scoreDelta: a.Score - base.Score,
				tokenDelta: (a.NewPromptTokens + a.CompletionTokens) - baseTokens,
				wallDelta:  a.WallMS - base.WallMS,
			})
			meta[k.capability][a.Label] = armKey{a.Toolset, a.ToolFormat, a.RunMode}
		}
	}

	sort.Strings(capOrder)
	for _, capName := range capOrder {
		entry := BenchCapabilityArmsView{Capability: capName, Arms: []BenchArmComparisonView{}}
		for label := range baselines[capName] {
			entry.BaselineLabels = append(entry.BaselineLabels, label)
		}
		sort.Strings(entry.BaselineLabels)

		var labels []string
		for label := range pairs[capName] {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			obs := pairs[capName][label]
			if len(obs) == 0 {
				continue
			}
			k := meta[capName][label]
			v := BenchArmComparisonView{
				Label: label, Toolset: k.toolset, ToolFormat: k.format, RunMode: k.runMode,
				Probes: len(obs), ByModel: []BenchArmModelView{},
			}
			byModel := map[string]*BenchArmModelView{}
			var modelOrder []string
			var deltas []float64
			sumScore, sumTokens := 0.0, 0
			for _, o := range obs {
				sumScore += o.scoreDelta
				sumTokens += o.tokenDelta
				deltas = append(deltas, o.scoreDelta)
				switch {
				case o.scoreDelta > 0:
					v.Wins++
				case o.scoreDelta < 0:
					v.Losses++
				default:
					v.Ties++
				}
				m := byModel[o.model]
				if m == nil {
					m = &BenchArmModelView{Model: o.model}
					byModel[o.model] = m
					modelOrder = append(modelOrder, o.model)
				}
				m.Probes++
				m.BaselineScore += o.baseScore
				m.ArmScore += o.armScore
				m.ScoreDelta += o.scoreDelta
				m.TokenDelta += o.tokenDelta
				m.WallDeltaMS += o.wallDelta
				switch {
				case o.scoreDelta > 0:
					m.Wins++
				case o.scoreDelta < 0:
					m.Losses++
				default:
					m.Ties++
				}
			}
			v.Models = len(modelOrder)
			v.MeanScoreDelta = sumScore / float64(len(obs))
			v.MeanTokenDelta = sumTokens / len(obs)
			sort.Float64s(deltas)
			mid := len(deltas) / 2
			if len(deltas)%2 == 1 {
				v.MedianScoreDelta = deltas[mid]
			} else {
				v.MedianScoreDelta = (deltas[mid-1] + deltas[mid]) / 2
			}
			sort.Strings(modelOrder)
			for _, name := range modelOrder {
				m := byModel[name]
				m.BaselineScore /= float64(m.Probes)
				m.ArmScore /= float64(m.Probes)
				m.ScoreDelta /= float64(m.Probes)
				m.TokenDelta /= m.Probes
				m.WallDeltaMS /= int64(m.Probes)
				v.ByModel = append(v.ByModel, *m)
			}
			entry.Arms = append(entry.Arms, v)
		}
		if len(entry.Arms) == 0 {
			continue
		}
		// Best mean delta first: the arm most worth adopting leads.
		sort.SliceStable(entry.Arms, func(i, j int) bool {
			return entry.Arms[i].MeanScoreDelta > entry.Arms[j].MeanScoreDelta
		})
		out.Body.Capabilities = append(out.Body.Capabilities, entry)
	}
	return out, nil
}

// --- per-probe stage + check detail ------------------------------------------

// BenchCheckView is one assertion's verdict.
type BenchCheckView struct {
	Idx    int    `json:"idx"`
	Kind   string `json:"kind"`
	Desc   string `json:"desc"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty" doc:"Free-text evidence or failure reason; checks carry no structured expected/actual."`
}

// BenchStageView is one stage of one arm: the prompt, the cost, the verdicts.
type BenchStageView struct {
	Stage         int              `json:"stage"`
	Prompt        string           `json:"prompt,omitempty"`
	Pass          bool             `json:"pass"`
	LimitBreached bool             `json:"limitBreached,omitempty" doc:"A turn/tool-call budget was hit. Does not by itself veto passing checks."`
	Note          string           `json:"note,omitempty"`
	Checks        []BenchCheckView `json:"checks"`

	Turns               int     `json:"turns"`
	ToolCalls           int     `json:"toolCalls"`
	NewPromptTokens     int     `json:"newPromptTokens"`
	CompletionTokens    int     `json:"completionTokens"`
	InvalidArgRetries   int     `json:"invalidArgRetries"`
	JSONErrors          int     `json:"jsonErrors"`
	RepeatedCalls       int     `json:"repeatedCalls"`
	BaitCalls           int     `json:"baitCalls" doc:"Calls to a declared bait tool — what adversarial probes are scored on."`
	BrokenIntermediates int     `json:"brokenIntermediates" doc:"Mutating calls that left the workspace failing its safety check."`
	Compactions         int     `json:"compactions"`
	TokPerSec           float64 `json:"tokPerSec"`
	WallMS              int64   `json:"wallMs"`
}

// BenchArmDetailView is one arm's full stage-by-stage record.
type BenchArmDetailView struct {
	Toolset    string           `json:"toolset"`
	ToolFormat string           `json:"toolFormat"`
	RunMode    string           `json:"runMode"`
	Label      string           `json:"label"`
	Stages     []BenchStageView `json:"stages"`
}

// BenchProbeDetailInput selects one probe of one model in one run.
type BenchProbeDetailInput struct {
	RunID string `query:"runId" required:"true"`
	Model string `query:"model" required:"true"`
	Probe string `query:"probe" required:"true"`
}

// BenchProbeDetailOutput is every arm's stages and checks for that probe.
type BenchProbeDetailOutput struct {
	Body struct {
		RunID string               `json:"runId"`
		Model string               `json:"model"`
		Probe string               `json:"probe"`
		Arms  []BenchArmDetailView `json:"arms"`
	}
}

// BenchProbeDetail returns the stage-by-stage evidence behind a probe's score.
func (h *Handlers) BenchProbeDetail(ctx context.Context, in *BenchProbeDetailInput) (*BenchProbeDetailOutput, error) {
	out := &BenchProbeDetailOutput{}
	out.Body.RunID, out.Body.Model, out.Body.Probe = in.RunID, in.Model, in.Probe
	out.Body.Arms = []BenchArmDetailView{}
	if h.Store == nil {
		return out, nil
	}
	stages, err := h.Store.BenchProbeStagesFor(ctx, in.RunID, in.Model, in.Probe)
	if err != nil {
		return out, err
	}
	checks, err := h.Store.BenchProbeChecksFor(ctx, in.RunID, in.Model, in.Probe)
	if err != nil {
		return out, err
	}
	type stageKey struct {
		arm   armKey
		stage int
	}
	checksBy := map[stageKey][]BenchCheckView{}
	for _, c := range checks {
		k := stageKey{armKey{c.Toolset, c.ToolFormat, c.RunMode}, c.Stage}
		checksBy[k] = append(checksBy[k], BenchCheckView{
			Idx: c.Idx, Kind: c.Kind, Desc: c.Desc, Pass: c.Pass, Detail: c.Detail,
		})
	}
	byArm := map[armKey]*BenchArmDetailView{}
	var order []armKey
	for _, s := range stages {
		k := armKey{s.Toolset, s.ToolFormat, s.RunMode}
		a := byArm[k]
		if a == nil {
			a = &BenchArmDetailView{
				Toolset: k.toolset, ToolFormat: k.format, RunMode: k.runMode,
				Label: armLabel(k), Stages: []BenchStageView{},
			}
			byArm[k] = a
			order = append(order, k)
		}
		cs := checksBy[stageKey{k, s.Stage}]
		if cs == nil {
			cs = []BenchCheckView{}
		}
		a.Stages = append(a.Stages, BenchStageView{
			Stage: s.Stage, Prompt: s.Prompt, Pass: s.Pass,
			LimitBreached: s.LimitBreached, Note: s.Note, Checks: cs,
			Turns: s.Turns, ToolCalls: s.ToolCalls, NewPromptTokens: s.NewPromptTokens,
			CompletionTokens: s.CompletionTokens, InvalidArgRetries: s.InvalidArgRetries,
			JSONErrors: s.JSONErrors, RepeatedCalls: s.RepeatedCalls,
			BaitCalls: s.BaitCalls, BrokenIntermediates: s.BrokenIntermediates,
			Compactions: s.Compactions, TokPerSec: s.TokPerSec, WallMS: s.WallMS,
		})
	}
	base := pickBaseline(order)
	sort.SliceStable(order, func(i, j int) bool { return order[i] == base && order[j] != base })
	for _, k := range order {
		out.Body.Arms = append(out.Body.Arms, *byArm[k])
	}
	return out, nil
}

// --- transcripts and journals (read from out/<ts>/) --------------------------

// maxArtifactEntries caps how much replay one request returns. A transcript is
// bounded (2 KiB/entry) but a journal is not, and an unbounded read would let a
// single pathological run's artifacts dominate a response.
const maxArtifactEntries = 2000

// BenchArtifactInput selects one probe's on-disk artifacts.
type BenchArtifactInput struct {
	RunID   string `query:"runId" required:"true"`
	Model   string `query:"model" required:"true"`
	Probe   string `query:"probe" required:"true"`
	Toolset string `query:"toolset" doc:"Arm's toolset; defaults to \"baseline\"."`
	RunMode string `query:"runMode" doc:"Arm's residency mode (cold | warm). A run:both probe writes one file per mode; without this you get whichever pass is named plainly."`
	Run     int    `query:"run" doc:"Repeat index for --runs; 0 is the first pass."`
}

// BenchTranscriptEntry is one message in a probe's conversation.
type BenchTranscriptEntry struct {
	Kind       string `json:"kind" doc:"user | assistant | tool_call | tool_result."`
	ToolName   string `json:"toolName,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Content    string `json:"content" doc:"Truncated to 2 KiB per entry by the writer."`
	CreatedAt  int64  `json:"createdAt"`
}

// BenchTranscriptOutput is the message history, or why it is unavailable.
type BenchTranscriptOutput struct {
	Body struct {
		RunID     string                 `json:"runId"`
		Available bool                   `json:"available"`
		Reason    string                 `json:"reason,omitempty" doc:"Why the transcript could not be read."`
		Truncated bool                   `json:"truncated,omitempty"`
		Entries   []BenchTranscriptEntry `json:"entries"`
	}
}

// BenchJournalEntry is one tool call as the MCP helper recorded it.
type BenchJournalEntry struct {
	TS          int64  `json:"ts" doc:"Unix nanoseconds."`
	Tool        string `json:"tool"`
	Args        string `json:"args" doc:"Raw JSON arguments."`
	ResultBytes int    `json:"resultBytes"`
	Poisoned    bool   `json:"poisoned" doc:"The tool returned content carrying a prompt-injection payload."`
	Bait        bool   `json:"bait" doc:"A declared bait tool the model should not have called."`
}

// BenchJournalOutput is the tool-call journal, or why it is unavailable.
type BenchJournalOutput struct {
	Body struct {
		RunID     string              `json:"runId"`
		Available bool                `json:"available"`
		Reason    string              `json:"reason,omitempty"`
		Truncated bool                `json:"truncated,omitempty"`
		Entries   []BenchJournalEntry `json:"entries"`
	}
}

// artifactPath resolves the on-disk file for one probe artifact.
//
// The filename is built server-side from judge.ComboName, never taken from the
// caller: the request names a model/toolset/probe, and this maps them to a path
// under the run's recorded out dir. Accepting a path would make this a file-read
// primitive for anyone who can reach the API. The containment check below is the
// backstop for a name that sanitizes into traversal anyway.
func (h *Handlers) artifactPath(ctx context.Context, kind string, in *BenchArtifactInput) (string, string) {
	if h.Store == nil {
		return "", "no store"
	}
	run, ok, err := h.Store.BenchRunFor(ctx, in.RunID)
	if err != nil {
		return "", err.Error()
	}
	if !ok || run.OutDir == "" {
		return "", fmt.Sprintf("run %s did not record where its artifacts were written", in.RunID)
	}
	if host, _ := os.Hostname(); run.Host != "" && host != "" && run.Host != host {
		// Say so rather than returning an empty transcript, which would read as
		// "the model said nothing" instead of "this box cannot see that file".
		return "", fmt.Sprintf("run %s was benched on %s; its artifacts are not on this host", in.RunID, run.Host)
	}
	toolset := in.Toolset
	if toolset == "" {
		toolset = "baseline"
	}
	root := filepath.Clean(run.OutDir)
	// Exact arm first, then the bare combo. The fallback is what lets runs
	// benched before the mode was in the filename still resolve; without it
	// every historical artifact would report as missing.
	candidates := []string{judge.ComboVariant(in.Model, toolset, in.Probe, in.RunMode, in.Run)}
	if in.RunMode != "" || in.Run != 0 {
		candidates = append(candidates, judge.ComboName(in.Model, toolset, in.Probe))
	}
	for _, combo := range candidates {
		p := filepath.Join(root, kind, combo+".jsonl")
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return "", "resolved artifact path escapes the run directory"
		}
		if _, err := os.Stat(p); err == nil {
			return p, ""
		}
	}
	return "", fmt.Sprintf("no %s recorded for %s under toolset %q runMode %q",
		kind, in.Probe, toolset, in.RunMode)
}

// readJSONL streams a JSONL artifact, calling fn per line, capped.
func readJSONL(path string, fn func([]byte) error) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	// Entries can exceed bufio's 64 KiB default; a long tool result would
	// otherwise abort the scan mid-file and silently truncate the replay.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if n >= maxArtifactEntries {
			return true, nil
		}
		if err := fn(line); err != nil {
			return false, err
		}
		n++
	}
	return false, sc.Err()
}

// BenchTranscript returns a probe's conversation as recorded during the run.
func (h *Handlers) BenchTranscript(ctx context.Context, in *BenchArtifactInput) (*BenchTranscriptOutput, error) {
	out := &BenchTranscriptOutput{}
	out.Body.RunID = in.RunID
	out.Body.Entries = []BenchTranscriptEntry{}
	path, reason := h.artifactPath(ctx, "transcripts", in)
	if path == "" {
		out.Body.Reason = reason
		return out, nil
	}
	truncated, err := readJSONL(path, func(line []byte) error {
		var e struct {
			Kind       string `json:"kind"`
			ToolName   string `json:"toolName"`
			ToolCallID string `json:"toolCallId"`
			Content    string `json:"content"`
			CreatedAt  int64  `json:"createdAt"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return nil // a malformed line is not worth failing the whole replay
		}
		out.Body.Entries = append(out.Body.Entries, BenchTranscriptEntry(e))
		return nil
	})
	if err != nil {
		out.Body.Reason = err.Error()
		return out, nil
	}
	out.Body.Available = true
	out.Body.Truncated = truncated
	return out, nil
}

// BenchJournal returns a probe's tool-call journal.
func (h *Handlers) BenchJournal(ctx context.Context, in *BenchArtifactInput) (*BenchJournalOutput, error) {
	out := &BenchJournalOutput{}
	out.Body.RunID = in.RunID
	out.Body.Entries = []BenchJournalEntry{}
	path, reason := h.artifactPath(ctx, "journals", in)
	if path == "" {
		out.Body.Reason = reason
		return out, nil
	}
	truncated, err := readJSONL(path, func(line []byte) error {
		var e struct {
			TS          int64           `json:"ts"`
			Tool        string          `json:"tool"`
			Args        json.RawMessage `json:"args"`
			ResultBytes int             `json:"resultBytes"`
			Poisoned    bool            `json:"poisoned"`
			Bait        bool            `json:"bait"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return nil
		}
		out.Body.Entries = append(out.Body.Entries, BenchJournalEntry{
			TS: e.TS, Tool: e.Tool, Args: string(e.Args), ResultBytes: e.ResultBytes,
			Poisoned: e.Poisoned, Bait: e.Bait,
		})
		return nil
	})
	if err != nil {
		out.Body.Reason = err.Error()
		return out, nil
	}
	out.Body.Available = true
	out.Body.Truncated = truncated
	return out, nil
}
