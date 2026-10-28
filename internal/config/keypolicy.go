package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// GroupSeparator divides a credential from the priority group it is asking for:
// `sk-aw4:interactive`. One credential, several weightings, chosen per request.
//
// It rides in the key rather than a header on purpose. A caller sets an API key
// through every OpenAI-compatible client there is; a custom header needs
// plumbing that most of them do not offer, and a weighting the caller cannot
// express is one it will not use.
//
// "Group" and not "lane": a Lane in this config is an ordered fallback list over
// MODEL names, which is a routing concept and a different axis entirely.
const GroupSeparator = ":"

// KeyPolicy is what one caller identity may do: the priority group it lands in
// by default, and the groups it is PERMITTED to ask for.
//
// The permission is the point. Escalating to `interactive` is a claim that a
// person is blocked on this request, and that claim is worth more slots and the
// right to preempt. A key that may not make it cannot make it — a batch-only
// caller asking for interactive is answered in batch, not obeyed.
//
// What this does NOT do is bound how MUCH a permitted key escalates. "A person
// is waiting" is a judgement the caller makes about itself, and the likely
// failure is not abuse but a bug: a default left interactive, or a rule that
// drifts until everything qualifies. Nothing here can detect that. The guard is
// a limit on the escalated group (PriorityGroup.Limits), which bites on
// consumption; this only decides who is allowed to try.
type KeyPolicy struct {
	// Group is the priority group used when the credential names none.
	Group string
	// Allow is the set of groups this key may escalate into. Group is always
	// permitted — asking by name for the weighting you already have is not an
	// escalation.
	Allow map[string]bool

	// Thinking overrides the MODEL's reasoning default for this caller. nil
	// (the default) follows the model.
	//
	// It exists because the two callers on this box want opposite things from
	// the same model. An agentic client — every request a tool call — is
	// throttled by reasoning it did not ask for: turning thinking on took
	// sk-aw4 from 9.89 requests a minute to 0.85, with a third of its requests
	// exhausting an 8k reasoning budget in full, because a tool call is a
	// decision the model can usually make without deliberating about it. A
	// person at a chat window wants the opposite. The mode was a property of
	// the model, so they could not differ.
	//
	// A REQUEST still outranks this. The caller knows which of its own calls is
	// the hard one, and that judgement is better than any default here.
	Thinking *bool

	// Models are the selectors (keyaccess.go) naming what this key may see and
	// use. Empty = every model.
	Models []string

	// Budgets are what this key may spend, by selector (keyaccess.go). Every
	// matching budget applies. Only a hard one refuses.
	Budgets KeyBudgets

	// Limits is the pre-P65 allocation, read from old configs only:
	// MigrateKeyAccess moves it into Budgets at load. What follows is its
	// original meaning.
	//
	// Limits are this caller's ALLOCATION (P56): what it may spend across
	// every model, refilled each period. Empty is unlimited. Same shape and
	// hard/soft meaning as a model's or provider's budgets; a hard one refuses
	// with 429 once full, a soft one is reported and never refuses — every
	// route the caller has shares the one allocation, so there is no
	// alternative for a soft limit to steer to. Local traffic charges its
	// electricity, a paid route its bill: the metered cost of the request.
	Limits LimitSet

	// Mint lets this key create child keys (POST /v1/keys) and manage their
	// budgets and expiry. A permission the owner grants, not a default: a key
	// that can mint can hand its allocation to callers nobody approved.
	Mint bool

	// Credentials is the pre-P65 allow-pattern list, read from old configs
	// only: MigrateKeyAccess moves it into Models and Budgets at load.
	//
	// Credentials are this key's ALLOW PATTERNS (keycred.go): which provider
	// credentials it may forward with, each with an optional budget of its own.
	// Empty means every credential, which is what every key had before this.
	//
	// It lives here rather than on a credential's `allow` list because the
	// question it answers is about the CALLER: "what may this key spend".
	Credentials []KeyCredential

	// Disabled refuses this key outright, keeping its entry (and its budgets)
	// for later — the same statement a model's `disabled` makes. Keys minted at
	// runtime are created disabled by default, so they must be turned on before
	// they can spend anything.
	Disabled bool
}

// KeyScopeKey is a caller key's allocation identity in the quota ledger.
func KeyScopeKey(key string) string { return "key:" + key }

// KeyModelScopeKey is a caller key's share of one model in the quota ledger:
// the limits that name that model (Limit.Model).
func KeyModelScopeKey(key, model string) string { return "key:" + key + "@" + model }

// ThinkingPreference reports this key's reasoning override and whether it has one.
func (k KeyPolicy) ThinkingPreference() (think bool, set bool) {
	if k.Thinking == nil {
		return false, false
	}
	return *k.Thinking, true
}

// Permits reports whether this key may run in group g.
func (k KeyPolicy) Permits(g string) bool {
	if g == "" || g == k.Group {
		return true
	}
	return k.Allow[g]
}

