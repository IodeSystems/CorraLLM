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

	// Scheduler holds global admission knobs (queue bounds).
	Scheduler SchedulerConfig `yaml:"scheduler,omitempty"`
}

// SchedulerConfig bounds queueing so saturated callers get a fast, informative
// 429 to shape against instead of a long blocking wait (the llama-swap fork's
// maxWait / maxQueueDepth contract). Zero values keep the prior behavior:
// MaxWait 0 → bounded only by the request context; MaxQueueDepth 0 → unbounded.
type SchedulerConfig struct {
	MaxWait       string `yaml:"maxWait,omitempty"`       // e.g. "60s": queue wait before a 429
	MaxQueueDepth int    `yaml:"maxQueueDepth,omitempty"` // reject once this many already wait on a backend
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
	// MaxConcurrent is the backend's admission slots (the fairshare capacity
	// unit). For a local llama-server this mirrors --parallel. Default 1.
	MaxConcurrent int `yaml:"maxConcurrent,omitempty"`
	// MaxTokens caps the completion length this (often smaller/degraded) backend
	// will be asked for: when a request degrades onto it, its max_tokens is
	// clamped to this value (P7). 0 = no cap.
	MaxTokens int `yaml:"maxTokens,omitempty"`
}

// Slots returns the backend's concurrency capacity, defaulting to 1.
func (b Backend) Slots() int {
	if b.MaxConcurrent > 0 {
		return b.MaxConcurrent
	}
	return 1
}

// MaxQuality returns the highest Quality among the backends (0 if none/unset) —
// the top of a served model's quality ladder (P7).
func MaxQuality(bs []Backend) int {
	top := 0
	for _, b := range bs {
		if b.Quality > top {
			top = b.Quality
		}
	}
	return top
}

// Swap is the measured cost of loading a backend (residency input, P4). P6 adds
// LoadWatts so the load's energy can be priced (loadSeconds × loadWatts → $);
// absent, only its latency feeds scheduling.
type Swap struct {
	LoadSeconds float64 `yaml:"loadSeconds,omitempty"`
	LoadWatts   float64 `yaml:"loadWatts,omitempty"`
}

// PriorityGroup is the single policy unit (fairshare + saturation behavior).
type PriorityGroup struct {
	Weight        int               `yaml:"weight,omitempty"`
	ShareCurrency string            `yaml:"shareCurrency,omitempty"` // requests | dwell | cost
	Interruptible bool              `yaml:"interruptible,omitempty"`
	OnSaturated   map[string]Stage  `yaml:"onSaturated,omitempty"` // backend type → stage policy
	Limits        map[string]string `yaml:"limits,omitempty"`      // group-wide TCO caps
	// AcceptDegrade opts the group into quality-degrade fall-through (P7): when
	// set, the group may be served by lower-quality backends in the model's list
	// (down to QualityFloor). Default false → the group is served only by the
	// model's highest-quality tier; below that it backs off (reject/queue) rather
	// than be served a worse model.
	AcceptDegrade bool `yaml:"acceptDegrade,omitempty"`
	// QualityFloor is the lowest backend quality the group will accept when it
	// does degrade (0 = no floor). Ignored unless AcceptDegrade is set.
	QualityFloor int `yaml:"qualityFloor,omitempty"`
}

// AcceptsQuality reports whether the group may be served by a backend of quality
// q, given the model's top-tier quality. A non-degrading group accepts only the
// top tier; a degrading group accepts down to its QualityFloor (P7).
func (g PriorityGroup) AcceptsQuality(q, topQuality int) bool {
	floor := topQuality // no degrade → only the highest-quality backends
	if g.AcceptDegrade {
		floor = g.QualityFloor
	}
	return q >= floor
}

// EffectiveWeight returns the group's fairshare weight, defaulting to 1.
func (g PriorityGroup) EffectiveWeight() int {
	if g.Weight > 0 {
		return g.Weight
	}
	return 1
}

// StageFor returns the saturation stage for a backend type, falling back to the
// group's "default" stage, then to a reject stage if neither is declared.
func (g PriorityGroup) StageFor(backendType string) Stage {
	if s, ok := g.OnSaturated[backendType]; ok && !s.IsZero() {
		return s
	}
	if s, ok := g.OnSaturated["default"]; ok && !s.IsZero() {
		return s
	}
	return Stage{Reject: true}
}

// ResolveGroup maps a caller key to its priority group. An empty/unknown key, or
// a key whose group is absent, resolves to the "default" group (synthesized as
// weight-1 reject-on-saturation if the config omits it).
func (c *Config) ResolveGroup(key string) (name string, g PriorityGroup) {
	name = c.Keys[key]
	if name == "" {
		name = "default"
	}
	if grp, ok := c.PriorityGroups[name]; ok {
		return name, grp
	}
	return "default", PriorityGroup{Weight: 1}
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
	for srvName, srv := range c.Servers {
		if _, err := ParseSizes(srv.Pools); err != nil {
			return fmt.Errorf("server %q pools: %w", srvName, err)
		}
		if _, err := ParseSizes(srv.Reserve); err != nil {
			return fmt.Errorf("server %q reserve: %w", srvName, err)
		}
	}
	for name, m := range c.Models {
		if len(m.Backends) == 0 {
			return fmt.Errorf("model %q: no backends", name)
		}
		for i, b := range m.Backends {
			if b.Cmd != "" && b.Server == "" {
				return fmt.Errorf("model %q backend %d: cmd set but no server", name, i)
			}
			if b.Server != "" {
				srv, ok := c.Servers[b.Server]
				if !ok {
					return fmt.Errorf("model %q backend %d: unknown server %q", name, i, b.Server)
				}
				if _, err := ParseSizes(b.RAMUsage); err != nil {
					return fmt.Errorf("model %q backend %d ramUsage: %w", name, i, err)
				}
				for pool := range b.RAMUsage {
					if _, ok := srv.Pools[pool]; !ok {
						return fmt.Errorf("model %q backend %d: ramUsage pool %q not declared on server %q",
							name, i, pool, b.Server)
					}
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
