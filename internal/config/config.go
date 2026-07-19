package config

import (
	"fmt"
	"os"
	"strings"

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

	// Convert is the GLOBAL default for attachment ingestion (PDFs in chat). A
	// model's own Convert block overrides it field-by-field. Empty = built-in
	// defaults (extract text).
	Convert ConvertConfig `yaml:"convert,omitempty"`

	// Models maps a served model name → exactly one serving path (a spawned cmd
	// or a proxy target) + residency policy. Fallback across models is a lane.
	Models map[string]Model `yaml:"models,omitempty"`

	// Lanes are named, ordered fallback lists over model names. Requesting a
	// lane name allows substitution across its members (walked best-quality
	// first, gated by the caller group's acceptDegrade/qualityFloor); requesting
	// a model name pins exactly that model.
	Lanes map[string]Lane `yaml:"lanes,omitempty"`

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

// Model is a served name with exactly ONE serving path: either a spawned local
// process (`cmd` + the port it binds in `proxy`) or a standalone proxy target
// (remote/paid endpoint, or another process's surface). The same weights served
// two ways = two models with distinct names; fallback across them is a lane.
type Model struct {
	Sticky     *Sticky `yaml:"sticky,omitempty"`
	Persistent bool    `yaml:"persistent,omitempty"`
	// Convert overrides the global attachment-ingestion config for this model
	// (e.g. a vision model rasterizes PDFs to images instead of extracting text).
	Convert *ConvertConfig `yaml:"convert,omitempty"`

	Cmd    string `yaml:"cmd,omitempty"`    // spawn it (local model); empty → pure proxy
	Server string `yaml:"server,omitempty"` // which server it draws capacity from (cmd only)
	// RAMUsage is an ADVISORY bootstrap hint, not a fact anything relies on.
	//
	// A measured profile supersedes it the moment one exists (see
	// proc.effectiveUsage), and a model with neither is treated as needing the
	// whole pool for one spawn so it can be measured alone. Declaring it only
	// saves that first heavy eviction.
	//
	// It is advisory because every hand-written size on this box was wrong: the
	// gpu0 pool understated the real card by 2 GB, ternary-bonsai-27b's ramUsage
	// by 7 GB, Qwen3-6-27B-MPT's by 1 GB, and nomic-embed-text's OVER-stated by
	// 262 MiB. They stayed invisible because the errors cancelled — until
	// measurement made one side honest and the arithmetic stopped working.
	RAMUsage map[string]string `yaml:"ramUsage,omitempty"` // per-pool footprint vector (cmd only)
	Swap     *Swap             `yaml:"swap,omitempty"`     // measured load cost (cmd only)
	Proxy    yaml.Node         `yaml:"proxy,omitempty"`    // forward target: number | "host:port" | {host,port,headers}
	Type     string            `yaml:"type,omitempty"`     // cost class: chat | embed | openrouter | …
	Quality  int               `yaml:"quality,omitempty"`
	// MaxConcurrent is the model's admission slots (the fairshare capacity
	// unit). For a local llama-server this mirrors --parallel. Default 1.
	MaxConcurrent int `yaml:"maxConcurrent,omitempty"`

	// ContextPerRequest is the context window each REQUEST must get, in tokens.
	//
	// llama.cpp's --ctx-size is a TOTAL divided across --parallel slots, so
	// raising concurrency silently shrinks the window every request sees. That
	// inverts how anyone actually reasons about it: the context length is a
	// requirement ("this model must serve 220k-token prompts") and concurrency
	// is the free variable you discover by tuning.
	//
	// When set, corrallm computes the spawned --ctx-size as
	// ContextPerRequest * slots, so the declared window is preserved by
	// construction and SLOTS become what gets reduced under VRAM pressure. If
	// not even one slot fits, that is reported loudly rather than served
	// quietly at a shorter window.
	//
	// Unset (0) keeps llama.cpp's native meaning: whatever --ctx-size the cmd
	// says is the total, divided by slots. Existing configs are unaffected
	// until they opt in.
	ContextPerRequest int `yaml:"contextPerRequest"`
	// MaxTokens caps the completion length this (often smaller/degraded) model
	// will be asked for: when a lane request degrades onto it, its max_tokens is
	// clamped to this value (P7). 0 = no cap.
	MaxTokens int `yaml:"maxTokens,omitempty"`
	// Modalities declares the input modalities this model accepts, each with
	// optional client-facing metadata (see ModalitySpec). Keys: text|image|audio.
	// Replaces the old coarse modality bucket. Unset → inferred: {audio} for audio
	// cost types, else {text}. Note: llama.cpp auto-loads the mmproj sibling from a
	// vision repo (no --mmproj flag), so `image` is declared here, not detected.
	Modalities map[string]ModalitySpec `yaml:"modalities,omitempty"`
}

// ModalitySpec is optional client-facing metadata for one accepted input
// modality. Only the fields relevant to that modality are set: image uses
// maxResolution + formats, audio uses formats, text may cap generation with
// maxTokens; the rest stay zero and are omitted from output.
type ModalitySpec struct {
	MaxResolution int      `yaml:"maxResolution,omitempty" json:"maxResolution,omitempty"` // image: longest-edge px cap
	Formats       []string `yaml:"formats,omitempty" json:"formats,omitempty"`             // image/audio: accepted encodings
	MaxTokens     int      `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`         // text: generation-length cap
}

// KnownModalities is the accepted set of modality keys (typo guard in Validate).
var KnownModalities = map[string]bool{"text": true, "image": true, "audio": true}

// EffectiveModalities returns the model's declared modalities, or a single
// inferred default when none are configured: "audio" when audioDefault (an audio
// cost type), else "text". Callers pass audioDefault because audio-ness lives in
// the cost model, not config.
func (m Model) EffectiveModalities(audioDefault bool) map[string]ModalitySpec {
	if len(m.Modalities) > 0 {
		return m.Modalities
	}
	d := "text"
	if audioDefault {
		d = "audio"
	}
	return map[string]ModalitySpec{d: {}}
}

// Slots returns the model's concurrency capacity, defaulting to 1.
func (m Model) Slots() int {
	if m.MaxConcurrent > 0 {
		return m.MaxConcurrent
	}
	return 1
}

// Lane is a named, ordered fallback list over model names.
type Lane struct {
	Members []LaneMember `yaml:"members,omitempty"`
}

// LaneMember references a model by name, optionally overriding its residency
// stickiness when it was loaded on this lane's behalf (e.g. a fallback member
// unloads sooner than when requested directly). YAML accepts a plain string
// (`- gemma-4-12b`) or an object (`- {model: gemma-4-12b, sticky: {ttl: 120s}}`).
type LaneMember struct {
	Model  string  `yaml:"model"`
	Sticky *Sticky `yaml:"sticky,omitempty"`
}

// UnmarshalYAML lets a member be a scalar model name or the object form.
func (lm *LaneMember) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		lm.Model = n.Value
		return nil
	}
	type raw LaneMember // avoid recursion
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*lm = LaneMember(r)
	return nil
}

