// Package task defines the llm-bench task.yaml schema, its loader, and
// validation. A task is a directory under tasks/<name>/ holding a task.yaml
// plus a fixture/ workspace seed. See tasks/README.md for the field reference.
package task

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/agentkit/llm"
)

// Task is one benchmark scenario loaded from tasks/<name>/task.yaml.
type Task struct {
	// Dir is the absolute directory the task was loaded from (not from YAML).
	Dir string `yaml:"-"`

	Name string `yaml:"name"`
	// Description is human prose shown in the UI and never sent to the model.
	// Markdown probes get it from the text above the first `##` heading;
	// task.yaml declares it explicitly. Optional in both.
	Description string `yaml:"description"`
	Class       string `yaml:"class"` // coding | tooluse | adversarial | capability

	// Weight is how much this probe counts toward its CLASS score. Default 1.
	//
	// Probes are not equal. Renaming a type across five files and tracing a
	// reference chain through 8,300 lines are both one probe and one row, and
	// averaging them flat says a model that only manages the easy one is half
	// as good — when what it is, is good at easy ones. The weight is where that
	// judgement goes, stated by whoever wrote the probe and knows what it cost
	// to pass.
	//
	// A box may disagree: `weights: {probe: n}` in the bench config overrides
	// this, because what a score should REFLECT is the reader's opinion, not
	// the author's. Same rule as probeDirs and toolsets.
	//
	// 0 keeps a probe running while excluding it from the score — how you park
	// one that is unreliable without losing its rows.
	Weight *float64 `yaml:"weight"`

	// StageFold is how a probe's stages combine into one grade:
	//
	//   worst (default) — a SEQUENTIAL task. Diagnose, fix, verify: a run that
	//     breaks the build at the last step did not do the job, and calling it
	//     two-thirds capable would let a probe launder a late failure.
	//   mean — INDEPENDENT dimensions. mcpshell-instructions tests `vars`, then
	//     `export`, then `help()`; they share a probe for convenience and
	//     nothing else. Under worst, getting two of three right scores the same
	//     as getting none — which reported a real A/B as a dead wash the first
	//     time it ran against a model.
	//
	// Harm dominates under either: a stage that did damage floors the probe,
	// because a mean would let two good stages average away a delete_repo.
	StageFold string `yaml:"stageFold"`

	// Axis says which experiment this probe belongs to:
	//
	//   toolset (default) — the probe can tell toolsets APART, so it runs on
	//     every arm and its deltas are the measurement.
	//   model — the probe measures the MODEL and no toolset can change its
	//     answer, so it runs on the baseline arm ONLY.
	//
	// Measured on a real 26-probe run: only TWO probes produced different
	// outcomes across arms. Every other probe agreed in all three, costing 29.3
	// minutes of redundant arms against 0.7 minutes of real measurement — 63%
	// of the run's model time bought no signal, and one image probe alone
	// burned 20 of those minutes re-answering a question no toolset touches.
	//
	// `class: capability` implies model-axis by default: such a probe verifies
	// a declared MODALITY against the live backend, and whether a model can see
	// pixels does not depend on which tools are advertised beside it. An
	// explicit axis always wins.
	Axis      string       `yaml:"axis"`
	Workspace string       `yaml:"workspace"` // dir (relative to Dir) copied into the scratch workspace
	Limits    Limits       `yaml:"limits"`
	BaitTools []BaitTool   `yaml:"baitTools"`
	Poison    []PoisonRule `yaml:"poison"`
	Stages    []Stage      `yaml:"stages"`

	// SafetyCheck, when set, is a shell command run in the scratch workspace
	// AFTER every mutating tool call (write_file / node_edit / node_delete /
	// node_refactor / node_rename_file). A non-zero exit means the model left
	// the workspace in a BROKEN state on disk; each such occurrence increments
	// the row's brokenIntermediates. Use a fast compile check, e.g.
	// "go build ./...". Empty = the safety metric is not measured for this task.
	SafetyCheck string `yaml:"safetyCheck"`

	// System REPLACES the runner's base system prompt entirely for this task.
	//
	// Appending is not always enough: the base prompt says "do not ask the user
	// questions", and codex-plan-3-violation REQUIRES ask_user_question. Its
	// systemAppend told the model to escalate, the base prompt told it not to,
	// and the model obeyed the base — so `tool_called: ask_user_question` failed
	// 8/8 across every arm and every run. A check no arm can pass looks like a
	// hard task rather than a broken one, which is exactly why it went unnoticed.
	//
	// So a task that needs to contradict the base prompt must be able to say so,
	// not fight it. Empty = keep the base prompt.
	System string `yaml:"system"`

	// SystemAppend, when set, is appended (a blank line then this text) after
	// System (or after the base prompt when System is empty) — used to establish
	// a task-class persona (e.g. the initiative/decisiveness tasks tell the model
	// to act autonomously and only escalate on genuinely ambiguous, consequential
	// decisions). Empty = no append. Composes with System.
	SystemAppend string `yaml:"systemAppend"`

	// ContextBudget optionally overrides the global agentkit Shaper token budget
	// for this task's session (e.g. a small budget to force LOD truncation +
	// compaction for a compaction-continuation task). 0 = use the global budget.
	// When set it must be >= 2000 (below that the Shaper cannot keep a usable
	// pristine tail).
	ContextBudget int `yaml:"contextBudget"`

	// Audio, when set, makes this probe drive an AUDIO surface directly instead
	// of a chat session. STT and TTS are not conversations — multipart upload
	// in, binary audio out — so the agent loop has nothing to do with them and
	// a probe that needs them cannot be expressed as a prompt.
	Audio *AudioProbe `yaml:"audio"`

	// Run selects the residency state this probe runs against:
	// "" (any, the default -- residency untouched) | warm.
	// Cold mode was removed with the exclusive lease: arranging a cold model
	// means EVICTING one, which is a cost every other caller on the box pays.
	// See probes/README.md for what that gives up.
	Run string `yaml:"run"`

	// Requires declares what a model must ALREADY claim for this probe to be
	// meaningful. A model that does not satisfy it is SKIPPED, not failed:
	// a text-only model has not failed a vision probe, it was never a
	// candidate. Scoring it as a failure is the same category error as
	// letting a turn cap veto passing checks.
	Requires Requires `yaml:"requires"`
}

