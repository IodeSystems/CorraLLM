package agent

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileConfig is the agent's own configuration, as installed at
// ./corrallm-agent/agent.yml.
//
// A file rather than environment variables, because the environment has to be
// arranged by whatever starts the agent — and that means a shell-specific
// incantation (`set -a; . agent.env; set +a`) which is bash syntax and simply
// does not work in fish. A config file the binary reads itself is shell-neutral
// by construction.
type FileConfig struct {
	// Primary is the corrallm this agent reports to.
	Primary string `yaml:"primary"`
	// Server is the `servers:` key this machine backs. On first enrollment the
	// primary may assign it, in which case this is rewritten.
	Server string `yaml:"server"`
	// Token is the long-lived credential. Written here after enrollment.
	Token string `yaml:"token,omitempty"`
	// EnrollToken is the one-time token, consumed on first start and then
	// removed — leaving it would produce a confusing "already used" failure on
	// the next restart.
	EnrollToken string `yaml:"enrollToken,omitempty"`
	// Addr is the agent's own listen address.
	Addr string `yaml:"addr,omitempty"`
	// SelfUpdate lets the agent replace its binary with the primary's build
	// when versions differ AND nothing is running here.
	SelfUpdate *bool `yaml:"selfUpdate,omitempty"`
}

// LoadFileConfig reads an agent config. A missing file is not an error: the
// agent is equally configurable by flags and environment.
func LoadFileConfig(path string) (*FileConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &FileConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent config %s: %w", path, err)
	}
	var c FileConfig
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a typo'd key must fail rather than be ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse agent config %s: %w", path, err)
	}
	return &c, nil
}

// SaveFileConfig writes the agent config atomically at 0600.
//
// 0600 because it holds a credential, and atomically because this file is how
// the agent finds its primary — a truncated write leaves a machine that cannot
// rejoin without someone going to it, which is the whole thing the install flow
// exists to avoid.
func SaveFileConfig(path string, c *FileConfig) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