// Candidate is one resolved serving option for a served name: the model, its
// name (process identity + audit key), and an optional lane-member sticky
// override applied when this candidate is loaded via the lane.
type Candidate struct {
	Name   string
	Model  Model
	Sticky *Sticky // nil → the model's own sticky applies
}

// ResolveServed maps a request's served name to its ordered candidates: a lane
// name yields its members (fallback allowed), a model name yields exactly that
// model (pinned). Unknown lane members are skipped (Validate rejects them at
// load; skipping keeps a hand-built Config safe).
func (c *Config) ResolveServed(served string) ([]Candidate, bool) {
	if lane, ok := c.Lanes[served]; ok {
		cands := make([]Candidate, 0, len(lane.Members))
		for _, mem := range lane.Members {
			m, ok := c.Models[mem.Model]
			if !ok {
				continue
			}
			cands = append(cands, Candidate{Name: mem.Model, Model: m, Sticky: mem.Sticky})
		}
		return cands, len(cands) > 0
	}
	if m, ok := c.Models[served]; ok {
		return []Candidate{{Name: served, Model: m}}, true
	}
	return nil, false
}

// ConvertConfig governs how attached files (currently PDFs) in a chat request are
// ingested before reaching the model. Resolved per request as built-in defaults →
// global `convert:` → the model's `convert:`, each field overriding the last.
// Zero/empty fields inherit; OCR is a pointer so a model can force it false.
type ConvertConfig struct {
	PDF      string `yaml:"pdf,omitempty"`      // text | vision | off
	DPI      int    `yaml:"dpi,omitempty"`      // rasterization DPI (vision/OCR)
	Quality  int    `yaml:"quality,omitempty"`  // JPEG quality 1–100 (vision)
	Format   string `yaml:"format,omitempty"`   // jpeg | png (vision page images)
	MaxPages int    `yaml:"maxPages,omitempty"` // page cap (vision rasterize / OCR)
	MaxChars int    `yaml:"maxChars,omitempty"` // injected-text cap (text)
	OCR      *bool  `yaml:"ocr,omitempty"`      // OCR fallback for scanned PDFs (text)
}