// AudioProbe drives one audio endpoint. Exactly one direction is set.
type AudioProbe struct {
	// Transcribe is a workspace-relative audio file POSTed to
	// /v1/audio/transcriptions. The transcript becomes the probe's "response",
	// so the ordinary response_contains / python checks apply unchanged.
	Transcribe string `yaml:"transcribe"`

	// Speak is text POSTed to /v1/audio/speech. The synthesized bytes are not
	// text, so checks see audio_bytes and audio_format instead of a response —
	// asserting on the CONTENT of generated speech would need an STT round trip,
	// which is what the round-trip probe does explicitly rather than by magic.
	Speak string `yaml:"speak"`
	Voice string `yaml:"voice"`
	// Format requests a container ("wav" | "mp3"). wav matters when the output
	// is fed back into STT.
	Format string `yaml:"format"`

	// ThenTranscribe feeds the synthesized audio straight back into an STT
	// model, so one probe exercises both directions and the transcript can be
	// compared against the text that produced it. That round trip is the only
	// way to assert TTS actually said something intelligible rather than
	// emitting a well-formed blob of silence.
	ThenTranscribe string `yaml:"thenTranscribe"`
}

// Requires gates a probe on a model's declared capabilities.
type Requires struct {
	// Modality names a modality the model must declare (image | audio | text).
	// Matched against the model's corrallm `modalities` declaration.
	Modality string `yaml:"modality"`

	// Capability names the SERVING SURFACE this probe needs: chat, audio.stt,
	// audio.tts, embeddings. Empty means chat, since every probe written before
	// this field existed drives a chat session.
	//
	// Modality alone is not enough. A coding probe is text-shaped and an STT
	// backend declares the text modality too, so `requires: {modality: text}`
	// happily matches an endpoint that cannot hold a conversation. That is not
	// theoretical: a UI-triggered run put all 13 chat probes against stt, tts,
	// stt-diarize and realtime-stt, which scored 1/21 apiece and published
	// results that mean nothing.
	Capability string `yaml:"capability"`
}

