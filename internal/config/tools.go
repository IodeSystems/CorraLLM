package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/toolchain/recipes"
)

// Top-level `tools:` — the programs that RUN the models, tracked per host.
//
// corrallm knew what models it ran and nothing about what ran them: llama.cpp
// was a path inside a `cmd:` string and that was the whole of its awareness.
// The cost of that shows up three ways — a tool that ships several builds a day
// with no way to see which one a host has, a second engine (ninfer) that can
// only exist on some hosts, and the same binary spelled as two hand-maintained
// absolute paths because /home and /Users differ.
//
//	tools:
//	  llama.cpp:
//	    url: https://github.com/ggml-org/llama.cpp.git
//	    ref: master
//	    bin: llama-server
//	    hosts:
//	      box1: {}                       # managed: corrallm clones and builds
//	      carlsmacbookpro:
//	        installedAt: /Users/.../ml-kit/local/bin/llama.cpp   # adopted
//
// ADOPTED VS MANAGED is the distinction that makes this safe to land on a live
// box. An entry with `installedAt` is adopted: corrallm probes it, reports its
// version and its drift, and never writes there. Only a managed entry — one
// with no installedAt — gets a checkout and a build, under corrallm's own home.
// Day one is therefore all adoption: every existing ml-kit build becomes
// visible without corrallm cloning or compiling anything.
type Tool struct {
	// URL is the upstream git remote. Required: it is what `upstream` asks
	// about, and a tool nobody can check for drift is just a path.
	URL string `yaml:"url"`

	// Ref is what the tool TRACKS — a branch or a tag. Required, and
	// deliberately not defaulted to "main"/"master": the two projects tracked
	// here disagree (llama.cpp uses master, ninfer main), so a default would be
	// wrong half the time and silently.
	//
	// To hold a tool at one commit, set Pin rather than putting a sha here: Ref
	// is what the drift check follows, and a sha in it makes that check
	// meaningless (a commit never moves).
	Ref string `yaml:"ref"`

	// Pin HOLDS the tool at one commit, overriding Ref.
	//
	// Ref says what to track; Pin says "not past here". They are separate
	// fields rather than one because holding a tool back is temporary and
	// tracking is not: overwriting `ref: master` with a sha loses the branch
	// you meant to return to, and un-pinning then becomes an act of memory.
	// With both, a pin is one field set and one field cleared, and the drift
	// check can go on reporting how far the tracked branch has run ahead while
	// you stay put.
	//
	// A full 40-character sha, or empty. Abbreviated is refused: `git fetch`
	// cannot ask a remote for a short hash, and accepting one would produce a
	// pin that resolves on a host with a clone and fails on a host without.
	//
	// While pinned, "behind" means behind the PIN, not behind upstream — so a
	// scheduled rebuild converges on the pin and then stops, which is the whole
	// point of setting one.
	Pin string `yaml:"pin,omitempty"`

	// Recipe names the script that knows how to probe and build this tool.
	// Empty means "same as the tool's key", which is the case for both tools
	// shipped today.
	Recipe string `yaml:"recipe,omitempty"`

	// Bin is the executable a probe asks for a version, relative to the install
	// directory. Empty lets the recipe pick its own default.
	Bin string `yaml:"bin,omitempty"`

	// Hosts is where this tool is declared to exist, keyed by `servers:` name.
	//
	// A host ABSENT from this map is undeclared, which is not the same as
	// unavailable — "ninfer can never run here" and "nobody has said yet" are
	// different facts and only one of them is a bug. The registry reports them
	// differently and neither is inferred from the other.
	Hosts map[string]ToolHost `yaml:"hosts,omitempty"`

	// Check is how often to ask upstream whether the pin has moved. Empty means
	// DefaultCheckInterval; "off" (or "0") disables it for this tool.
	//
	// On by default because it is one `git ls-remote` round trip and the
	// alternative is a pin that silently rots. Building is the expensive,
	// disruptive half and is off by default; see Rebuild.
	Check string `yaml:"check,omitempty"`

	// Rebuild allows the scheduled check to go on and BUILD when it finds
	// drift. Off unless asked for: a CUDA build is ten to twenty minutes of
	// pegged GPU that can replace a binary running models depend on, and that
	// stays a decision rather than a timer.
	Rebuild bool `yaml:"rebuild,omitempty"`

	// Notes is free text kept about the tool, shown in the UI beside it.
	Notes string `yaml:"notes,omitempty"`
}

// ToolHost is one host's relationship to a tool.
type ToolHost struct {
	// InstalledAt ADOPTS an existing install: probe reads this directory and
	// nothing ever writes to it. Empty makes the entry managed, installing
	// under corrallm's own home instead.
	//
	// Pointing this at a checkout a human edits is the thing to avoid — a
	// managed build runs `git clean -xdf`, which is why managed trees live
	// where corrallm owns them and adopted ones are strictly read-only.
	InstalledAt string `yaml:"installedAt,omitempty"`

	// Prefix overrides where a MANAGED install goes. Empty is
	// <home>/tools/<tool>. Ignored when InstalledAt is set.
	Prefix string `yaml:"prefix,omitempty"`

	// Notes is free text about this host's copy.
	Notes string `yaml:"notes,omitempty"`
}

