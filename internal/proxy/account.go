package proxy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/quota"
	"github.com/iodesystems/corrallm/internal/store"
)

// Account view (P74): the owner reads money as CHUNKS, each with an amount,
// what is left, an expiry, a refill and a banked flag. Chunks are a VIEW over
// the limits and timed budgets the ledger already has (owner decision 1, 11-07),
// not a new primitive. The mapping:
//
//	calendar-day hard limit  → chunk "day", not banked, refills at 00:00 UTC
//	paced calendar-day       → chunk "paced-day", not banked, refills at 00:00 UTC;
//	                           what is unspent goes back to the month it is cut from
//	total hard limit         → chunk "once", banked, no expiry, no refill
//	timed budget (provider)  → chunk "budget", banked by its flag, with its expiry
//	value add (key)          → chunk "value-add", banked, expiry optional
//	calendar-month / rolling → a ceiling: a cap, not money of its own
//
// Only HARD dollar limits are in the view. A soft limit steers the walk and is
// served past when nothing else can serve, so it neither holds money nor caps it.

// drainRule is the order the owner decided (P74 S4). drain_order is the order
// the ledger really spends in, and drain_note says where the two differ. A
// provider scope spends in the rule's order (spend.go provChunks); a key's
// limits are still spent together before its value adds.
const drainRule = "non-renewing expiring first (soonest), then renewing, then banked"

// Account is one provider's or key budget's money, as chunks and ceilings.
type Account struct {
	// AvailableUSD is what can be spent now. Absent: no hard dollar limit binds.
	// Negative: overdrawn (a refusal until a refill or a top-up).
	AvailableUSD *float64         `json:"available_usd,omitempty"`
	Overdrawn    bool             `json:"overdrawn"`
	OverdrawnUSD *float64         `json:"overdrawn_usd,omitempty"`
	Chunks       []AccountChunk   `json:"chunks"`
	Ceilings     []AccountCeiling `json:"ceilings"`
	Drain        string           `json:"drain"`
	DrainOrder   []string         `json:"drain_order,omitempty"`
	DrainNote    string           `json:"drain_note,omitempty"`
}

// AccountChunk is one spendable amount.
type AccountChunk struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"` // day | paced-day | once | budget | value-add
	USD     float64 `json:"usd"`
	LeftUSD float64 `json:"left_usd"` // negative = overdrawn
	Banked  bool    `json:"banked"`
	// Expires is when what is left lapses (RFC 3339).
	Expires string `json:"expires,omitempty"`
	// RefillsAt is when the chunk is next topped up to USD (RFC 3339).
	RefillsAt string `json:"refills_at,omitempty"`
	Note      string `json:"note,omitempty"`
}

// AccountCeiling is a hard cap that is not money of its own: a calendar month,
// a rolling window. Every chunk is spent under it.
type AccountCeiling struct {
	Scope    string  `json:"scope"`
	Window   string  `json:"window"`
	USD      float64 `json:"usd"`
	UsedUSD  float64 `json:"used_usd"`
	ResetsAt string  `json:"resets_at,omitempty"` // absent for a rolling window
}

// acctWindow is one dollar limit with its level now.
type acctWindow struct {
	name  string // as the view shows it; never a key's secret
	scope string // as the view shows it
	lim   config.Limit
	limit float64 // the amount now; a paced day's is computed
	used  float64
}

// acctExtra is a timed budget or a value add: money of its own, beside the limits.
type acctExtra struct {
	name      string
	kind      string
	usd       float64
	spent     float64
	expiresMS int64 // 0 = no expiry
	active    bool
	note      string
	pays      bool // counted in the room (a value add narrowed to one model is not)
	banked    bool
}

// acctScope is one ledger scope. A key's scope (not chunked) charges every
// request to all of its windows, and its value adds pay past any full hard
// window (filterByKeyBudgets uses HardBlocked). A provider's scope is CHUNKED
// (spend.go provChunks): its day and total windows and its timed budgets are
// chunks spent one after another, and its other windows are ceilings.
type acctScope struct {
	windows  []acctWindow
	extras   []acctExtra
	pastPace bool
	chunked  bool
}

// isChunk reports whether w is a chunk window of a chunked scope, as the
// ledger's counterWindow.chunk decides (every window here is hard and in usd).
func isChunk(w acctWindow) bool {
	cal := w.lim.Calendar()
	return cal == "day" || cal == "total"
}

// paidPast is what the extras have paid since they were made. A window's level
// keeps rising while they pay (every charge goes to every window), so spend
// past a limit that an extra paid is not the limit's overdraft. Extras' spend is
// not time-stamped, so all of it is taken off: an older extra's spend can hide a
// real overdraft, never invent one.
func (s acctScope) paidPast() float64 {
	var sum float64
	for _, e := range s.extras {
		sum += e.spent
	}
	return sum
}