// EffectiveCapability returns the serving surface this probe needs, defaulting
// to chat.
func (r Requires) EffectiveCapability() string {
	if r.Capability == "" {
		return "chat"
	}
	return r.Capability
}

// Limits bounds a looping model so a bad run burns bounded tokens. A zero
// value means "unbounded" for that dimension, but the loader defaults them.
type Limits struct {
	MaxTurnsPerStage     int `yaml:"maxTurnsPerStage"`
	MaxToolCallsPerStage int `yaml:"maxToolCallsPerStage"`
}

// BaitTool is a tool the model MUST NOT call. llm-bench-mcp registers it,
// returns a benign canned string, and journals every call so tool_not_called
// checks can assert it never fired.
type BaitTool struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
}

// PoisonRule splices instruction-bearing text into a designated tool RESULT.
type PoisonRule struct {
	Into  string `yaml:"into" json:"into"`   // tool name whose results are poisoned
	Match string `yaml:"match" json:"match"` // only when the call's args JSON contains this substring ("" = always)
	Text  string `yaml:"text" json:"text"`   // appended to the tool result
}

// Stage is one user prompt plus the checks evaluated after it. Stages run
// sequentially in ONE session; context carries across them.
type Stage struct {
	Prompt string  `yaml:"prompt"`
	Checks []Check `yaml:"checks"`
	// Parts, when non-empty, sends this stage's prompt as MULTIMODAL content
	// (text + images) instead of a plain string. Populated by markdown probes
	// from ![](path) syntax; task.yaml has no syntax for it. Requires
	// agentkit's Entry.Parts to reach the model.
	Parts []llm.ContentPart `yaml:"-"`
	// ForceCompact folds the session history (agentkit Shaper.Compact) BEFORE
	// this stage's prompt runs, so a compaction-continuation task deterministically
	// exercises recall-across-compaction instead of hoping budget pressure trips
	// a fold. A no-op when nothing summarizable remains.
	ForceCompact bool `yaml:"forceCompact"`
}

// Check is a single deterministic assertion. In YAML each check is a
// single-key mapping whose key names the kind:
//
//   - cmd_ok: "go test ./..."
//   - file_contains: { path: notes.md, text: "port 8080" }
//   - file_absent: { path: ../secret }
//   - tool_called: { name: write_file, argContains: "main.go", min: 1, max: 3 }
//   - tool_not_called: { name: exfiltrate_env }
//   - no_repeat_calls: { n: 2 }
//   - compactions_min: 1     (scalar int; cumulative Shaper compactions >= N)
//   - compaction_under: 1500 (scalar int; stage's compactionTokensAfter >0 and <= N)
type Check struct {
	Kind string `json:"kind"`

	// Harm marks a check whose FAILURE means the model did something harmful,
	// as opposed to failing to do something useful. The distinction is the
	// whole reason a score is signed: not fixing the bug is 0, firing
	// delete_repo is -1, and collapsing those into one "fail" loses the only
	// difference that matters.
	//
	// Author-declared, never inferred from the kind. A negative assertion is
	// not reliably a harm assertion — `file_absent` can mean "cleaned up the
	// temp file" (capability) or "did not write pwned.txt" (harm) — and
	// guessing wrong reports harm that did not happen, which is worse than
	// reporting none.
	Harm bool `json:"harm,omitempty"`

	Cmd string `json:"cmd,omitempty"` // cmd_ok

	Path string `json:"path,omitempty"` // file_contains / file_absent
	Text string `json:"text,omitempty"` // file_contains

	Name        string `json:"name,omitempty"`        // tool_called / tool_not_called
	ArgContains string `json:"argContains,omitempty"` // tool_called / tool_not_called
	Min         *int   `json:"min,omitempty"`         // tool_called
	Max         *int   `json:"max,omitempty"`         // tool_called

	N int `json:"n,omitempty"` // no_repeat_calls (default 2)
}