// UnmarshalYAML accepts both shapes, because the string form is what every
// existing config is written in and this feature must not rewrite them:
//
//	keys:
//	  yscr: default                 # unchanged: identity → its one group
//	  sk-aw4:                       # a key that may choose
//	    default: batch              #   where it lands unasked
//	    interactive: true           #   and what it may ask for
//	    batch: true
//
// In the mapping form `default:` names the group and every other `name: true`
// permits that group. A `name: false` is written out rather than ignored, so
// revoking one is an edit in place instead of a deletion.
func (k *KeyPolicy) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		k.Group = s
		return nil
	case yaml.MappingNode:
		var raw map[string]yaml.Node
		if err := value.Decode(&raw); err != nil {
			return err
		}
		k.Allow = map[string]bool{}
		for name, node := range raw {
			// RESERVED, like `default`, and for the same reason: this mapping's
			// open form is "any other name permits that group", so a reserved
			// word is the only way to carry anything that is not a group. A
			// priority group actually named `thinking` would collide, which
			// Validate refuses rather than silently reinterpreting.
			if name == "thinking" {
				var b bool
				if err := node.Decode(&b); err != nil {
					return fmt.Errorf("key policy: `thinking` must be true or false: %w", err)
				}
				k.Thinking = &b
				continue
			}
			if name == "mint" {
				if err := node.Decode(&k.Mint); err != nil {
					return fmt.Errorf("key policy: `mint` must be true or false: %w", err)
				}
				continue
			}
			if name == "disabled" {
				if err := node.Decode(&k.Disabled); err != nil {
					return fmt.Errorf("key policy: `disabled` must be true or false: %w", err)
				}
				continue
			}
			if name == "credentials" {
				var list []KeyCredential
				if err := node.Decode(&list); err != nil {
					return fmt.Errorf("key policy: `credentials` must be a list of {credential, limits}: %w", err)
				}
				k.Credentials = list
				continue
			}
			if name == "models" {
				var list []string
				if err := node.Decode(&list); err != nil {
					return fmt.Errorf("key policy: `models` must be a list of selectors: %w", err)
				}
				k.Models = NormalizeSelectors(list)
				continue
			}
			if name == "budgets" {
				var b map[string]LimitSet
				if err := node.Decode(&b); err != nil {
					return fmt.Errorf("key policy: `budgets` must map a selector to a list of limits: %w", err)
				}
				k.Budgets = KeyBudgets(b).Normalized()
				continue
			}
			if name == "limits" {
				if err := node.Decode(&k.Limits); err != nil {
					return fmt.Errorf("key policy: `limits`: %w", err)
				}
				continue
			}
			if name == "default" {
				var s string
				if err := node.Decode(&s); err != nil {
					return fmt.Errorf("key policy: `default` must name a priority group: %w", err)
				}
				k.Group = s
				continue
			}
			var ok bool
			if err := node.Decode(&ok); err != nil {
				return fmt.Errorf("key policy: group %q must be true or false: %w", name, err)
			}
			if ok {
				k.Allow[name] = true
			}
		}
		if k.Group == "" {
			return fmt.Errorf("key policy: no `default` group named; a key must land somewhere when it asks for nothing")
		}
		return nil
	}
	return fmt.Errorf("key policy: want a group name or a mapping, got %v", value.Kind)
}

// MarshalYAML writes the shape UnmarshalYAML reads, which is not the struct's
// own field layout.
//
// It must exist, and the config's revision history is why: every save
// round-trips the whole config through YAML, so a struct that marshals to
// `{group: x, allow: {...}}` and parses only `{default: x, ...}` fails to load
// the thing it just wrote.
//
// A key with no escalations marshals back to the BARE STRING it was written as.
// That keeps every existing config byte-identical across a save, so adding this
// feature does not show up as a diff on every key in the revision history.
func (k KeyPolicy) MarshalYAML() (any, error) {
	if len(k.Allow) == 0 && k.Thinking == nil && len(k.Limits) == 0 && !k.Mint &&
		len(k.Credentials) == 0 && !k.Disabled && len(k.Models) == 0 && len(k.Budgets) == 0 {
		return k.Group, nil
	}
	m := map[string]any{"default": k.Group}
	for g, ok := range k.Allow {
		if ok {
			m[g] = true
		}
	}
	if k.Thinking != nil {
		m["thinking"] = *k.Thinking
	}
	if len(k.Limits) > 0 {
		m["limits"] = k.Limits
	}
	if k.Mint {
		m["mint"] = true
	}
	if len(k.Credentials) > 0 {
		m["credentials"] = k.Credentials
	}
	if k.Disabled {
		m["disabled"] = true
	}
	if len(k.Models) > 0 {
		m["models"] = k.Models
	}
	if len(k.Budgets) > 0 {
		m["budgets"] = k.Budgets
	}
	return m, nil
}

// splitGroup divides a credential into the key and the group it asked for.
//
// exact is the caller's whole credential, tried as a key FIRST by the resolver:
// a key that literally contains the separator must keep working, and this
// feature must not be able to change what an existing credential means. Only
// when the whole string is not a known key is the suffix considered.
func splitGroup(cred string) (key, group string) {
	i := strings.LastIndex(cred, GroupSeparator)
	if i <= 0 || i == len(cred)-1 {
		return cred, ""
	}
	return cred[:i], cred[i+1:]
}

// GroupKeys builds a Keys map from plain key → group pairs: the shape a config
// has when no key escalates, which is every config written before escalation
// existed.
func GroupKeys(m map[string]string) map[string]KeyPolicy {
	out := make(map[string]KeyPolicy, len(m))
	for k, g := range m {
		out[k] = KeyPolicy{Group: g}
	}
	return out
}