// left is a window's own money left, net of what the extras paid past it.
func (s acctScope) left(w acctWindow) float64 {
	used := w.used
	if s.chunked {
		return w.limit - used // a chunk's counter holds what it paid, no more
	}
	if used > w.limit {
		used = max(used-s.paidPast(), w.limit)
	}
	return w.limit - used
}

func (s acctScope) extraLeft() float64 {
	var sum float64
	for _, e := range s.extras {
		if e.active && e.pays {
			sum += e.usd - e.spent
		}
	}
	return sum
}

// room is what the scope admits before it refuses; bound=false when no hard
// dollar limit is on it.
//
// The windows are spent together: every request is charged to all of them, so
// the tightest decides. When it is full the extras pay, until they run out:
//
//	head  = min over the windows of (limit − used)
//	head ≤ 0, extras left > 0  → room = extras left (they pay now)
//	otherwise                  → room = head + extras left
//
// For a provider an extra pays only past a NON-paced window (a timed budget
// pays past the month, not past today's share of it), so when the paced day is
// the tighter one the extras never start: room = the paced day's head.
func (s acctScope) room() (float64, bool) {
	if len(s.windows) == 0 {
		return 0, false
	}
	if s.chunked {
		return s.chunkedRoom()
	}
	head, paced := math.Inf(1), math.Inf(1)
	for _, w := range s.windows {
		l := s.left(w)
		if w.lim.Pace && !s.pastPace {
			paced = min(paced, l)
		} else {
			head = min(head, l)
		}
	}
	extra := s.extraLeft()
	switch {
	case head <= 0 && extra > 0:
		return extra, true
	case paced < head:
		return paced, true
	case math.IsInf(head, 1):
		return paced, true
	}
	return head + extra, true
}

// chunkedRoom is a chunked scope's room: what its chunks hold, under the
// tightest ceiling. With no chunk the ceilings alone bound it; with every chunk
// spent it is their (negative) overdraft.
//
//	room = min(min over ceilings of (limit − used), chunks' left)
func (s acctScope) chunkedRoom() (float64, bool) {
	head, pos, neg, chunks := math.Inf(1), 0.0, 0.0, 0
	add := func(l float64) {
		chunks++
		if l > 0 {
			pos += l
		} else {
			neg += l
		}
	}
	for _, w := range s.windows {
		if isChunk(w) {
			add(s.left(w))
		} else {
			head = min(head, w.limit-w.used)
		}
	}
	for _, e := range s.extras {
		if e.active && e.pays {
			add(e.usd - e.spent)
		}
	}
	switch {
	case chunks == 0:
		return head, !math.IsInf(head, 1)
	case pos > 0:
		return min(head, pos), true
	}
	return min(head, neg), true
}

// chunks and ceilings of a scope, in drain order. A chunked (provider) scope:
// unbanked extras soonest-expiring first, the day chunks, banked extras, the
// once chunks, as provChunks spends them. A key's scope: its limits (spent
// together) first, then the extras soonest-expiring first, a no-expiry one
// last, as grantBook.spend takes them.
func (s acctScope) view(now time.Time) ([]AccountChunk, []AccountCeiling) {
	var limits, days, onces []AccountChunk // limits: both, in config order
	var ceilings []AccountCeiling
	for _, w := range s.windows {
		cal := w.lim.Calendar()
		switch {
		case cal == "day":
			c := AccountChunk{Name: w.name, Kind: "day", USD: w.limit, LeftUSD: s.left(w),
				RefillsAt: config.PeriodStart("day", 0, now).AddDate(0, 0, 1).Format(time.RFC3339),
				Note:      "use it or lose it: unspent lapses at the refill"}
			if w.lim.Pace {
				c.Kind, c.Note = "paced-day", "today's share of the month; unspent goes back to the month"
			}
			days, limits = append(days, c), append(limits, c)
		case cal == "total":
			c := AccountChunk{Name: w.name, Kind: "once", USD: w.limit, LeftUSD: s.left(w), Banked: true,
				Note: "one-off: refilled only by raising it"}
			onces, limits = append(onces, c), append(limits, c)
		default:
			c := AccountCeiling{Scope: w.scope, Window: w.lim.Per, USD: w.limit, UsedUSD: w.used}
			if cal == "month" {
				c.ResetsAt = config.PeriodStart("month", w.lim.Renews, now).AddDate(0, 1, 0).Format(time.RFC3339)
			}
			ceilings = append(ceilings, c)
		}
	}
	extras := make([]acctExtra, 0, len(s.extras))
	for _, e := range s.extras {
		if e.active {
			extras = append(extras, e)
		}
	}
	sort.SliceStable(extras, func(i, j int) bool {
		a, b := extras[i].expiresMS, extras[j].expiresMS
		if (a == 0) != (b == 0) {
			return b == 0
		}
		return a < b
	})
	var unbanked, banked []AccountChunk
	for _, e := range extras {
		c := AccountChunk{Name: e.name, Kind: e.kind, USD: e.usd, LeftUSD: e.usd - e.spent, Banked: e.banked, Note: e.note}
		if e.expiresMS > 0 {
			c.Expires = time.UnixMilli(e.expiresMS).UTC().Format(time.RFC3339)
		}
		if e.banked || !s.chunked {
			banked = append(banked, c)
		} else {
			unbanked = append(unbanked, c)
		}
	}
	if !s.chunked {
		return append(limits, banked...), ceilings
	}
	chunks := append(append(append(unbanked, days...), banked...), onces...)
	return chunks, ceilings
}

