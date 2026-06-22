package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Stage is a priority group's saturation policy for one backend type — the exit
// taken when a backend can't admit a request right now. The exits compose; P2
// acts on Queue and Reject, P3 adds Spill/FallThrough, P5 adds Preempt. Over a
// `limits` budget feeds the same sequence (just another reason a stage fails).
//
// In YAML a stage is either a bare verb (`reject`, `queue`, `fallThrough`) or an
// object (`{ preempt: true, then: fallThrough }`, `{ spill: true, limits: {…} }`).
type Stage struct {
	Queue       bool              `yaml:"queue,omitempty"`
	Reject      bool              `yaml:"reject,omitempty"`
	Preempt     bool              `yaml:"preempt,omitempty"`
	Spill       bool              `yaml:"spill,omitempty"`
	FallThrough bool              `yaml:"fallThrough,omitempty"`
	Then        string            `yaml:"then,omitempty"`   // follow-up verb, e.g. fallThrough
	Limits      map[string]string `yaml:"limits,omitempty"` // per-(group×type) TCO caps
}

// UnmarshalYAML accepts both the scalar-verb and object forms.
func (s *Stage) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.Value {
		case "reject":
			s.Reject = true
		case "queue":
			s.Queue = true
		case "fallThrough":
			s.FallThrough = true
		case "spill":
			s.Spill = true
		case "preempt":
			s.Preempt = true
		default:
			return fmt.Errorf("unknown saturation stage %q", n.Value)
		}
		return nil
	}
	// Object form. Alias avoids recursion into this UnmarshalYAML.
	type stageAlias Stage
	var a stageAlias
	if err := n.Decode(&a); err != nil {
		return err
	}
	*s = Stage(a)
	return nil
}

// IsZero reports whether the stage declares no action (treated as reject).
func (s Stage) IsZero() bool {
	return !s.Queue && !s.Reject && !s.Preempt && !s.Spill && !s.FallThrough && s.Then == "" && len(s.Limits) == 0
}