// DefaultConvert is the built-in base every resolution starts from.
func DefaultConvert() ConvertConfig {
	on := true
	return ConvertConfig{PDF: "text", DPI: 200, Quality: 85, Format: "jpeg", MaxPages: 20, MaxChars: 400000, OCR: &on}
}

// Merge returns c with the set (non-zero) fields of over applied on top.
func (c ConvertConfig) Merge(over ConvertConfig) ConvertConfig {
	if over.PDF != "" {
		c.PDF = over.PDF
	}
	if over.DPI != 0 {
		c.DPI = over.DPI
	}
	if over.Quality != 0 {
		c.Quality = over.Quality
	}
	if over.Format != "" {
		c.Format = over.Format
	}
	if over.MaxPages != 0 {
		c.MaxPages = over.MaxPages
	}
	if over.MaxChars != 0 {
		c.MaxChars = over.MaxChars
	}
	if over.OCR != nil {
		c.OCR = over.OCR
	}
	return c
}

// OCREnabled reports whether the OCR fallback is on (defaults to true if unset).
func (c ConvertConfig) OCREnabled() bool { return c.OCR == nil || *c.OCR }

// ConvertFor resolves the effective ingestion config for a served name: the
// global default overridden by the model's own block. A lane inherits its
// first (top-preference) member's block — conversion happens once, before the
// member walk, so the primary member's needs win.
func (c *Config) ConvertFor(global ConvertConfig, served string) ConvertConfig {
	eff := global
	name := served
	if lane, ok := c.Lanes[served]; ok && len(lane.Members) > 0 {
		name = lane.Members[0].Model
	}
	if m, ok := c.Models[name]; ok && m.Convert != nil {
		eff = eff.Merge(*m.Convert)
	}
	return eff
}

// Sticky keeps a model warm after last use and resists eviction (residency, P4).
type Sticky struct {
	TTL       string `yaml:"ttl,omitempty"`
	EvictCost string `yaml:"evictCost,omitempty"` // low | medium | high
}

// MaxQuality returns the highest Quality among the candidates (0 if none/unset)
// — the top of a served name's quality ladder (P7).
func MaxQuality(cands []Candidate) int {
	top := 0
	for _, c := range cands {
		if c.Model.Quality > top {
			top = c.Model.Quality
		}
	}
	return top
}

// Capability classifies a backend cost-class `type` into the operation it serves,
// the same convention modality is inferred from. STT and TTS are kept DISTINCT
// (speech-in vs speech-out) — never lumped as "audio". Drives /v1/models,
// /v1/capabilities, and the dashboard so clients/LLMs pick the right model.
func Capability(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case strings.Contains(t, "realtime"):
		// Live ws transcription (/v1/realtime) — a distinct delivery from batch
		// STT, so the catalog/console can route to the right surface without a
		// separate "modes" field.
		return "audio.realtime"
	case strings.Contains(t, "tts") || strings.Contains(t, "speech"):
		return "audio.tts"
	case strings.Contains(t, "stt") || strings.Contains(t, "asr") ||
		strings.Contains(t, "whisper") || strings.Contains(t, "transcri") || strings.Contains(t, "parakeet"):
		return "audio.stt"
	case strings.Contains(t, "embed"):
		return "embeddings"
	case strings.Contains(t, "rerank"):
		return "rerank"
	default:
		return "chat"
	}
}

