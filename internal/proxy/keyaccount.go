package proxy

import (
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/store"
)

// The account view of a key (P74): its own budgets as chunks and ceilings, and
// what it can really spend. Every request is gated by the provider, the key
// and the subkey (owner decision 5), so a nominal budget above what the chain
// leaves is not money: the EFFECTIVE amount is the minimum over the subkey's
// budget, its key's, and the providers the budget reaches, and the view names
// which of them binds.

// KeyAccount is `keys list`'s account object.
type KeyAccount struct {
	// Budgets are this key's own, and those of its parent, by selector.
	Budgets []KeyBudgetAccount `json:"budgets"`
	// Subkeys is each subkey's spend and effective budgets (a parent only).
	Subkeys []SubkeyAccount `json:"subkeys,omitempty"`
}

// KeyBudgetAccount is one selector's money.
type KeyBudgetAccount struct {
	Selector string `json:"selector"`
	// The key's own budget on the selector; absent when only one above it has one.
	*Account
	// EffectiveUSD is what can be spent on the selector now: the least over
	// the key, its parent and the providers. Absent: nothing bounds it.
	EffectiveUSD *float64 `json:"effective_usd,omitempty"`
	LimitedBy    string   `json:"limited_by,omitempty"`
}

// SubkeyAccount is one subkey in its parent's view.
type SubkeyAccount struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// SpentTodayUSD is what it has spent since 00:00 UTC (the activity log).
	SpentTodayUSD float64            `json:"spent_today_usd"`
	Budgets       []KeyBudgetAccount `json:"budgets"`
}

// chainLink is one key in the gate's chain, as the view names it.
type chainLink struct {
	key, name string
}

// keyAccountCtx holds what every budget's view reads once per listing.
type keyAccountCtx struct {
	p         *Proxy
	now       time.Time
	windows   map[string][]quota.Window // by ledger scope
	providers map[string]*Account       // by config provider name
	targets   []config.Target
}

// keyAccount is the account view for key: its own and its parent's budgets,
// and, for a key with subkeys, each subkey's.
func (p *Proxy) keyAccount(key string, now time.Time) KeyAccount {
	cx := p.newKeyAccountCtx(now)
	self := []chainLink{{key, "this key"}}
	if ck, ok := p.children.get(key); ok && ck.Parent != "" {
		self = append(self, chainLink{ck.Parent, "parent key " + p.keyName(ck.Parent)})
	}
	out := KeyAccount{Budgets: cx.budgets(self)}
	kids := p.children.of(key)
	if len(kids) == 0 {
		return out
	}
	spent := map[string]float64{}
	if p.store != nil {
		rows, err := p.store.RollupByKey(store.Since(config.PeriodStart("day", 0, now).UnixMilli()))
		if err != nil {
			slog.Warn("key account: spend not read", "err", err)
		}
		for _, r := range rows {
			spent[r.Key] = r.CostUSD
		}
	}
	for _, k := range kids {
		chain := []chainLink{{k.Key, "subkey " + firstNonEmpty(k.Label, "(no label)")}, {key, "this key"}}
		out.Subkeys = append(out.Subkeys, SubkeyAccount{Key: k.Key, Label: k.Label, SpentTodayUSD: spent[k.Key],
			Budgets: cx.budgets(chain)})
	}
	return out
}

// keyName is a key as the view shows it: its label, never the secret.
func (p *Proxy) keyName(key string) string {
	if ck, ok := p.children.get(key); ok && ck.Label != "" {
		return ck.Label
	}
	if kp, ok := p.config().Keys[key]; ok && kp.Label != "" {
		return kp.Label
	}
	return "(no label)"
}

func (p *Proxy) newKeyAccountCtx(now time.Time) *keyAccountCtx {
	cx := &keyAccountCtx{p: p, now: now, windows: map[string][]quota.Window{}, providers: map[string]*Account{}}
	if p.quota != nil {
		for _, e := range p.quota.Snapshot() {
			cx.windows[e.Backend] = e.Windows
		}
	}
	if rep, err := p.Spend(now); err != nil {
		slog.Warn("key account: spend report", "err", err)
	} else {
		for _, pr := range rep.Providers {
			for _, n := range pr.names {
				cx.providers[n] = pr.Account
			}
		}
	}
	cfg := p.config()
	names := make([]string, 0)
	for n := range cfg.AllModels() {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cands, _ := cfg.ResolveServed(n)
		for _, c := range cands {
			cx.targets = append(cx.targets, config.TargetOf(c))
		}
	}
	return cx
}