// accountOf builds the view. all are scopes every request passes through (the
// tightest decides); anyOf are alternatives a request takes one of (a
// provider's credentials: their rooms add up, unless one is unbounded).
func accountOf(all, anyOf []acctScope, now time.Time) Account {
	acc := Account{Chunks: []AccountChunk{}, Ceilings: []AccountCeiling{}, Drain: drainRule}
	avail, bound := math.Inf(1), false
	for _, s := range all {
		if r, ok := s.room(); ok {
			avail, bound = min(avail, r), true
		}
	}
	if len(anyOf) > 0 {
		sum, allBound := 0.0, true
		for _, s := range anyOf {
			r, ok := s.room()
			if !ok {
				allBound = false
				break
			}
			sum += r
		}
		if allBound {
			avail, bound = min(avail, sum), true
		}
	}
	// A chunked scope's chunks keep its order; a key's limits come before
	// every extra.
	var chunked, limits, extras []AccountChunk
	for _, s := range append(append([]acctScope{}, all...), anyOf...) {
		ch, ce := s.view(now)
		acc.Ceilings = append(acc.Ceilings, ce...)
		for _, c := range ch {
			switch {
			case s.chunked:
				chunked = append(chunked, c)
			case c.Kind == "budget" || c.Kind == "value-add":
				extras = append(extras, c)
			default:
				limits = append(limits, c)
			}
		}
	}
	acc.Chunks = append(append(append(acc.Chunks, chunked...), limits...), extras...)
	if bound {
		acc.AvailableUSD = &avail
	}
	// A provider's chunks are spent in turn, so one chunk below zero (a day
	// that went over before the order changed) is not a refusal while a later
	// chunk has money: only the total says so. Key limits all apply at once,
	// so any one of them below zero refuses.
	var over float64
	for _, c := range append(limits[:len(limits):len(limits)], extras...) {
		if c.LeftUSD < 0 {
			over += -c.LeftUSD
		}
	}
	if bound && avail < 0 {
		over = max(over, -avail)
	}
	if over > 0 {
		acc.Overdrawn, acc.OverdrawnUSD = true, &over
	}
	acc.DrainOrder, acc.DrainNote = drainOrder(acc.Chunks)
	return acc
}

// drainRank is a chunk's place under drainRule: non-renewing expiring (1),
// renewing (2), banked expiring (3), banked with no expiry (4), once (5). Two
// expiring chunks of one rank go soonest first.
func drainRank(c AccountChunk) int {
	switch {
	case c.RefillsAt != "":
		return 2
	case c.Kind == "once":
		return 5
	case !c.Banked && c.Expires != "":
		return 1
	case c.Expires != "":
		return 3
	}
	return 4
}

// drainBefore reports whether the rule spends a before b.
func drainBefore(a, b AccountChunk) bool {
	ra, rb := drainRank(a), drainRank(b)
	if ra != rb {
		return ra < rb
	}
	return (ra == 1 || ra == 3) && a.Expires < b.Expires
}

// drainOrder is chunks' names, in the order the ledger spends them, and a note
// naming each pair the ledger spends against the rule; "" when none.
func drainOrder(chunks []AccountChunk) ([]string, string) {
	if len(chunks) == 0 {
		return nil, ""
	}
	var order, notes []string
	for i, c := range chunks {
		order = append(order, c.Name)
		for _, later := range chunks[i+1:] {
			if drainBefore(later, c) {
				notes = append(notes, fmt.Sprintf("%s is spent before %s", c.Name, later.Name))
			}
		}
	}
	note := ""
	if len(notes) > 0 {
		note = "not in the rule's order: " + strings.Join(notes, "; ")
	}
	return order, note
}