// ModelCapability is a served model's capability, inferred from its cost type.
func ModelCapability(m Model) string {
	return Capability(m.Type)
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
	// PreferResident makes the group best-effort against what is already loaded:
	// among the backends it accepts (quality-wise), any that are currently
	// resident (a warm process) are tried first, in quality order, before any
	// cold-load candidate. Only when no accepted backend is resident does it fall
	// to the normal quality-first cold-load ladder. Keeps a latency-sensitive lane
	// (a concierge) on whatever chat model is hot instead of cold-loading a bigger
	// one and re-hogging the box. Independent of AcceptDegrade (though pairing them
	// is what lets the lane ride a degraded-but-resident tier).
	PreferResident bool `yaml:"preferResident,omitempty"`
}

// AcceptsQuality reports whether the group may be served by a backend of quality
// q, given the model's top-tier quality. A non-degrading group accepts only the
// top tier; a degrading group accepts down to its QualityFloor (P7).
func (g PriorityGroup) AcceptsQuality(q, topQuality int) bool {
	// A model's own top tier is always acceptable — the floor only gates
	// degrading BELOW the best when a better tier exists. Without this, a group
	// with QualityFloor>0 rejects any model whose whole ladder sits under the
	// floor (e.g. audio backends default to quality 0), emptying the walk →
	// "no backend available".
	if q >= topQuality {
		return true
	}
	if !g.AcceptDegrade {
		return false // no degrade → only the top tier
	}
	return q >= g.QualityFloor // degrade down to the floor
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
		if m.Cmd == "" && m.Proxy.IsZero() {
			return fmt.Errorf("model %q: needs cmd (spawned) or proxy (standalone proxy model); legacy backends: lists are no longer supported — flatten to one path per model and compose fallbacks as a lane", name)
		}
		if m.Cmd != "" && m.Server == "" {
			return fmt.Errorf("model %q: cmd set but no server", name)
		}
		if m.Cmd == "" {
			// A pure proxy model has no local lifecycle: residency knobs are
			// meaningless on it and almost certainly a config mistake.
			if m.Sticky != nil || m.Persistent || len(m.RAMUsage) > 0 || m.Swap != nil || m.Server != "" {
				return fmt.Errorf("model %q: sticky/persistent/ramUsage/swap/server only apply to cmd models", name)
			}
		}
		for k := range m.Modalities {
			if !KnownModalities[k] {
				return fmt.Errorf("model %q: unknown modality %q (want text|image|audio)", name, k)
			}
		}
		if m.Server != "" {
			srv, ok := c.Servers[m.Server]
			if !ok {
				return fmt.Errorf("model %q: unknown server %q", name, m.Server)
			}
			if _, err := ParseSizes(m.RAMUsage); err != nil {
				return fmt.Errorf("model %q ramUsage: %w", name, err)
			}
			for pool := range m.RAMUsage {
				if _, ok := srv.Pools[pool]; !ok {
					return fmt.Errorf("model %q: ramUsage pool %q not declared on server %q",
						name, pool, m.Server)
				}
			}
		}
	}
	for name, lane := range c.Lanes {
		if _, clash := c.Models[name]; clash {
			return fmt.Errorf("lane %q: name collides with a model", name)
		}
		if len(lane.Members) == 0 {
			return fmt.Errorf("lane %q: no members", name)
		}
		for i, mem := range lane.Members {
			if mem.Model == "" {
				return fmt.Errorf("lane %q member %d: empty model name", name, i)
			}
			if _, ok := c.Models[mem.Model]; !ok {
				return fmt.Errorf("lane %q member %d: unknown model %q", name, i, mem.Model)
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
