package proxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
)

// Key access and budgets by selector (P65; config/keyaccess.go): what a key
// may see and use (`models`), and what it may spend there (`budgets`). A
// subkey answers to its own entry AND its parent's: it may use what both
// permit, and every matching budget of both is checked and charged.

// keyAccess is one key's models and budgets, from whichever source holds it.
func (p *Proxy) keyAccess(key string) ([]string, config.KeyBudgets) {
	if ck, ok := p.children.get(key); ok {
		return ck.models, ck.budgets
	}
	kp := p.config().Keys[key]
	return kp.Models, kp.Budgets
}

func nonNilSelectors(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilBudgets(in config.KeyBudgets) config.KeyBudgets {
	if in == nil {
		return config.KeyBudgets{}
	}
	return in
}

// keyRole names a key in the chain as the caller reads it.
func keyRole(c config.Caller, key string) string {
	if c.Parent != "" && key == c.Parent {
		return "parent key " + key
	}
	return "key " + key
}

// filterByAccess drops the candidates the caller may not use, and says why
// when none is left: the key (or parent) whose `models` hide them.
//
// A caller with NO key entry — a stranger, served by default on this box —
// forwards with no declared account: budgets and filters live on keys, and
// without this a stranger would reach every paid upstream (owner 10-27, "I'm
// on a network and I don't want anyone using my cost").
func (p *Proxy) filterByAccess(r *http.Request, cands []config.Candidate, c config.Caller) ([]config.Candidate, string) {
	if p.trustedCaller != nil && p.trustedCaller(r, c.Key) {
		return cands, ""
	}
	kept := make([]config.Candidate, 0, len(cands))
	why := ""
	for _, cand := range cands {
		if cand.Credential != nil && !c.Recognized {
			why = "a caller with no corrallm key may not use a paid account (" +
				cand.Credential.ScopeKey(cand.Model.ProviderName) + ")"
			continue
		}
		t := config.TargetOf(cand)
		ok := true
		for _, k := range callerChain(c) {
			models, _ := p.keyAccess(k)
			if !config.ModelsPermit(models, t) {
				ok = false
				why = fmt.Sprintf("%s may not use %s: its models are [%s]", keyRole(c, k), cand.Name, strings.Join(models, ", "))
				break
			}
		}
		if ok {
			kept = append(kept, cand)
		}
	}
	return kept, why
}

// budgetHit is one budget of the caller's chain that applies to a candidate.
type budgetHit struct {
	key, sel, scope string
}

// budgetHits lists every budget in the caller's chain whose selector names
// cand.
func (p *Proxy) budgetHits(c config.Caller, cand config.Candidate) []budgetHit {
	t := config.TargetOf(cand)
	var out []budgetHit
	for _, k := range callerChain(c) {
		_, budgets := p.keyAccess(k)
		for _, sel := range budgets.Matching(t) {
			out = append(out, budgetHit{key: k, sel: sel, scope: config.KeyBudgetScope(k, sel)})
		}
	}
	return out
}

// filterByKeyBudgets drops candidates a full HARD budget in the caller's chain
// refuses, unless a value add of that key covers the model. When nothing is
// left it returns the full scopes (for the refusal) and the soonest wait.
func (p *Proxy) filterByKeyBudgets(cands []config.Candidate, c config.Caller) ([]config.Candidate, []string, time.Duration) {
	if p.quota == nil {
		return cands, nil, 0
	}
	kept := cands[:0:0]
	var full []string
	seen := map[string]bool{}
	wait := time.Duration(0)
	for _, cand := range cands {
		ok := true
		for _, h := range p.budgetHits(c, cand) {
			b, d := p.quota.HardBlocked(h.scope)
			if !b || p.grants.usable(h.key, cand.Name) {
				continue
			}
			ok = false
			if !seen[h.scope] {
				seen[h.scope] = true
				full = append(full, h.scope)
			}
			if wait == 0 || d < wait {
				wait = d
			}
		}
		if ok {
			kept = append(kept, cand)
		}
	}
	return kept, full, wait
}

// chargeCaller charges one served request to every budget of the caller's
// chain that names the backend that served it, and to each key's own `key:`
// and per-model scopes (the spend the dashboard and roster read, budget or
// not). A key that was admitted past a full hard budget pays from its value
// adds — that is the only way it was admitted — decided before this request
// is added to its windows.
func (p *Proxy) chargeCaller(c config.Caller, cand config.Candidate, costUSD float64) {
	if p.quota == nil {
		return
	}
	hits := p.budgetHits(c, cand)
	for _, k := range callerChain(c) {
		scopes := []string{config.KeyScopeKey(k), config.KeyModelScopeKey(k, cand.Name)}
		blocked := false
		for _, h := range hits {
			if h.key != k {
				continue
			}
			if b, _ := p.quota.HardBlocked(h.scope); b {
				blocked = true
			}
			if !hasStr(scopes, h.scope) {
				scopes = append(scopes, h.scope)
			}
		}
		if blocked && costUSD > 0 {
			p.grants.spend(p.store, k, cand.Name, costUSD)
		}
		for _, sc := range scopes {
			p.quota.Charge(sc, config.DimRequests, 1)
			if costUSD > 0 {
				p.quota.Charge(sc, config.DimUSD, costUSD)
			}
		}
	}
}

func hasStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// seedCallerBudgets registers one key's budgets, each under its own scope.
func seedCallerBudgets(set func(string, []config.Limit), key string, budgets config.KeyBudgets) {
	for _, sel := range budgets.Selectors() {
		if ls := budgets[sel]; len(ls) > 0 {
			set(config.KeyBudgetScope(key, sel), ls)
		}
	}
}

// budgetWindows is each of a key's budgets' live windows, keyed by selector.
func (p *Proxy) budgetWindows(key string) map[string][]quota.Window {
	if p.quota == nil {
		return nil
	}
	_, budgets := p.keyAccess(key)
	if len(budgets) == 0 {
		return nil
	}
	bySel := map[string]string{}
	for _, sel := range budgets.Selectors() {
		bySel[config.KeyBudgetScope(key, sel)] = sel
	}
	out := map[string][]quota.Window{}
	for _, e := range p.quota.Snapshot() {
		if sel, ok := bySel[e.Backend]; ok && len(e.Windows) > 0 {
			out[sel] = e.Windows
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// oversubscription warns where parent's subkeys promise more than what they
// sit under: on one selector and one kind of window (unit and length), the
// subkeys' budgets added up exceed the parent's own budget on that selector,
// or a model's, account's or provider's cap that every key shares. Allowed
// (owner 10-28: the parent's own budget stops the total); said, so a reader
// is not surprised by a 429 while each subkey shows room.
func (p *Proxy) oversubscription(parent string) []string {
	type slot struct {
		sel, dim string
		dur      time.Duration
	}
	sums := map[slot]float64{}
	pers := map[slot]string{}
	for _, k := range p.children.of(parent) {
		if k.Disabled {
			continue
		}
		for sel, ls := range k.budgets {
			for _, l := range ls {
				d, ok := l.Window()
				if !ok {
					continue
				}
				s := slot{sel, l.Dimension(), d}
				sums[s] += l.Amount()
				pers[s] = l.Per
			}
		}
	}
	if len(sums) == 0 {
		return nil
	}
	_, pBudgets := p.keyAccess(parent)
	shared := p.sharedCaps()
	var out []string
	for s, sum := range sums {
		words := fmt.Sprintf("subkeys' %s budgets on %s add up to %s", s.dim+"/"+pers[s], s.sel, amountWords(s.dim, sum))
		// The parent's budget on the same selector, and its budget on `*`,
		// which covers every selector.
		for _, psel := range []string{s.sel, config.SelectAll} {
			for _, l := range pBudgets[psel] {
				if d, ok := l.Window(); ok && d == s.dur && l.Dimension() == s.dim && sum > l.Amount() {
					out = append(out, fmt.Sprintf("%s, more than the parent's own %s on %s; the parent's budget stops the total",
						words, l.Compact(), psel))
				}
			}
			if s.sel == config.SelectAll {
				break
			}
		}
		model, literal := strings.CutPrefix(s.sel, "model:")
		if !literal || strings.Contains(model, "*") {
			continue
		}
		for _, c := range shared[model] {
			if c.dur == s.dur && c.dim == s.dim && sum > c.amount {
				out = append(out, fmt.Sprintf("%s, more than %s of %s; every key shares that cap",
					words, scopeOwner(c.scope), amountWords(c.dim, c.amount)))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Oversubscription is oversubscription for the admin API.
func (p *Proxy) Oversubscription(parent string) []string { return p.oversubscription(parent) }

// sharedCap is one window of a cap every key shares (a model's, an account's,
// a provider's, the box's).
type sharedCap struct {
	scope, dim string
	dur        time.Duration
	amount     float64
}

// sharedCaps lists, per served model name, the hard caps its candidates sit
// under, from the ledger's declared windows.
func (p *Proxy) sharedCaps() map[string][]sharedCap {
	if p.quota == nil {
		return nil
	}
	windows := map[string][]quota.Window{}
	for _, e := range p.quota.Snapshot() {
		windows[e.Backend] = e.Windows
	}
	cfg := p.config()
	out := map[string][]sharedCap{}
	for name := range cfg.AllModels() {
		cands, _ := cfg.ResolveServed(name)
		for _, cand := range cands {
			for _, sc := range cand.QuotaScopes(cfg) {
				for _, w := range windows[sc] {
					dim, per, _ := strings.Cut(w.Label, "/")
					d, ok := config.Limit{USD: 1, Per: per}.Window()
					if !ok || w.Limit <= 0 {
						continue
					}
					out[name] = append(out[name], sharedCap{scope: sc, dim: dim, dur: d, amount: w.Limit})
				}
			}
		}
	}
	return out
}

func amountWords(dim string, n float64) string {
	if dim == config.DimUSD {
		return fmt.Sprintf("$%.2f", n)
	}
	return fmt.Sprintf("%g %s", n, dim)
}