// UnmarshalYAML decodes the single-key-mapping check shape into a flat Check.
func (c *Check) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("check must be a mapping")
	}
	// The kind key, plus an OPTIONAL `harm` beside it. Beside rather than
	// inside, because half the kinds take a scalar (`cmd_ok: go build`,
	// `response_not_contains: SECRET`) and have nowhere to put a sub-key —
	// and a harm assertion on a leaked secret is exactly the case that must
	// not be the awkward one to write.
	var key string
	var val *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i].Value
		if k == "harm" {
			if err := node.Content[i+1].Decode(&c.Harm); err != nil {
				return fmt.Errorf("harm: %w", err)
			}
			continue
		}
		if key != "" {
			return fmt.Errorf("check has two kinds (%s and %s); one per list item", key, k)
		}
		key, val = k, node.Content[i+1]
	}
	if key == "" {
		return fmt.Errorf("check names no kind")
	}
	c.Kind = key
	switch key {
	case "cmd_ok":
		return val.Decode(&c.Cmd)
	case "file_contains", "file_absent":
		var m struct {
			Path string `yaml:"path"`
			Text string `yaml:"text"`
		}
		if err := val.Decode(&m); err != nil {
			return err
		}
		c.Path, c.Text = m.Path, m.Text
	case "tool_called", "tool_not_called":
		var m struct {
			Name        string `yaml:"name"`
			ArgContains string `yaml:"argContains"`
			Min         *int   `yaml:"min"`
			Max         *int   `yaml:"max"`
		}
		if err := val.Decode(&m); err != nil {
			return err
		}
		c.Name, c.ArgContains, c.Min, c.Max = m.Name, m.ArgContains, m.Min, m.Max
	case "no_repeat_calls":
		var m struct {
			N int `yaml:"n"`
		}
		if err := val.Decode(&m); err != nil {
			return err
		}
		c.N = m.N
	case "compactions_min", "compaction_under":
		// Scalar int: `compactions_min: 1` / `compaction_under: 1500`.
		if err := val.Decode(&c.N); err != nil {
			return err
		}
	case "python":
		// A scripted predicate. Block scalar in YAML/markdown:
		//   - python: |
		//       if "red" not in response.lower(): fail("expected red")
		if err := val.Decode(&c.Text); err != nil {
			return err
		}
	case "judge":
		// A rubric sentence, graded by the P1 judge model rather than computed
		// here. The only kind whose verdict is an OPINION, which is why it is
		// author-declared per assertion instead of a blanket quality score: the
		// probe says exactly what it wants judged, and the judge answers that
		// question and no other.
		if err := val.Decode(&c.Text); err != nil {
			return err
		}
	case "response_contains", "response_not_contains":
		// Scalar string: `response_contains: red`. Asserts on the model's
		// VISIBLE reply text — the only check kind that does, which is what
		// makes capability probing possible at all: "describe this image"
		// writes no file and calls no tool, so every other kind has nothing
		// to read.
		if err := val.Decode(&c.Text); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown check kind %q", key)
	}
	return nil
}

// TaskSpec is the subset of a Task the runner serializes to JSON for
// llm-bench-mcp (bait tools + poison rules). Workspace jail root, binary
// allowlist and journal path are passed as flags instead.
type TaskSpec struct {
	BaitTools []BaitTool   `json:"baitTools"`
	Poison    []PoisonRule `json:"poison"`
}

// Spec projects a Task onto its TaskSpec.
func (t *Task) Spec() TaskSpec {
	return TaskSpec{BaitTools: t.BaitTools, Poison: t.Poison}
}

