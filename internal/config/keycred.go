package config

import (
	"fmt"
	"strings"
)

// A key's ALLOW PATTERNS: which provider credentials it may forward with, and
// what each may cost.
//
// This used to be the other way round — a credential carried `allow: [keys]` —
// and that was backwards. Who may spend an account is a fact about the CALLER,
// so it belongs on the caller's entry: one key's permissions read as one block
// instead of being scattered across every provider in the config, and a
// provider entry stays about an ENDPOINT and an account rather than about the
// people using it.
//
// The patterns are globs over a credential's scope (`cred:<provider>/<name>`,
// the same identity budgets are kept under), so `cred:fireworks/*` is every
// account of one provider and `cred:fireworks/team-*` is a family of them.
// A key with no patterns may use every credential, which is what every key did
// before this existed.
//
// MOST SPECIFIC WINS, and a pattern may carry its own budgets. That is the
// point of writing them down: "$2/day on fireworks, but $20/day on the one
// account that pays for itself" is two patterns, the narrower one naming the
// credential exactly. A matched pattern's limits become a ledger scope of their
// own, gated before a candidate is walked and charged on the request it
// admits, so a narrow budget never hides inside a wide one.

// KeyCredential is one allow pattern on a key.
type KeyCredential struct {
	// Credential is a glob over `cred:<provider>/<name>`. `*` matches any run.
	Credential string `yaml:"credential" json:"credential"`
	// Limits budget what this pattern admits. Same shape and hard/soft meaning
	// as a key's own allocation; a `per: total` one is a one-off ceiling.
	Limits LimitSet `yaml:"limits,omitempty" json:"limits"`
}

// KeyCredentialScopeKey is the ledger identity of one allow pattern's budget.
//
// The separator is ">" rather than the ":"/"@" the other scopes use because the
// pattern itself contains both (`cred:fireworks/main`), and a scope that
// re-serializes ambiguously would merge two patterns' counters.
func KeyCredentialScopeKey(key, pattern string) string {
	return "keycred:" + key + ">" + pattern
}

// ToKeyCredentials normalizes a configured pattern list: pattern trimmed, and
// limits never nil so a caller may range over them without a check.
func ToKeyCredentials(in []KeyCredential) []KeyCredential {
	if len(in) == 0 {
		return nil
	}
	out := make([]KeyCredential, 0, len(in))
	for _, c := range in {
		k := KeyCredential{Credential: strings.TrimSpace(c.Credential), Limits: c.Limits}
		if k.Limits == nil {
			k.Limits = LimitSet{}
		}
		out = append(out, k)
	}
	return out
}

// AllowedCredential resolves one credential scope against a key's patterns and
// returns the MOST SPECIFIC matching pattern's limits.
//
// ok is false when nothing matches. The caller tells "no patterns at all"
// (every credential allowed, the pre-existing behaviour) from "some patterns,
// none matching" (refused) by testing the list itself, which is why this
// function never guesses at that: whether an empty list means everything is a
// decision for the code doing the permitting, not for the matcher.
func AllowedCredential(patterns []KeyCredential, scope string) (limits LimitSet, ok bool) {
	p, ok := MatchCredential(patterns, scope)
	return p.Limits, ok
}

// MatchCredential is AllowedCredential with the winning PATTERN returned, not
// just its limits. The pattern is a budget identity (KeyCredentialScopeKey), so
// whoever gates on a budget and whoever charges it must agree on which pattern
// it was — recomputing the winner in two places is how a gate ends up checking a
// scope nothing was ever seeded under.
func MatchCredential(patterns []KeyCredential, scope string) (KeyCredential, bool) {
	best := -1
	var bestLen, bestLit int
	for i, p := range patterns {
		if !globMatch(p.Credential, scope) {
			continue
		}
		length, literal := specificity(p.Credential)
		if best < 0 || length > bestLen || (length == bestLen && literal > bestLit) ||
			(length == bestLen && literal == bestLit && p.Credential < patterns[best].Credential) {
			best, bestLen, bestLit = i, length, literal
		}
	}
	if best < 0 {
		return KeyCredential{}, false
	}
	return patterns[best], true
}