// budgets is the view of every selector a hard dollar budget of the chain
// names; chain[0] is the key the view is for.
func (cx *keyAccountCtx) budgets(chain []chainLink) []KeyBudgetAccount {
	type held struct {
		link  chainLink
		sel   string
		scope acctScope
	}
	var all []held
	var sels []string
	for _, l := range chain {
		_, budgets := cx.p.keyAccess(l.key)
		for _, sel := range budgets.Selectors() {
			s := cx.keyScope(l.key, sel, budgets[sel])
			if len(s.windows) == 0 {
				continue
			}
			all = append(all, held{l, sel, s})
			if !slices.Contains(sels, sel) {
				sels = append(sels, sel)
			}
		}
	}
	out := make([]KeyBudgetAccount, 0, len(sels))
	for _, sel := range sels {
		e := KeyBudgetAccount{Selector: sel}
		best, whose := math.Inf(1), ""
		for _, h := range all {
			if h.link.key == chain[0].key && h.sel == sel {
				a := accountOf([]acctScope{h.scope}, nil, cx.now)
				e.Account = &a
			}
			if !cx.covers(h.sel, sel) {
				continue
			}
			if r, ok := h.scope.room(); ok && r < best {
				best, whose = r, h.link.name
			}
		}
		if r, who, ok := cx.providerRoom(sel); ok && r < best {
			best, whose = r, who
		}
		if !math.IsInf(best, 1) {
			e.EffectiveUSD, e.LimitedBy = &best, whose
		}
		out = append(out, e)
	}
	return out
}

// keyScope is one key budget's windows, levels from the live ledger, and the
// key's value adds, which pay past any full hard window of the key.
func (cx *keyAccountCtx) keyScope(key, sel string, ls config.LimitSet) acctScope {
	live := cx.windows[config.KeyBudgetScope(key, sel)]
	level := func(l config.Limit) (float64, float64, bool) {
		for _, w := range live {
			if w.Label == l.Label() {
				return w.Used, w.Limit, true
			}
		}
		return 0, 0, false
	}
	s := acctScope{pastPace: true}
	for _, l := range ls {
		if l.Dimension() != config.DimUSD || !l.Hard {
			continue
		}
		used, limit, ok := level(l)
		if !ok {
			limit = l.Amount()
		}
		s.windows = append(s.windows, acctWindow{name: sel + " " + l.Per, scope: sel, lim: l, limit: limit, used: used})
	}
	for _, g := range cx.p.grants.of(key) {
		note := g.Note
		if g.Model != "" {
			note = strings.TrimSpace("only " + g.Model + "; " + note)
		}
		s.extras = append(s.extras, acctExtra{name: "value add " + strconv.FormatInt(g.ID, 10), kind: "value-add", usd: g.USD, spent: g.Spent,
			expiresMS: g.ExpiresMS, active: grantActive(&g, cx.now), note: strings.TrimSuffix(note, "; "), pays: g.Model == ""})
	}
	return s
}

// covers reports whether every model sel reaches is also reached by p, so p's
// budget bounds all of sel's spend. With no model to judge by, only the same
// selector or "*" does.
func (cx *keyAccountCtx) covers(p, sel string) bool {
	if p == sel || p == config.SelectAll {
		return true
	}
	seen := false
	for _, t := range cx.targets {
		if !config.SelectorMatches(sel, t) {
			continue
		}
		seen = true
		if !config.SelectorMatches(p, t) {
			return false
		}
	}
	return seen
}

// providerRoom is what the providers sel reaches can still pay: their
// available amounts added up. ok=false when sel reaches a model with no
// bounded provider (local, or a provider with no hard dollar limit).
func (cx *keyAccountCtx) providerRoom(sel string) (float64, string, bool) {
	var names []string
	for _, t := range cx.targets {
		if !config.SelectorMatches(sel, t) {
			continue
		}
		a := cx.providers[t.Provider]
		if t.Provider == "" || a == nil || a.AvailableUSD == nil {
			return 0, "", false
		}
		if !slices.Contains(names, t.Provider) {
			names = append(names, t.Provider)
		}
	}
	if len(names) == 0 {
		return 0, "", false
	}
	sort.Strings(names)
	var sum float64
	for _, n := range names {
		sum += *cx.providers[n].AvailableUSD
	}
	who := "provider " + names[0]
	if len(names) > 1 {
		who = "providers " + strings.Join(names, " + ")
	}
	return sum, who, true
}
