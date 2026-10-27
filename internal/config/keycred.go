package config

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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
		if p.Credential == NoCredentialPattern || !globMatch(p.Credential, scope) {
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

// NoCredentialPattern is a pattern that admits NOTHING. It exists for one
// purpose: writing down, in the key's own entry, that the key may use no paid
// account at all.
//
// Before patterns, that was said on the OTHER side — every credential's
// `allow:` list simply omitted the key, so "this key may not spend this account"
// was spelled as an absence across N entries. Absence is not expressible on the
// caller's side, and `credentials:` empty already means the opposite (every
// account), so the migration that moves those lists onto the keys needs a
// pattern that matches nothing and reads as what it is.
//
// It cannot collide: a credential scope always starts with "cred:" and always
// has a provider and a name, so ":none" matches none of them.
const NoCredentialPattern = ":none"

// IsNoCredential reports whether a pattern list says "no account at all", as
// opposed to an EMPTY list, which says "every account".
func IsNoCredential(patterns []KeyCredential) bool {
	for _, p := range patterns {
		if p.Credential == NoCredentialPattern {
			return true
		}
	}
	return false
}

// MigrateCredentialAllowlists moves every credential's `allow:` list onto the
// keys it names, as allow patterns, and clears the lists. It is idempotent:
// with no allow lists left there is nothing to move.
//
// Why a migration and not a load-time fallback: two sources of truth for one
// permission is how they drift. The dashboard writes patterns and the config
// history shows them, so leaving an old list in place means the page can show
// "every account" for a key the proxy still refuses, or the reverse. The old
// field is enforced only until this has run once.
//
// WHAT IT MUST NOT DO is widen access. A credential with an allow list refused
// every key NOT named in it, so:
//
//   - a key named there gets that credential's scope as a pattern;
//   - a key with NO patterns after every list is read gets NoCredentialPattern —
//     it could reach no account before, and an empty list would mean "every
//     account" now, which is a paid upstream handed to a caller nobody approved.
//
// A caller with no key entry at all has nowhere to write a pattern into; the
// proxy refuses such a caller every declared credential (credentialVerdict),
// so the lists' refusal of strangers survives the move.
func (c *Config) MigrateCredentialAllowlists() {
	type scope struct{ allow []string }
	byScope := map[string]scope{}
	found := false
	for xname, ext := range c.Extensions {
		for pname, pv := range ext.Providers {
			if len(pv.Credentials) == 0 {
				continue // the synthesised default credential carries no list
			}
			for i, cr := range pv.Credentials {
				if len(cr.Allow) == 0 {
					continue
				}
				found = true
				byScope[cr.ScopeKey(pname)] = scope{allow: cr.Allow}
				// Cleared, so no later write emits a field that no longer decides
				// anything — a stale list in stored config is a second answer to a
				// question the keys have already answered.
				pv.Credentials[i].Allow = nil
				ext.Providers[pname] = pv
			}
		}
		c.Extensions[xname] = ext
	}
	if !found {
		return
	}
	// Sorted so the patterns a key gains do not depend on map order: the same
	// config must migrate to the same thing on every load.
	scopes := make([]string, 0, len(byScope))
	for sc := range byScope {
		scopes = append(scopes, sc)
	}
	sort.Strings(scopes)
	for key, pol := range c.Keys {
		for _, sc := range scopes {
			if !allowsKey(byScope[sc].allow, key) {
				continue
			}
			// A key that already HAS this pattern keeps it, and its budget: this
			// runs on every load, and appending twice would grow the list each
			// boot.
			if containsPattern(pol.Credentials, sc) {
				continue
			}
			pol.Credentials = append(pol.Credentials, KeyCredential{Credential: sc, Limits: LimitSet{}})
		}
		if len(pol.Credentials) == 0 {
			pol.Credentials = []KeyCredential{{Credential: NoCredentialPattern, Limits: LimitSet{}}}
		}
		c.Keys[key] = pol
	}
	c.warnMigratedAllowlists(scopes)
}

// warnMigratedAllowlists names the accounts whose list moved, once per process
// (the daemon reloads config on every save).
func (c *Config) warnMigratedAllowlists(scopes []string) {
	migrateWarn.Do(func() {
		slog.Info("credential `allow:` lists moved onto the keys they named (P63); the field is no longer read",
			"accounts", strings.Join(scopes, " "))
	})
}

var migrateWarn sync.Once

func allowsKey(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

func containsPattern(list []KeyCredential, pattern string) bool {
	for _, p := range list {
		if p.Credential == pattern {
			return true
		}
	}
	return false
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
	// The one pattern that is deliberately shaped like no credential at all; see
	// NoCredentialPattern.
	if p == NoCredentialPattern {
		return nil
	}
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
	if IsNoCredential(patterns) {
		return "no paid account"
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