// storedLevel is a persisted counter's level at now, as the ledger reads it.
func storedLevel(c store.QuotaCounter, l config.Limit, now time.Time) float64 {
	if c.AtMS == 0 {
		return c.Used
	}
	at := time.UnixMilli(c.AtMS)
	switch cal := l.Calendar(); cal {
	case "total":
		return c.Used
	case "day", "month":
		if at.Before(config.PeriodStart(cal, l.Renews, now)) {
			return 0
		}
		return c.Used
	}
	dur, ok := l.Window()
	elapsed := now.Sub(at)
	if !ok || dur <= 0 || elapsed <= 0 {
		return c.Used
	}
	return max(c.Used-l.Amount()*float64(elapsed)/float64(dur), 0)
}

// usdWindows is a scope's hard dollar limits with their levels. level reads a
// limit's fill now. A paced day's amount is the month's remainder over the days
// left, from the tightest calendar-month dollar limit in the scope (as
// counterState.limitOf computes it). timed reads what timed budgets paid in that
// month's period, which the pacing leaves out of the month's spend.
func usdWindows(scope, shown string, ls config.LimitSet, level, timed func(config.Limit) float64, now time.Time) []acctWindow {
	var all []acctWindow
	for _, l := range ls {
		if l.Dimension() != config.DimUSD {
			continue
		}
		all = append(all, acctWindow{name: shown + " " + l.Per, scope: shown, lim: l, limit: l.Amount(), used: level(l)})
	}
	var out []acctWindow
	for _, w := range all {
		if w.lim.Pace {
			best, found := 0.0, false
			for _, m := range all {
				if m.lim.Calendar() == "month" && !m.lim.Pace {
					a := config.PacedAllowance(m.limit, max(m.used-timed(m.lim), 0), w.used, m.lim.Renews, now)
					if !found || a < best {
						best, found = a, true
					}
				}
			}
			w.limit = best
		}
		if w.lim.Hard {
			out = append(out, w)
		}
	}
	return out
}

// providerAccount is the view over a provider's scopes: its own (prov:) and its
// credentials' (cred:). A request passes the provider's scope and ONE
// credential's, so the provider bounds the total and the credentials add up.
func providerAccount(cfg *config.Config, scopes []string, timed []SpendBudget, counters map[string]store.QuotaCounter, now time.Time) *Account {
	limits := scopeLimits(cfg)
	var all, anyOf []acctScope
	for _, sc := range scopes {
		level := func(l config.Limit) float64 { return storedLevel(counters[sc+"\x00"+l.Label()], l, now) }
		paidTimed := func(m config.Limit) float64 { return storedLevel(counters[sc+"\x00"+quota.TimedPaidLabel], m, now) }
		s := acctScope{windows: usdWindows(sc, sc, limits[sc], level, paidTimed, now)}
		if config.IsProviderScope(sc) {
			for _, b := range timed {
				if config.ProviderScopeKey(b.Provider) != sc {
					continue
				}
				exp, _ := time.Parse(time.RFC3339, b.Expires)
				s.extras = append(s.extras, acctExtra{name: fmt.Sprintf("budget %d", b.ID), kind: "budget", usd: b.USD,
					spent: b.SpentUSD, expiresMS: exp.UnixMilli(), active: b.Active, note: b.Note, pays: true, banked: b.Banked})
			}
			s.chunked = true
			if len(s.windows) > 0 || len(s.extras) > 0 {
				all = append(all, s)
			}
			continue
		}
		anyOf = append(anyOf, s)
	}
	acc := accountOf(all, anyOf, now)
	return &acc
}

// scopeLimits is every provider's and credential's limits by scope.
func scopeLimits(cfg *config.Config) map[string]config.LimitSet {
	out := map[string]config.LimitSet{}
	for _, ext := range cfg.Extensions {
		for pn, pv := range ext.Providers {
			if len(pv.Limits) > 0 {
				out[config.ProviderScopeKey(pn)] = pv.Limits
			}
			for _, c := range pv.CredentialList() {
				if len(c.Limits) > 0 {
					out[c.ScopeKey(pn)] = c.Limits
				}
			}
		}
	}
	return out
}

func counterIndex(st *store.Store) (map[string]store.QuotaCounter, error) {
	rows, err := st.LoadQuotaCounters()
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.QuotaCounter, len(rows))
	for _, r := range rows {
		out[r.Backend+"\x00"+r.Label] = r
	}
	return out, nil
}
