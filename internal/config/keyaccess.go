package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// KEY ACCESS AND BUDGETS BY SELECTOR (P65).
//
// A key says two things about the models it reaches, in one vocabulary:
//
//	keys:
//	  sk-aw4:
//	    default: batch
//	    models: [local, provider:fireworks]     # what it may see and use
//	    budgets:                                # what it may spend there
//	      "*":                  [$20/calendar-day]
//	      model:*deepseek*:     [$3/calendar-day,hard]
//
// A subkey (child_key) carries the same two fields. Before this, the same
// facts were spread over `limits` (key-wide, or one model by `limits[].model`)
// and `credentials` allow patterns with budgets of their own, and a reader had
// to know which of the three said what.
//
// SELECTORS name a set of candidate backends:
//
//	*                    everything
//	local                runs on this box or a host corrallm manages, and
//	                     forwards with no provider account
//	proxy                everything else: a host corrallm does not run, or
//	                     any candidate that forwards with an account
//	provider:<glob>      the provider the model is declared under
//	cred:<prov>/<glob>   the account a candidate forwards with
//	model:<glob>         the backend model's name
//	<name>               a bare name is model:<name>
//
// `models` empty = every model. Every matching budget applies — they nest, so
// "$20/day on everything, $3/day of it on deepseek" is two entries, both
// charged by a deepseek request.

// Selector kinds.
const (
	SelectAll   = "*"
	SelectLocal = "local"
	SelectProxy = "proxy"
)

// Target is what a selector is matched against: one candidate backend.
type Target struct {
	Model    string // backend model name
	Provider string // declared provider (Model.ProviderName)
	Cred     string // credential scope (cred:<provider>/<name>); "" = none
	Remote   bool
}

// TargetOf describes a candidate for selector matching.
func TargetOf(c Candidate) Target {
	t := Target{Model: c.Name, Provider: c.Model.ProviderName, Remote: c.Model.Remote()}
	if c.Credential != nil {
		t.Cred = c.Credential.ScopeKey(c.Model.ProviderName)
	}
	return t
}

// NormalizeSelector trims s and turns a bare model name into model:<name>.
func NormalizeSelector(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == SelectAll || s == SelectLocal || s == SelectProxy:
		return s
	case strings.HasPrefix(s, "provider:"), strings.HasPrefix(s, "cred:"), strings.HasPrefix(s, "model:"):
		return s
	}
	return "model:" + s
}

const selectorSyntax = "*, local, proxy, provider:<name>, cred:<provider>/<name>, model:<name> (* is a wildcard in names)"

// ValidateSelector checks a NORMALIZED selector's shape. It does not check
// that a named model or provider exists: a config must keep loading after a
// model is removed. Callers that take input from a person check names too
// (Config.CheckSelectorNames).
func ValidateSelector(s string) error {
	switch {
	case s == SelectAll || s == SelectLocal || s == SelectProxy:
		return nil
	case strings.HasPrefix(s, "cred:"):
		return ValidateCredentialPattern(s)
	}
	kind, name, ok := strings.Cut(s, ":")
	if !ok || (kind != "provider" && kind != "model") || strings.TrimSpace(name) == "" {
		return fmt.Errorf("selector %q: want %s", s, selectorSyntax)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("selector %q: contains a control character", s)
		}
	}
	return nil
}

// SelectorMatches reports whether a normalized selector names t.
func SelectorMatches(sel string, t Target) bool {
	switch {
	case sel == SelectAll:
		return true
	// An account makes a candidate paid wherever it is served — a provider
	// reached through a loopback tunnel is still the provider — so `local`
	// never admits one.
	case sel == SelectLocal:
		return !t.Remote && t.Cred == ""
	case sel == SelectProxy:
		return t.Remote || t.Cred != ""
	case strings.HasPrefix(sel, "provider:"):
		return globMatch(strings.TrimPrefix(sel, "provider:"), t.Provider)
	case strings.HasPrefix(sel, "cred:"):
		return t.Cred != "" && globMatch(sel, t.Cred)
	case strings.HasPrefix(sel, "model:"):
		return globMatch(strings.TrimPrefix(sel, "model:"), t.Model)
	}
	return false
}

