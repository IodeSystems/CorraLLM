package proxy

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// Value adds (P62): a one-time amount on a caller key — "$10, expires in two
// days" — spent only once the key's own hard allocation refuses. An overflow
// pool rather than a raised limit, so the extra is spent once, not once per
// window: on a $3/day key, a $10 value add over two days is $10 more in all,
// not $13 a day.
//
// A value add covers the KEY's allocation only. A model's or provider's own
// budget (the model-wide deepseek cap) still refuses: that is a different
// owner's limit.

// grantBook is the in-memory copy of key_grant, read on every request.
type grantBook struct {
	mu    sync.Mutex
	byKey map[string][]*store.KeyGrant
	now   func() time.Time // tests
}

func (g *grantBook) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// active reports whether a grant can still pay at now.
func grantActive(gr *store.KeyGrant, now time.Time) bool {
	return gr.Spent < gr.USD && (gr.ExpiresMS == 0 || now.UnixMilli() < gr.ExpiresMS)
}

// any reports whether key has an active value add for any model.
func (g *grantBook) any(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	for _, gr := range g.byKey[key] {
		if grantActive(gr, now) {
			return true
		}
	}
	return false
}

// usable reports whether key has an active value add that model may spend.
func (g *grantBook) usable(key, model string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	for _, gr := range g.byKey[key] {
		if grantActive(gr, now) && (gr.Model == "" || gr.Model == model) {
			return true
		}
	}
	return false
}

// spend takes usd from key's value adds that model may spend, the one expiring
// soonest first (a grant without expiry last), so nothing lapses unspent that
// a later one could have covered. What no grant can cover is dropped: the
// request was admitted on a grant, and charging after the fact overshoots by
// at most one request, as everywhere in the ledger.
func (g *grantBook) spend(st *store.Store, key, model string, usd float64) {
	g.mu.Lock()
	now := g.clock()
	var use []*store.KeyGrant
	for _, gr := range g.byKey[key] {
		if grantActive(gr, now) && (gr.Model == "" || gr.Model == model) {
			use = append(use, gr)
		}
	}
	sort.SliceStable(use, func(i, j int) bool {
		a, b := use[i].ExpiresMS, use[j].ExpiresMS
		if (a == 0) != (b == 0) {
			return b == 0
		}
		return a < b
	})
	type take struct {
		id  int64
		usd float64
	}
	var takes []take
	for i, gr := range use {
		if usd <= 0 {
			break
		}
		amt := min(usd, gr.USD-gr.Spent)
		if i == len(use)-1 {
			amt = usd // the last one takes the overshoot
		}
		gr.Spent += amt
		usd -= amt
		takes = append(takes, take{gr.ID, amt})
	}
	g.mu.Unlock()
	if st == nil {
		return
	}
	for _, t := range takes {
		if _, err := st.SpendKeyGrant(t.id, t.usd); err != nil {
			slog.Warn("value add: spend not saved", "grant", t.id, "err", err)
		}
	}
}

// of lists key's grants, oldest first, as copies.
func (g *grantBook) of(key string) []store.KeyGrant {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]store.KeyGrant, 0, len(g.byKey[key]))
	for _, gr := range g.byKey[key] {
		out = append(out, *gr)
	}
	return out
}

func (g *grantBook) put(gr store.KeyGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byKey == nil {
		g.byKey = map[string][]*store.KeyGrant{}
	}
	for _, cur := range g.byKey[gr.Key] {
		if cur.ID == gr.ID {
			*cur = gr
			return
		}
	}
	c := gr
	g.byKey[gr.Key] = append(g.byKey[gr.Key], &c)
}

// remove drops a grant by id and returns it.
func (g *grantBook) remove(id int64) (store.KeyGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, list := range g.byKey {
		for i, gr := range list {
			if gr.ID == id {
				g.byKey[k] = append(list[:i:i], list[i+1:]...)
				return *gr, true
			}
		}
	}
	return store.KeyGrant{}, false
}

// get returns a grant by id.
func (g *grantBook) get(id int64) (store.KeyGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, list := range g.byKey {
		for _, gr := range list {
			if gr.ID == id {
				return *gr, true
			}
		}
	}
	return store.KeyGrant{}, false
}

// dropKey forgets every grant of key (the key was deleted).
func (g *grantBook) dropKey(key string) []store.KeyGrant {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []store.KeyGrant
	for _, gr := range g.byKey[key] {
		out = append(out, *gr)
	}
	delete(g.byKey, key)
	return out
}

func (p *Proxy) loadGrants() {
	if p.store == nil {
		return
	}
	rows, err := p.store.LoadKeyGrants()
	if err != nil {
		slog.Warn("value adds: load failed", "err", err)
		return
	}
	for _, r := range rows {
		p.grants.put(r)
	}
}

// GrantView is a value add as the APIs show it.
type GrantView struct {
	ID      int64   `json:"id"`
	Key     string  `json:"key,omitempty"`
	USD     float64 `json:"usd"`
	Spent   float64 `json:"spent"`
	Model   string  `json:"model,omitempty" doc:"Empty = any model."`
	Expires string  `json:"expires,omitempty" doc:"RFC 3339; empty = until spent."`
	Created string  `json:"created"`
	Active  bool    `json:"active"`
	Note    string  `json:"note,omitempty"`
}

func grantViews(gs []store.KeyGrant) []GrantView {
	now := time.Now()
	out := make([]GrantView, 0, len(gs))
	for _, g := range gs {
		v := GrantView{ID: g.ID, Key: g.Key, USD: g.USD, Spent: g.Spent, Model: g.Model, Note: g.Note,
			Created: time.UnixMilli(g.CreatedMS).UTC().Format(time.RFC3339), Active: grantActive(&g, now)}
		if g.ExpiresMS > 0 {
			v.Expires = time.UnixMilli(g.ExpiresMS).UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}