// WriteSpec writes t's TaskSpec as JSON to path.
func (t *Task) WriteSpec(path string) error {
	b, err := json.MarshalIndent(t.Spec(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadSpec reads a TaskSpec JSON file (used by llm-bench-mcp).
func LoadSpec(path string) (TaskSpec, error) {
	var s TaskSpec
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse taskspec %s: %w", path, err)
	}
	return s, nil
}

const (
	defaultMaxTurns     = 8
	defaultMaxToolCalls = 24
)

// capability: does the model do what it CLAIMS (modalities, formats, tool
// calling)? Cheap and deterministic, unlike the quality-oriented classes.
// applyDefaults fills the zero-valued knobs a loader may leave unset. Shared by
// the task.yaml and probe.md loaders so the two formats cannot drift on
// defaults — a markdown probe silently getting different turn limits than the
// equivalent YAML would make the formats non-equivalent, which is the one
// property the markdown format must preserve.
func applyDefaults(t *Task) {
	if t.Limits.MaxTurnsPerStage == 0 {
		t.Limits.MaxTurnsPerStage = defaultMaxTurns
	}
	if t.Limits.MaxToolCallsPerStage == 0 {
		t.Limits.MaxToolCallsPerStage = defaultMaxToolCalls
	}
}

var validClasses = map[string]bool{"coding": true, "tooluse": true, "adversarial": true, "capability": true}

// LoadDir loads a probe directory in EITHER format: task.yaml if present,
// otherwise probe.md. Returns os.ErrNotExist if the dir holds neither, so
// callers can skip non-probe directories.
//
// task.yaml wins when both exist. That is arbitrary but must be deterministic —
// silently running a different probe than the author edited is worse than
// either choice.
func LoadDir(dir string) (*Task, error) {
	if _, err := os.Stat(filepath.Join(dir, "task.yaml")); err == nil {
		return Load(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ProbeFile)); err == nil {
		return LoadMarkdown(dir)
	}
	return nil, fmt.Errorf("%s: no task.yaml or %s: %w", dir, ProbeFile, os.ErrNotExist)
}

// Load reads and validates tasks/<name>/task.yaml under dir.
func Load(dir string) (*Task, error) {
	path := filepath.Join(dir, "task.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := yaml.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	t.Dir = abs
	applyDefaults(&t)
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Adversarial reports whether the task is in the adversarial class (run last).
func (t *Task) Adversarial() bool { return t.Class == "adversarial" }

// The two experiments a probe can belong to. See Task.Axis.
const (
	AxisModel   = "model"
	AxisToolset = "toolset"
)

// EffectiveAxis is the probe's declared axis, defaulted.
//
// A capability probe defaults to the MODEL axis: it verifies a declared
// modality against the live backend, and no toolset changes whether a model can
// see an image. Everything else defaults to the toolset axis, which preserves
// the old behaviour — running on every arm — so an unannotated probe can never
// silently stop covering an arm where a real regression might hide.
func (t *Task) EffectiveAxis() string {
	if t.Axis != "" {
		return t.Axis
	}
	if t.Class == "capability" {
		return AxisModel
	}
	return AxisToolset
}

// RunsOnToolset reports whether this probe should run against a non-baseline
// arm. A model-axis probe runs on the baseline only: its answer cannot differ
// per arm, so the extra runs cost time and add identical rows that clutter a
// table meant to show differences.
func (t *Task) RunsOnToolset(toolset string) bool {
	if t.EffectiveAxis() == AxisToolset {
		return true
	}
	return toolset == "baseline" || toolset == ""
}

// DefaultWeight is what a probe counts for when it says nothing.
const DefaultWeight = 1.0

// EffectiveWeight is the probe's declared weight, defaulted. A pointer field
// distinguishes "unset" from an explicit 0, which are different intentions:
// unset means ordinary, 0 means run it but keep it out of the score.
func (t *Task) EffectiveWeight() float64 {
	if t.Weight == nil {
		return DefaultWeight
	}
	return *t.Weight
}

// WorkspaceDir is the absolute path to the fixture directory to seed from.
func (t *Task) WorkspaceDir() string { return filepath.Join(t.Dir, t.Workspace) }

// Validate checks the loaded task for structural errors.
func (t *Task) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !validClasses[t.Class] {
		return fmt.Errorf("class %q invalid (want coding|tooluse|adversarial)", t.Class)
	}
	// Workspace is optional. A capability probe ("describe this image") needs
	// no fixture at all; requiring one would force every such probe to carry an
	// empty directory purely to satisfy the validator. When unset the runner
	// gets an empty scratch dir.
	if t.Workspace != "" {
		if fi, err := os.Stat(t.WorkspaceDir()); err != nil || !fi.IsDir() {
			return fmt.Errorf("workspace dir %q does not exist", t.Workspace)
		}
	}
	switch t.StageFold {
	case "", "worst", "mean":
	default:
		return fmt.Errorf("stageFold %q invalid (want worst or mean)", t.StageFold)
	}
	switch t.Axis {
	case "", AxisModel, AxisToolset:
	default:
		return fmt.Errorf("axis %q invalid (want %s or %s)", t.Axis, AxisModel, AxisToolset)
	}
	if t.Weight != nil && (*t.Weight < 0 || math.IsNaN(*t.Weight) || math.IsInf(*t.Weight, 0)) {
		return fmt.Errorf("weight %v invalid (want >= 0; 0 excludes the probe from the score)", *t.Weight)
	}
	if len(t.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
	}
	if t.Audio != nil {
		a := t.Audio
		if a.Transcribe == "" && a.Speak == "" {
			return fmt.Errorf("audio: set transcribe (a file) or speak (text)")
		}
		if a.Transcribe != "" && a.Speak != "" {
			return fmt.Errorf("audio: set transcribe OR speak, not both — one probe drives one direction")
		}
		if a.ThenTranscribe != "" && a.Speak == "" {
			return fmt.Errorf("audio: thenTranscribe needs speak (there is nothing to feed back otherwise)")
		}
	}
	switch t.Run {
	case "", "warm":
	case "cold", "both":
		// Named explicitly so an existing probe gets an explanation rather than
		// a bare "invalid" for a value that used to be correct.
		return fmt.Errorf("run %q is no longer supported: cold mode needed eviction rights, which were removed with the exclusive lease (want warm, or omit)", t.Run)
	default:
		return fmt.Errorf("run %q invalid (want warm, or omit)", t.Run)
	}
	if t.ContextBudget != 0 && t.ContextBudget < 2000 {
		return fmt.Errorf("contextBudget %d is too small (must be >= 2000 when set)", t.ContextBudget)
	}
	baitNames := map[string]bool{}
	for i, b := range t.BaitTools {
		if b.Name == "" {
			return fmt.Errorf("baitTools[%d]: name is required", i)
		}
		baitNames[b.Name] = true
	}
	for i, p := range t.Poison {
		if p.Into == "" {
			return fmt.Errorf("poison[%d]: into is required", i)
		}
		if p.Text == "" {
			return fmt.Errorf("poison[%d]: text is required", i)
		}
	}
	for i, s := range t.Stages {
		if s.Prompt == "" {
			return fmt.Errorf("stages[%d]: prompt is required", i)
		}
		for j, c := range s.Checks {
			if err := c.validate(); err != nil {
				return fmt.Errorf("stages[%d].checks[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

func (c *Check) validate() error {
	switch c.Kind {
	case "cmd_ok":
		if c.Cmd == "" {
			return fmt.Errorf("cmd_ok: command is required")
		}
	case "file_contains":
		if c.Path == "" || c.Text == "" {
			return fmt.Errorf("file_contains: path and text are required")
		}
	case "file_absent":
		if c.Path == "" {
			return fmt.Errorf("file_absent: path is required")
		}
	case "tool_called", "tool_not_called":
		if c.Name == "" {
			return fmt.Errorf("%s: name is required", c.Kind)
		}
	case "no_repeat_calls":
		// n defaults later; nothing required
	case "compactions_min":
		if c.N < 1 {
			return fmt.Errorf("compactions_min: value must be >= 1 (a compactions_min:0 check is vacuous)")
		}
	case "compaction_under":
		if c.N < 1 {
			return fmt.Errorf("compaction_under: bound must be >= 1")
		}
	case "python":
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("python: script is required")
		}
	case "judge":
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("judge: an assertion to grade is required")
		}
	case "response_contains", "response_not_contains":
		if c.Text == "" {
			// An empty needle matches everything, so a positive check would be
			// vacuous and a prohibition unsatisfiable. Both are author errors.
			return fmt.Errorf("%s: text is required", c.Kind)
		}
	default:
		return fmt.Errorf("unknown check kind %q", c.Kind)
	}
	return nil
}
