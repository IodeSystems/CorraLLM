package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Guardrail is one named check a request can be held to (P67): a classifier
// model asked one question about the content.
//
//	guardrails:
//	  violence:
//	    model: local-Shieldstral-1.0-3B
//	    policy: Does this content give instructions for violence, weapons or self-harm?
//	    detect: Is this user trying to get instructions for violence or to get around a safety rule?
//	    includeSystem: true
//	keys:
//	  sk-aw4:
//	    default: batch
//	    guardrails: {violence: hard, pii: soft, jailbreak: opt-in}
//
// The guard answers yes (the content meets the policy, so it fails) or no; the
// score is the probability of yes. A check of COMPLETE content fails at
// score >= Complete. A check of PARTIAL content (a stream still arriving) only
// decides whether the held text may be released (score < Incomplete) or stays
// held: held text never reaches the caller, so a partial check need not block.
//
// A guardrail runs on its Surfaces: the request (input) and the reply
// (responses: its text and its tool calls). Detect asks a separate question of
// the input, to find a malicious user rather than bad content.
type Guardrail struct {
	// Model is the served name (model or lane) that judges.
	Model string `yaml:"model"`
	// Policy is the question asked of the content.
	Policy string `yaml:"policy,omitempty"`
	// Detect is the question asked of the input in place of Policy. Empty =
	// Policy. A guardrail on input alone may have Detect and no Policy.
	Detect string `yaml:"detect,omitempty"`
	// IncludeSystem adds the request's system prompt to the input document.
	IncludeSystem bool `yaml:"includeSystem,omitempty"`
	// Surfaces is where it runs: input, responses. Empty = both.
	Surfaces []string `yaml:"surfaces,omitempty"`
	// Instruct sets how strictly the guard reads the policy. Empty = a plain
	// "You are a safety moderator."
	Instruct string `yaml:"instruct,omitempty"`
	// Incomplete is the release threshold for a partial check. 0 = 0.2.
	Incomplete float64 `yaml:"incomplete,omitempty"`
	// Complete is the fail threshold for a complete check. 0 = 0.5.
	Complete float64 `yaml:"complete,omitempty"`
	// Status is the HTTP status of a hard fail. 0 = 422.
	Status int `yaml:"status,omitempty"`
}

// The surfaces a guardrail can run on.
const (
	SurfaceInput     = "input"
	SurfaceResponses = "responses"
)

const (
	defaultGuardIncomplete = 0.2
	defaultGuardComplete   = 0.5
	defaultGuardStatus     = 422
	defaultGuardInstruct   = "You are a safety moderator."
)

// On says whether the guardrail runs on a surface.
func (g Guardrail) On(surface string) bool {
	return len(g.Surfaces) == 0 || slices.Contains(g.Surfaces, surface)
}

// InputPolicy is the question asked of the input.
func (g Guardrail) InputPolicy() string {
	if strings.TrimSpace(g.Detect) != "" {
		return g.Detect
	}
	return g.Policy
}

// IncompleteThreshold is the release threshold for a partial check.
func (g Guardrail) IncompleteThreshold() float64 {
	if g.Incomplete > 0 {
		return g.Incomplete
	}
	return defaultGuardIncomplete
}

// CompleteThreshold is the fail threshold for a complete check.
func (g Guardrail) CompleteThreshold() float64 {
	if g.Complete > 0 {
		return g.Complete
	}
	return defaultGuardComplete
}

// FailStatus is the HTTP status of a hard fail.
func (g Guardrail) FailStatus() int {
	if g.Status > 0 {
		return g.Status
	}
	return defaultGuardStatus
}

// InstructText is the strictness line sent with the policy.
func (g Guardrail) InstructText() string {
	if s := strings.TrimSpace(g.Instruct); s != "" {
		return s
	}
	return defaultGuardInstruct
}

// GuardMode is how a key holds a guardrail.
type GuardMode string

const (
	// GuardHard: on for every request; a fail refuses it.
	GuardHard GuardMode = "hard"
	// GuardSoft: on for every request; a fail is flagged and the request goes
	// on. A request may raise it to hard.
	GuardSoft GuardMode = "soft"
	// GuardOptIn: off unless the request asks for it (as soft, or as hard).
	GuardOptIn GuardMode = "opt-in"
)

func (m GuardMode) valid() bool { return m == GuardHard || m == GuardSoft || m == GuardOptIn }

