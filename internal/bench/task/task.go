// Package task defines the llm-bench task.yaml schema, its loader, and
// validation. A task is a directory under tasks/<name>/ holding a task.yaml
// plus a fixture/ workspace seed. See tasks/README.md for the field reference.
package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Task is one benchmark scenario loaded from tasks/<name>/task.yaml.
type Task struct {
	// Dir is the absolute directory the task was loaded from (not from YAML).
	Dir string `yaml:"-"`

	Name      string       `yaml:"name"`
	Class     string       `yaml:"class"`     // coding | tooluse | adversarial
	Workspace string       `yaml:"workspace"` // dir (relative to Dir) copied into the scratch workspace
	Limits    Limits       `yaml:"limits"`
	BaitTools []BaitTool   `yaml:"baitTools"`
	Poison    []PoisonRule `yaml:"poison"`
	Stages    []Stage      `yaml:"stages"`

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
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("check must be a single-key mapping, got %d keys", len(node.Content)/2)
	}
	key := node.Content[0].Value
	val := node.Content[1]
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

var validClasses = map[string]bool{"coding": true, "tooluse": true, "adversarial": true}

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
	if t.Limits.MaxTurnsPerStage == 0 {
		t.Limits.MaxTurnsPerStage = defaultMaxTurns
	}
	if t.Limits.MaxToolCallsPerStage == 0 {
		t.Limits.MaxToolCallsPerStage = defaultMaxToolCalls
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Adversarial reports whether the task is in the adversarial class (run last).
func (t *Task) Adversarial() bool { return t.Class == "adversarial" }

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
	if t.Workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if fi, err := os.Stat(t.WorkspaceDir()); err != nil || !fi.IsDir() {
		return fmt.Errorf("workspace dir %q does not exist", t.Workspace)
	}
	if len(t.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
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
	default:
		return fmt.Errorf("unknown check kind %q", c.Kind)
	}
	return nil
}
