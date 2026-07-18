package run

import (
	"fmt"
	"os"

	"github.com/iodesystems/agentkit/agent/toolfmt"
	"gopkg.in/yaml.v3"
)

// DefaultToolResultFormat is the baseline: no re-encoding (raw JSON results).
const DefaultToolResultFormat = "json"

// EncoderFor maps a tool-result format name to its agentkit toolfmt encoder.
// "json" (the baseline) returns a nil encoder — Session.EncodeToolResult stays
// nil, so results pass through unchanged. An unknown format is an error, so it
// can be rejected at startup.
func EncoderFor(format string) (encode func(raw string) string, err error) {
	switch format {
	case "", "json":
		return nil, nil
	case "tightc":
		return toolfmt.EncodeTightC, nil
	default:
		return nil, fmt.Errorf("unknown tool-result format %q (want json|tightc)", format)
	}
}

// EffectiveToolResultFormat returns the configured format, defaulted to json.
func (c Config) EffectiveToolResultFormat() string {
	if c.ToolResultFormat == "" {
		return DefaultToolResultFormat
	}
	return c.ToolResultFormat
}

// Config is llm-bench.yaml: the LLM endpoint, the models under test, the named
// toolset variants (each a list of MCP servers spawned alongside llm-bench-mcp),
// and the P1 judge settings.
type Config struct {
	LLM      LLMConfig       `yaml:"llm"`
	Models   []string        `yaml:"models"`
	Toolsets OrderedToolsets `yaml:"toolsets"`
	Judge    JudgeConfig     `yaml:"judge"`

	// ToolResultFormat re-encodes tool-call RESULTS before they enter the LLM's
	// context, as a measured axis: json (baseline, no re-encoding) | toon | csv
	// | json-toon | loose | tight | tight-lift. Default "json". A --tool-format
	// flag overrides. "tight-lift" is tight plus a byte-probed subtree-hoisting
	// add-in.
	ToolResultFormat string `yaml:"toolResultFormat"`
}

// JudgeConfig configures the P1 judge phase.
type JudgeConfig struct {
	Model              string `yaml:"model"`              // judge model / corrallm lane (default "chat")
	MaxTranscriptBytes int    `yaml:"maxTranscriptBytes"` // transcript truncation budget (default 65536)
}

// LLMConfig points at the corrallm endpoint. APIKeyEnv names the env var
// holding the scheduling-identity bearer key (empty env = no auth).
// ContextBudget is the agentkit Shaper token budget applied to every
// candidate session — the SAME budget for every model, so context handling is
// a controlled variable rather than each model's raw window size. 0 → 60000.
type LLMConfig struct {
	BaseURL       string `yaml:"baseURL"`
	APIKeyEnv     string `yaml:"apiKeyEnv"`
	ContextBudget int    `yaml:"contextBudget"`
}

// EffectiveContextBudget returns the shaper budget, defaulted.
func (l LLMConfig) EffectiveContextBudget() int {
	if l.ContextBudget > 0 {
		return l.ContextBudget
	}
	return 60000
}

// ServerSpec is one extra MCP server to spawn for a toolset. The literal
// "{{workspace}}" in any arg is replaced with the run's scratch workspace path.
type ServerSpec struct {
	Cmd  string   `yaml:"cmd"`
	Args []string `yaml:"args"`
}

// Toolset is a named list of extra MCP servers (baseline = empty list).
// CedeFileTools drops llm-bench-mcp's read_file/write_file for this toolset so
// a bundled server that has its OWN file/edit surface (poly-lsp's
// node_read/node_edit) owns editing without a competing convention. run and
// list_dir always stay — llm-bench-mcp is the only server that can execute.
type Toolset struct {
	Name          string
	Servers       []ServerSpec
	CedeFileTools bool
}

// toolsetBody is the object form of a toolset value: {servers, cedeFileTools}.
type toolsetBody struct {
	Servers       []ServerSpec `yaml:"servers"`
	CedeFileTools bool         `yaml:"cedeFileTools"`
}

// OrderedToolsets preserves the declaration order of the toolsets mapping so
// runs + reports are deterministic.
type OrderedToolsets []Toolset

// UnmarshalYAML decodes the toolsets mapping node in declaration order. A value
// may be a bare server list (`mcpshell: [{cmd: …}]`, cedeFileTools=false) or an
// object (`polylsp: {servers: [{cmd: …}], cedeFileTools: true}`).
func (o *OrderedToolsets) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("toolsets must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		val := n.Content[i+1]
		ts := Toolset{Name: name}
		switch val.Kind {
		case yaml.SequenceNode:
			if err := val.Decode(&ts.Servers); err != nil {
				return fmt.Errorf("toolset %q: %w", name, err)
			}
		case yaml.MappingNode:
			var body toolsetBody
			if err := val.Decode(&body); err != nil {
				return fmt.Errorf("toolset %q: %w", name, err)
			}
			ts.Servers, ts.CedeFileTools = body.Servers, body.CedeFileTools
		default:
			return fmt.Errorf("toolset %q: must be a server list or an object", name)
		}
		*o = append(*o, ts)
	}
	return nil
}

// Get returns the named toolset and whether it exists.
func (o OrderedToolsets) Get(name string) (Toolset, bool) {
	for _, t := range o {
		if t.Name == name {
			return t, true
		}
	}
	return Toolset{}, false
}

// LoadConfig reads and lightly validates llm-bench.yaml.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return loadConfigBytes(b)
}

// loadConfigBytes parses + validates config YAML (the LoadConfig core, sans I/O).
func loadConfigBytes(b []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if c.LLM.BaseURL == "" {
		return c, fmt.Errorf("config: llm.baseURL is required")
	}
	if len(c.Models) == 0 {
		return c, fmt.Errorf("config: at least one model is required")
	}
	if len(c.Toolsets) == 0 {
		return c, fmt.Errorf("config: at least one toolset is required")
	}
	return c, nil
}