// CredentialPatternWins reports whether pattern a is the more specific of two,
// which is the rule that picks one budget when several patterns match: the one
// spelling out more of the credential is the one written about it.
func CredentialPatternWins(a, b string) bool {
	al, an := specificity(a)
	bl, bn := specificity(b)
	if al != bl {
		return al > bl
	}
	if an != bn {
		return an > bn
	}
	return a < b
}

// specificity ranks a pattern: its length, then the bytes it pins before its
// first wildcard.
//
// Length first because it is the useful reading of "most specific": the pattern
// that spells the most of the credential out is the one written about THIS
// credential, while `cred:*` fits the shape of anything. The literal count
// breaks ties, so among equal lengths the one that names more of the front of
// the scope wins.
func specificity(p string) (length, literal int) {
	if i := strings.Index(p, "*"); i >= 0 {
		return len(p), i
	}
	return len(p), len(p)
}

// PermitsCredential reports whether a key with these patterns may forward with
// scope, and which limits that permission carries. No patterns allows
// everything, with no budget of its own.
func PermitsCredential(patterns []KeyCredential, scope string) (limits LimitSet, ok bool) {
	if len(patterns) == 0 {
		return nil, true
	}
	return AllowedCredential(patterns, scope)
}

// ValidateKeyCredentials checks one key's patterns: a pattern must be a
// well-formed credential scope (a typo would otherwise silently match nothing
// and lock the key out), each pattern's limits are validated as a key
// allocation, and no two patterns may be spelled identically — that is one
// permission written twice, whose budget would be charged under whichever copy
// the scanner reached first.
func ValidateKeyCredentials(patterns []KeyCredential) error {
	seen := map[string]bool{}
	for _, p := range patterns {
		pat := strings.TrimSpace(p.Credential)
		if err := ValidateCredentialPattern(pat); err != nil {
			return err
		}
		if seen[pat] {
			return fmt.Errorf("two entries for %q — one permission written twice", pat)
		}
		seen[pat] = true
		if err := p.Limits.ValidateKey(); err != nil {
			return fmt.Errorf("%s limits: %w", pat, err)
		}
	}
	return nil
}

// ValidateCredentialPattern checks the shape a pattern must have to be able to
// match anything: the cred: prefix, a provider, a slash, a credential name.
// Wildcards are allowed anywhere in the provider and name.
func ValidateCredentialPattern(p string) error {
	const want = `a credential scope: cred:<provider>/<name>, with * as a wildcard (e.g. cred:fireworks/*)`
	if !strings.HasPrefix(p, "cred:") {
		return fmt.Errorf("credential %q: want %s", p, want)
	}
	rest := strings.TrimPrefix(p, "cred:")
	if i := strings.Index(rest, "/"); i <= 0 || i == len(rest)-1 {
		return fmt.Errorf("credential %q: want %s", p, want)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("credential %q: contains a control character", p)
		}
	}
	return nil
}

// KeyCredentialsSummary renders a key's patterns in words: what it may use, and
// what each pattern may cost.
func KeyCredentialsSummary(patterns []KeyCredential) string {
	if len(patterns) == 0 {
		return "every credential"
	}
	parts := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if len(p.Limits) == 0 {
			parts = append(parts, p.Credential)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Credential, limitsWords(p.Limits)))
	}
	return strings.Join(parts, ", ")
}

// limitsWords is a one-line rendering of a limit set, for a summary sentence.
func limitsWords(ls LimitSet) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		amount := ""
		switch {
		case l.USD > 0:
			amount = fmt.Sprintf("$%g", l.USD)
		case l.Req > 0:
			amount = fmt.Sprintf("%g req", l.Req)
		case l.Sec > 0:
			amount = fmt.Sprintf("%gs", l.Sec)
		}
		if l.Per != "" {
			amount += "/" + l.Per
		}
		if l.Model != "" {
			amount += " on " + l.Model
		}
		if l.Hard {
			amount += " hard"
		}
		parts = append(parts, amount)
	}
	return strings.Join(parts, ", ")
}