// ModelsPermit reports whether a `models` list lets a key use t. Empty = all.
func ModelsPermit(models []string, t Target) bool {
	if len(models) == 0 {
		return true
	}
	for _, s := range models {
		if SelectorMatches(s, t) {
			return true
		}
	}
	return false
}

// NormalizeSelectors normalizes, drops blanks and duplicates, and sorts.
func NormalizeSelectors(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		n := NormalizeSelector(s)
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// CheckSelectorNames refuses a selector whose literal model or provider name
// this config does not have: a typo would otherwise match nothing and lock a
// key out, or set a budget nothing charges. Globs are not checked.
func (c *Config) CheckSelectorNames(sels []string) error {
	for _, s := range sels {
		if err := ValidateSelector(s); err != nil {
			return err
		}
		kind, name, _ := strings.Cut(s, ":")
		if strings.Contains(name, "*") {
			continue
		}
		switch kind {
		case "model":
			if _, ok := c.AllModels()[name]; !ok {
				return fmt.Errorf("selector %q: no model %q (see GET /v1/models)", s, name)
			}
		case "provider":
			found := false
			for _, m := range c.AllModels() {
				if m.ProviderName == name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("selector %q: no provider %q", s, name)
			}
		}
	}
	return nil
}

// --- budgets ----------------------------------------------------------------

// KeyBudgets is a key's budgets by selector. YAML writes each limit in the
// compact form ("$3/calendar-day,hard"); both YAML and JSON read the compact
// form or the object form.
type KeyBudgets map[string]LimitSet

// Selectors returns the budget selectors, sorted.
func (b KeyBudgets) Selectors() []string {
	out := make([]string, 0, len(b))
	for s := range b {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Matching returns the selectors of every budget that applies to t, sorted.
func (b KeyBudgets) Matching(t Target) []string {
	var out []string
	for _, s := range b.Selectors() {
		if SelectorMatches(s, t) {
			out = append(out, s)
		}
	}
	return out
}

// Normalized normalizes every selector, merging entries that normalize to the
// same one, and drops empty entries.
func (b KeyBudgets) Normalized() KeyBudgets {
	if len(b) == 0 {
		return nil
	}
	out := KeyBudgets{}
	for s, ls := range b {
		if len(ls) == 0 {
			continue
		}
		n := NormalizeSelector(s)
		out[n] = append(out[n], ls...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Validate checks every selector and limit. A limit inside a budget names no
// model: the selector does that.
func (b KeyBudgets) Validate() error {
	for _, s := range b.Selectors() {
		if err := ValidateSelector(s); err != nil {
			return err
		}
		for _, l := range b[s] {
			if l.Model != "" {
				return fmt.Errorf("budget %s: a limit inside a budget names no model; the selector does", s)
			}
		}
		if err := b[s].ValidateKey(); err != nil {
			return fmt.Errorf("budget %s: %w", s, err)
		}
	}
	return nil
}

// MarshalYAML writes each limit in the compact form.
func (b KeyBudgets) MarshalYAML() (any, error) {
	out := map[string][]string{}
	for s, ls := range b {
		for _, l := range ls {
			out[s] = append(out[s], l.Compact())
		}
	}
	return out, nil
}

// KeyBudgetScope is the ledger scope one budget is kept under. The selectors
// that map 1:1 to a scope that existed before P65 keep it, so the counters
// carry over: `*` is the old key-wide allocation, `model:<name>` the old
// share of one model, `cred:…` the old allow-pattern budget.
func KeyBudgetScope(key, sel string) string {
	switch {
	case sel == SelectAll:
		return KeyScopeKey(key)
	case strings.HasPrefix(sel, "model:") && !strings.Contains(sel, "*"):
		return KeyModelScopeKey(key, strings.TrimPrefix(sel, "model:"))
	case strings.HasPrefix(sel, "cred:"):
		return KeyCredentialScopeKey(key, sel)
	}
	return "key:" + key + "|" + sel
}

// --- compact limits ---------------------------------------------------------

// LimitSyntax describes the compact form, for error messages and help text.
const LimitSyntax = "AMOUNT/PER[,hard]: AMOUNT is $N (dollars), Nreq (requests) or Nsec (seconds); " +
	"PER is a window (minute, hour, day, calendar-day, month, calendar-month, total). " +
	"E.g. $3/calendar-day,hard or 100req/hour"

// ParseLimit reads the compact form: "$3/calendar-day,hard", "100req/hour".
func ParseLimit(s string) (Limit, error) {
	var l Limit
	rest := strings.TrimSpace(s)
	if r, ok := strings.CutSuffix(rest, ",hard"); ok {
		l.Hard, rest = true, strings.TrimSpace(r)
	}
	amount, per, ok := strings.Cut(rest, "/")
	amount, per = strings.TrimSpace(amount), strings.TrimSpace(per)
	if !ok || per == "" || amount == "" {
		return l, fmt.Errorf("limit %q: want %s", s, LimitSyntax)
	}
	l.Per = per
	num, unit := amount, DimUSD
	switch {
	case strings.HasPrefix(amount, "$"):
		num = amount[1:]
	case strings.HasSuffix(amount, "usd"):
		num = strings.TrimSuffix(amount, "usd")
	case strings.HasSuffix(amount, "req"):
		num, unit = strings.TrimSuffix(amount, "req"), DimRequests
	case strings.HasSuffix(amount, "sec"):
		num, unit = strings.TrimSuffix(amount, "sec"), DimDwell
	default:
		return l, fmt.Errorf("limit %q: the amount needs a unit ($3, 3usd, 100req or 60sec)", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n <= 0 {
		return l, fmt.Errorf("limit %q: the amount must be a number more than 0", s)
	}
	switch unit {
	case DimUSD:
		l.USD = n
	case DimRequests:
		l.Req = n
	default:
		l.Sec = n
	}
	if _, ok := l.Window(); !ok {
		return l, fmt.Errorf("limit %q: unknown window %q", s, per)
	}
	return l, nil
}

// Compact is ParseLimit's inverse. A model-narrowed limit (the pre-P65 key
// form) adds "@model".
func (l Limit) Compact() string {
	var s string
	switch {
	case l.USD != 0:
		s = "$" + strconv.FormatFloat(l.USD, 'f', -1, 64)
	case l.Req != 0:
		s = strconv.FormatFloat(l.Req, 'f', -1, 64) + "req"
	default:
		s = strconv.FormatFloat(l.Sec, 'f', -1, 64) + "sec"
	}
	s += "/" + l.Per
	if l.Model != "" {
		s += "@" + l.Model
	}
	if l.Hard {
		s += ",hard"
	}
	return s
}

// UnmarshalYAML reads a limit as an object or in the compact form.
func (l *Limit) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := ParseLimit(n.Value)
		if err != nil {
			return err
		}
		*l = v
		return nil
	}
	type plain Limit
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*l = Limit(p)
	return nil
}

// UnmarshalJSON reads a limit as an object or in the compact form.
func (l *Limit) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		v, err := ParseLimit(s)
		if err != nil {
			return err
		}
		*l = v
		return nil
	}
	type plain Limit
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*l = Limit(p)
	return nil
}

// --- migration from limits + credentials ------------------------------------

// LegacyAccess converts the pre-P65 fields to models + budgets, reaching
// EXACTLY what they reached:
//
//   - `limits` → budgets["*"], or budgets["model:<m>"] for `limits[].model`;
//   - no `credentials` → no `models` (everything);
//   - `credentials` patterns admitted every candidate with NO credential plus
//     the candidates whose credential a pattern matched. A candidate with no
//     credential is a local model or a remote provider that declares no
//     accounts, so that is `local`, `provider:<p>` for each such provider
//     (openProviders), and each pattern. `:none` adds no pattern;
//   - a pattern's own budget → budgets[pattern].
func LegacyAccess(limits LimitSet, creds []KeyCredential, openProviders []string) ([]string, KeyBudgets) {
	var budgets KeyBudgets
	add := func(sel string, l Limit) {
		if budgets == nil {
			budgets = KeyBudgets{}
		}
		l.Model = ""
		budgets[sel] = append(budgets[sel], l)
	}
	for _, l := range limits {
		if l.Model != "" {
			add("model:"+l.Model, l)
		} else {
			add(SelectAll, l)
		}
	}
	if len(creds) == 0 {
		return nil, budgets
	}
	models := []string{SelectLocal}
	for _, p := range openProviders {
		models = append(models, "provider:"+p)
	}
	for _, p := range creds {
		if p.Credential == NoCredentialPattern {
			continue
		}
		models = append(models, p.Credential)
		for _, l := range p.Limits {
			add(p.Credential, l)
		}
	}
	return NormalizeSelectors(models), budgets
}

// OpenRemoteProviders lists the providers with a remote model and no declared
// accounts: what a pre-P65 credential pattern list let through unasked.
func (c *Config) OpenRemoteProviders() []string {
	seen := map[string]bool{}
	for _, m := range c.AllModels() {
		if !m.Remote() || m.ProviderName == "" || seen[m.ProviderName] {
			continue
		}
		if ext, ok := c.Extensions[m.Extension]; ok {
			if pv, ok := ext.Providers[m.ProviderName]; ok && len(pv.Credentials) > 0 {
				continue
			}
		}
		seen[m.ProviderName] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// MigrateKeyAccess moves every key's `limits` and `credentials` into `models`
// and `budgets` (LegacyAccess) and clears them. Idempotent. Runs after
// extensions resolve: it needs to know which providers are remote.
func (c *Config) MigrateKeyAccess() {
	var moved []string
	open := []string(nil)
	for key, pol := range c.Keys {
		if len(pol.Limits) == 0 && len(pol.Credentials) == 0 {
			continue
		}
		if open == nil {
			open = c.OpenRemoteProviders()
		}
		models, budgets := LegacyAccess(pol.Limits, pol.Credentials, open)
		pol.Models = NormalizeSelectors(append(pol.Models, models...))
		for s, ls := range budgets {
			if pol.Budgets == nil {
				pol.Budgets = KeyBudgets{}
			}
			pol.Budgets[s] = append(pol.Budgets[s], ls...)
		}
		pol.Limits, pol.Credentials = nil, nil
		c.Keys[key] = pol
		moved = append(moved, key)
	}
	if len(moved) > 0 {
		sort.Strings(moved)
		migrateAccessWarn.Do(func() {
			slog.Info("key `limits` and `credentials` moved into `models` and `budgets` (P65); the old fields are no longer written",
				"keys", strings.Join(moved, " "))
		})
	}
}

var migrateAccessWarn sync.Once

// AccessSummary renders a key's models and budgets in words.
func AccessSummary(models []string, budgets KeyBudgets) string {
	m := "every model"
	if len(models) > 0 {
		m = strings.Join(models, ", ")
	}
	if len(budgets) == 0 {
		return m + "; no budgets"
	}
	parts := make([]string, 0, len(budgets))
	for _, s := range budgets.Selectors() {
		ls := make([]string, 0, len(budgets[s]))
		for _, l := range budgets[s] {
			ls = append(ls, l.Compact())
		}
		parts = append(parts, s+" "+strings.Join(ls, " "))
	}
	return m + "; budgets " + strings.Join(parts, ", ")
}