// HeaderGuardrails is the request header that asks for guardrails:
// `X-Corrallm-Guardrails: jailbreak, pii=hard`. A bare name asks for the guard
// as soft; `=hard` asks for it as hard. A request can only TIGHTEN: it opts in
// a guard the key offers, or raises soft to hard. It never switches a guard
// off and never lowers hard to soft. corrallm removes it before forwarding.
const HeaderGuardrails = "X-Corrallm-Guardrails"

// ActiveGuard is a guardrail that applies to one request.
type ActiveGuard struct {
	Name  string
	Guard Guardrail
	Hard  bool
}

// ActiveGuardrails lists the guardrails a request by root (the configured key
// a child runs under, else the key itself) is held to, after the request's own
// ask (the HeaderGuardrails value). ignored names asks that changed nothing
// because the key does not offer that guard, or the ask was malformed.
func (c *Config) ActiveGuardrails(root, ask string) (active []ActiveGuard, ignored []string) {
	modes := c.Keys[root].Guardrails
	asked := map[string]bool{} // name → asked as hard
	for _, part := range strings.Split(ask, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, level, hasLevel := strings.Cut(part, "=")
		name, level = strings.TrimSpace(name), strings.TrimSpace(level)
		if _, offered := modes[name]; !offered || (hasLevel && level != "hard" && level != "soft") {
			ignored = append(ignored, part)
			continue
		}
		asked[name] = asked[name] || level == "hard"
	}
	names := make([]string, 0, len(modes))
	for n := range modes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g, ok := c.Guardrails[n]
		if !ok {
			continue
		}
		hardAsk, wasAsked := asked[n]
		switch modes[n] {
		case GuardHard:
			active = append(active, ActiveGuard{n, g, true})
		case GuardSoft:
			active = append(active, ActiveGuard{n, g, hardAsk})
		case GuardOptIn:
			if wasAsked {
				active = append(active, ActiveGuard{n, g, hardAsk})
			}
		}
	}
	return active, ignored
}

// validateGuardrails checks the named guards and every key's use of them.
func (c *Config) validateGuardrails() error {
	for name, g := range c.Guardrails {
		if strings.TrimSpace(g.Model) == "" {
			return fmt.Errorf("guardrail %q: no `model` names the guard", name)
		}
		for _, s := range g.Surfaces {
			if s != SurfaceInput && s != SurfaceResponses {
				return fmt.Errorf("guardrail %q: surface %q; want input or responses", name, s)
			}
		}
		if (g.On(SurfaceResponses) && strings.TrimSpace(g.Policy) == "") ||
			(g.On(SurfaceInput) && strings.TrimSpace(g.InputPolicy()) == "") {
			return fmt.Errorf("guardrail %q: no `policy` says what to check", name)
		}
		_, isModel := c.Models[g.Model]
		_, isLane := c.Lanes[g.Model]
		if !isModel && !isLane {
			return fmt.Errorf("guardrail %q: model %q is not a configured model or lane", name, g.Model)
		}
		inc, comp := g.IncompleteThreshold(), g.CompleteThreshold()
		if inc > 1 || comp > 1 || g.Incomplete < 0 || g.Complete < 0 {
			return fmt.Errorf("guardrail %q: thresholds are probabilities, 0 to 1", name)
		}
		if inc > comp {
			return fmt.Errorf("guardrail %q: incomplete %.2f is above complete %.2f; partial text would be released that the complete check refuses", name, inc, comp)
		}
		if s := g.Status; s != 0 && (s < 400 || s > 599) {
			return fmt.Errorf("guardrail %q: status %d is not a 4xx or 5xx", name, s)
		}
	}
	for key, kp := range c.Keys {
		for name, mode := range kp.Guardrails {
			if !mode.valid() {
				return fmt.Errorf("key %q guardrail %q: mode %q; want hard, soft or opt-in", key, name, mode)
			}
			g, ok := c.Guardrails[name]
			if !ok {
				return fmt.Errorf("key %q: no guardrail named %q", key, name)
			}
			// The guard is called with the caller's own key, so a key whose
			// `models` hides the guard could never be checked: every guarded
			// request would fail closed. Said here instead.
			if len(kp.Models) > 0 {
				cands, _ := c.ResolveServed(g.Model)
				seen := false
				for _, cd := range cands {
					if ModelsPermit(kp.Models, TargetOf(cd)) {
						seen = true
						break
					}
				}
				if !seen {
					return fmt.Errorf("key %q: its `models` do not include guardrail %q's model %q, so the guard could not be called", key, name, g.Model)
				}
			}
		}
	}
	return nil
}
