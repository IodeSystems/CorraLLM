package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the corrallm domain config (the YAML file). It is the scheduler's
// declarative source of truth: served models → ordered backends, server
// capacity, priority groups, and the cost model. P0 parses and validates the
// shape; later phases give each section behavior.
//
// The schema mirrors ~/doc/plan/corrallm.md §3. Sections not yet consumed are
// still parsed so configs are forward-compatible and round-trippable.
type Config struct {
	// CostPerKwh converts local energy → $ (cost model, P6).
	CostPerKwh float64 `yaml:"costPerKwh,omitempty"`

	// CommandCosts maps a backend `type` to its cost parameters.
	CommandCosts map[string]map[string]any `yaml:"commandCosts,omitempty"`

	// Servers declares host capacity as a vector over named memory pools.
	Servers map[string]Server `yaml:"servers,omitempty"`

	// Models maps a served model name → its ordered backend list + residency.
	Models map[string]Model `yaml:"models,omitempty"`

	// PriorityGroups bundle scheduling policy; a key maps to exactly one group.
	PriorityGroups map[string]PriorityGroup `yaml:"priorityGroups,omitempty"`

	// Keys maps a caller identity → priorityGroup name.
	Keys map[string]string `yaml:"keys,omitempty"`
}

// Server declares a host's capacity as a vector over named memory pools.
type Server struct {
	Pools         map[string]string `yaml:"pools,omitempty"`   // pool → size (e.g. "24GB")
	Reserve       map[string]string `yaml:"reserve,omitempty"` // headroom kept free per pool
	MaxConcurrent int               `yaml:"maxConcurrent,omitempty"`
}

// Model is a served name: residency policy + an ordered list of backends.
type Model struct {
	Sticky     *Sticky   `yaml:"sticky,omitempty"`
	Persistent bool      `yaml:"persistent,omitempty"`
	Backends   []Backend `yaml:"backends,omitempty"`
}

// Sticky keeps a model warm after last use and resists eviction (residency, P4).
type Sticky struct {
	TTL       string `yaml:"ttl,omitempty"`
	EvictCost string `yaml:"evictCost,omitempty"` // low | medium | high
}

// Backend is one route for a served model: optionally spawn a command, always
// proxy to a target. Round-robin within a `type`, ordered across types.
type Backend struct {
	Cmd      string            `yaml:"cmd,omitempty"`      // optional: spawn it
	Server   string            `yaml:"server,omitempty"`   // which server it draws capacity from
	RAMUsage map[string]string `yaml:"ramUsage,omitempty"` // per-pool footprint vector
	Swap     *Swap             `yaml:"swap,omitempty"`
	Proxy    yaml.Node         `yaml:"proxy,omitempty"` // number | "host:port" | {host,port,headers}
	Type     string            `yaml:"type,omitempty"`  // cost class: local | claude | …
	Quality  int               `yaml:"quality,omitempty"`
}

// Swap is the measured cost of loading a backend (residency input, P4).
type Swap struct {
	LoadSeconds float64 `yaml:"loadSeconds,omitempty"`
}

// PriorityGroup is the single policy unit (fairshare + saturation behavior).
type PriorityGroup struct {
	Weight        int                   `yaml:"weight,omitempty"`
	ShareCurrency string                `yaml:"shareCurrency,omitempty"` // requests | dwell | cost
	Interruptible bool                  `yaml:"interruptible,omitempty"`
	OnSaturated   map[string]yaml.Node  `yaml:"onSaturated,omitempty"` // backend type → stage policy
	Limits        map[string]string     `yaml:"limits,omitempty"`      // TCO caps (e.g. dwell: "600s/min")
}

// Load reads and parses the corrallm YAML config at path. A missing file yields
// an empty (valid) config so the proxy can boot with env-only configuration.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// Validate checks structural invariants that must hold before scheduling can
// run. P0 enforces only what's cheap and unambiguous; richer checks land with
// the phases that consume each section.
func (c *Config) Validate() error {
	for name, m := range c.Models {
		if len(m.Backends) == 0 {
			return fmt.Errorf("model %q: no backends", name)
		}
		for i, b := range m.Backends {
			if b.Cmd != "" && b.Server == "" {
				return fmt.Errorf("model %q backend %d: cmd set but no server", name, i)
			}
			if b.Server != "" {
				if _, ok := c.Servers[b.Server]; !ok {
					return fmt.Errorf("model %q backend %d: unknown server %q", name, i, b.Server)
				}
			}
		}
	}
	for key, grp := range c.Keys {
		if _, ok := c.PriorityGroups[grp]; !ok {
			return fmt.Errorf("key %q: unknown priorityGroup %q", key, grp)
		}
	}
	return nil
}