// DefaultCheckInterval is how often an unconfigured tool asks upstream whether
// its pin has moved.
const DefaultCheckInterval = 6 * time.Hour

// RecipeOf resolves the recipe name, applying the "same as the key" default.
func RecipeOf(name string, t Tool) string {
	if r := strings.TrimSpace(t.Recipe); r != "" {
		return r
	}
	return name
}

// Adopted reports whether this host's entry points at an install corrallm does
// not own. An adopted entry is never built and never written to.
func (h ToolHost) Adopted() bool { return strings.TrimSpace(h.InstalledAt) != "" }

// CheckIntervalOf resolves the drift-check cadence. ok=false means checking is
// switched off for this tool.
func CheckIntervalOf(t Tool) (d time.Duration, ok bool) {
	s := strings.TrimSpace(strings.ToLower(t.Check))
	switch s {
	case "":
		return DefaultCheckInterval, true
	case "off", "false", "no", "0", "none":
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		// Validate rejects this at load, so reaching here means a config that
		// was never validated. Fall back to the default rather than checking
		// in a tight loop.
		return DefaultCheckInterval, true
	}
	return d, true
}

// EffectiveRef is what a checkout should be aligned to: the pin when there is
// one, the tracked ref otherwise.
func EffectiveRef(t Tool) string {
	if p := strings.TrimSpace(t.Pin); p != "" {
		return p
	}
	return strings.TrimSpace(t.Ref)
}

// ValidatePin refuses everything a pin cannot be.
//
// Only a full sha, because that is the only form every path here can honour: a
// remote cannot be asked for an abbreviated hash, and a branch or tag belongs in
// `ref` where the drift check can follow it.
func ValidatePin(pin string) error {
	p := strings.TrimSpace(pin)
	if p == "" {
		return nil
	}
	if len(p) != 40 {
		return fmt.Errorf("pin %q is not a full commit sha (40 hex characters) — a remote cannot be asked for an abbreviated hash; to follow a branch or tag, set ref instead", p)
	}
	for _, r := range p {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return fmt.Errorf("pin %q is not a commit sha — %q is not a hex digit", p, string(r))
		}
	}
	return nil
}

// NormalizePin lowercases a pin so two spellings of the same commit compare
// equal. Git prints lowercase; a hash pasted from a UI may not.
func NormalizePin(pin string) string { return strings.ToLower(strings.TrimSpace(pin)) }

// validateTools checks the shape of `tools:` and that every host it names is a
// real server.
//
// A tool declared on a server that does not exist is the failure worth catching
// here: it would report as permanently unreachable, which looks exactly like a
// host that is down and sends someone to check the wrong machine.
func (c *Config) validateTools() error {
	for _, name := range sortedToolNames(c.Tools) {
		t := c.Tools[name]
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tools: a tool with an empty name")
		}
		if strings.TrimSpace(t.URL) == "" {
			return fmt.Errorf("tool %q: url is required — without it nothing can check the pin for drift", name)
		}
		if strings.TrimSpace(t.Ref) == "" {
			return fmt.Errorf("tool %q: ref is required — llama.cpp pins master and ninfer main, so there is no safe default", name)
		}
		if err := ValidatePin(t.Pin); err != nil {
			return fmt.Errorf("tool %q: %w", name, err)
		}
		recipe := RecipeOf(name, t)
		if !recipes.Has(recipe) {
			return fmt.Errorf("tool %q: no recipe %q (available: %s)",
				name, recipe, strings.Join(recipes.Names(), ", "))
		}
		if s := strings.TrimSpace(t.Check); s != "" {
			if _, ok := CheckIntervalOf(t); ok {
				if d, err := time.ParseDuration(s); err != nil || d <= 0 {
					return fmt.Errorf("tool %q: check %q is not a duration (e.g. 6h) or \"off\"", name, t.Check)
				}
			}
		}
		if t.Rebuild {
			if _, ok := CheckIntervalOf(t); !ok {
				return fmt.Errorf("tool %q: rebuild is on but check is off — a scheduled rebuild is driven by the check, so this would never fire", name)
			}
		}
		for _, host := range sortedHostNames(t.Hosts) {
			if _, ok := c.Servers[host]; !ok {
				return fmt.Errorf("tool %q: host %q is not a declared server %v", name, host, serverNames(c.Servers))
			}
			h := t.Hosts[host]
			if h.Adopted() && strings.TrimSpace(h.Prefix) != "" {
				return fmt.Errorf("tool %q host %q: installedAt and prefix are mutually exclusive — installedAt adopts an install corrallm never writes to, prefix says where corrallm should build one", name, host)
			}
		}
	}
	return nil
}

func sortedToolNames(m map[string]Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedHostNames(m map[string]ToolHost) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func serverNames(m map[string]Server) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
