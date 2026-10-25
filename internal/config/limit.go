package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Limit is ONE declared budget: an amount in one dimension, over one window.
//
//	limits:
//	  - {req: 20,     per: minute}
//	  - {req: 1000,   per: day}
//	  - {usd: "5.00", per: hour}
//	  - {usd: 200,    per: month}
//	  - {sec: 600,    per: minute}
//	  - {usd: 300,    per: month, hard: true}
//	  - {usd: 50,     per: calendar-month, hard: true}
//
// A flat list rather than a dimension→spec map, because the same dimension
// routinely needs several windows: a provider's "20/minute AND 1000/day" is two
// limits on one dimension, which a map keyed by dimension cannot express. That
// limitation is why rate limits (rpm/rpd) and spend limits (cost) grew separate
// schemas; one list subsumes both.
//
// Exactly ONE of Req/USD/Sec must be set — the field names the dimension, so an
// entry with none has no meaning and an entry with two has no single one.
// Validate enforces it.
type Limit struct {
	Req float64 `yaml:"req,omitempty" json:"req,omitempty"` // requests
	USD float64 `yaml:"usd,omitempty" json:"usd,omitempty"` // dollars spent
	Sec float64 `yaml:"sec,omitempty" json:"sec,omitempty"` // seconds of backend dwell
	Per string  `yaml:"per,omitempty" json:"per,omitempty"` // window: minute|hour|day|month|…

	// Hard makes an exhausted limit REFUSE rather than steer. A soft limit only
	// takes its backend out of the walk while something else can serve; when
	// every candidate is exhausted the request is served anyway, which is right
	// for a free tier and wrong for a paid key, where "served anyway" is the
	// spend the limit was written to stop. A hard limit's backend never serves
	// while the limit is full, and a caller with nothing else gets a 429.
	Hard bool `yaml:"hard,omitempty" json:"hard,omitempty"`

	// Model narrows a CALLER allocation to the requests one model served: "this
	// key may spend $3/day on deepseek". Key limits only — a model's or a
	// provider's budget is already about one thing, so Validate refuses it
	// anywhere else. The ledger keeps each model's share in its own scope
	// (KeyModelScopeKey), so the key-wide windows and every model's are counted
	// independently.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// limit dimensions, as used for counter labels and error messages.
const (
	DimRequests = "req"
	DimUSD      = "usd"
	DimDwell    = "sec"
)

// Dimension names which budget this limit constrains, or "" when the entry does
// not name exactly one.
func (l Limit) Dimension() string {
	n, dim := 0, ""
	for _, c := range []struct {
		v float64
		d string
	}{{l.Req, DimRequests}, {l.USD, DimUSD}, {l.Sec, DimDwell}} {
		if c.v != 0 {
			n, dim = n+1, c.d
		}
	}
	if n != 1 {
		return ""
	}
	return dim
}

// Amount is the budget in this limit's dimension.
func (l Limit) Amount() float64 {
	switch l.Dimension() {
	case DimRequests:
		return l.Req
	case DimUSD:
		return l.USD
	case DimDwell:
		return l.Sec
	}
	return 0
}

// Window is the period the amount is allowed over.
//
// "month" is 30 DAYS SLIDING, not a calendar month. The counters underneath are
// falloff buckets with no reset boundary, so there is no 1st-of-the-month cliff
// and no thundering burst after one — a budget that drains continuously is the
// better shape for a spend guard, and it is the only one this engine can honour
// honestly.
//
// "calendar-day" and "calendar-month" are the billing-cycle form: a fixed
// window that empties at 00:00 UTC / on the 1st, the way a provider bills. The
// duration returned for them is nominal (display only); see Calendar.
//
// "total" is a one-off budget: a fixed window that never empties ("$15 for
// the whole bench run"). Spent is spent; raising the amount is the only refill.
func (l Limit) Window() (time.Duration, bool) {
	per := strings.ToLower(strings.TrimSpace(l.Per))
	if d, ok := calendarUnits[per]; ok {
		return d, true
	}
	d, ok := windowUnits[per]
	return d, ok
}

// calendarUnits are the fixed-reset windows, with a nominal duration.
var calendarUnits = map[string]time.Duration{
	"calendar-day":   24 * time.Hour,
	"calendar-month": 30 * 24 * time.Hour,
	"total":          100 * 365 * 24 * time.Hour,
}

// Calendar is "day" or "month" for a fixed-reset window, "total" for one that
// never resets, "" for a falloff one.
func (l Limit) Calendar() string {
	per := strings.ToLower(strings.TrimSpace(l.Per))
	if _, ok := calendarUnits[per]; ok {
		return strings.TrimPrefix(per, "calendar-")
	}
	return ""
}

// Label is the counter key for this limit: dimension + window, so several
// windows on one dimension keep independent state.
func (l Limit) Label() string {
	return l.Dimension() + "/" + strings.ToLower(strings.TrimSpace(l.Per))
}

// Validate reports why this limit is unusable, or nil.
func (l Limit) Validate() error {
	if l.Model != "" {
		return fmt.Errorf("limit names model %q: only a caller key's allocation can be narrowed to a model", l.Model)
	}
	return l.validateAmount()
}

func (l Limit) validateAmount() error {
	dim := l.Dimension()
	if dim == "" {
		return fmt.Errorf("limit must set exactly one of req/usd/sec (got %+v)", l)
	}
	if l.Amount() <= 0 {
		return fmt.Errorf("limit %s: amount must be > 0", dim)
	}
	if strings.TrimSpace(l.Per) == "" {
		return fmt.Errorf("limit %s: missing `per`", dim)
	}
	if _, ok := l.Window(); !ok {
		return fmt.Errorf("limit %s: unknown window %q", dim, l.Per)
	}
	return nil
}

// LimitSet is a declared budget list that also accepts the two shapes that
// predate it, so an existing config keeps working unchanged:
//
//	limits: [{req: 20, per: minute}]        # the list form
//	limits: {rpm: 20, rpd: 1000}            # freeTier's old rate limits
//	limits: {cost: "$5/hr", dwell: "600s/min"}  # the old group/stage specs
//
// Both legacy forms are mappings and the new one is a sequence, so the node
// kind alone disambiguates — no ambiguity to resolve by guessing.
type LimitSet []Limit

// UnmarshalYAML accepts the list form or either legacy mapping.
func (ls *LimitSet) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var out []Limit
		if err := n.Decode(&out); err != nil {
			return err
		}
		*ls = out
		return nil
	case yaml.MappingNode:
		var raw map[string]string
		if err := n.Decode(&raw); err != nil {
			return fmt.Errorf("limits: %w", err)
		}
		out, err := legacyLimits(raw)
		if err != nil {
			return err
		}
		*ls = out
		return nil
	case 0: // empty / null
		*ls = nil
		return nil
	}
	return fmt.Errorf("limits: want a list of {req|usd|sec, per} or a legacy mapping")
}

// legacyLimits translates the pre-list shapes into Limits.
func legacyLimits(raw map[string]string) (LimitSet, error) {
	var out LimitSet
	for k, v := range raw {
		key := strings.ToLower(strings.TrimSpace(k))
		switch key {
		case "rpm", "rpd":
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return nil, fmt.Errorf("limits %s: %w", key, err)
			}
			per := "minute"
			if key == "rpd" {
				per = "day"
			}
			out = append(out, Limit{Req: n, Per: per})
		case "cost", "dwell", "requests":
			// "<amount>/<window>" — the ParseRate mini-language.
			r, err := ParseRate(key, v)
			if err != nil {
				return nil, err
			}
			l := Limit{Per: windowName(r.Window)}
			switch key {
			case "cost":
				l.USD = r.Amount
			case "dwell":
				l.Sec = r.Amount
			case "requests":
				l.Req = r.Amount
			}
			if l.Per == "" {
				return nil, fmt.Errorf("limits %s: window %s has no name", key, r.Window)
			}
			out = append(out, l)
		default:
			return nil, fmt.Errorf("limits: unknown legacy key %q", k)
		}
	}
	sortLimits(out)
	return out, nil
}

// windowName is windowUnits inverted, for rendering a parsed duration back to
// the canonical spelling.
func windowName(d time.Duration) string {
	for _, n := range []string{"second", "minute", "hour", "day", "month"} {
		if windowUnits[n] == d {
			return n
		}
	}
	return ""
}

// sortLimits gives a deterministic order (label) so a rewritten config and a
// counter's iteration order are stable.
func sortLimits(ls LimitSet) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j].Label() < ls[j-1].Label(); j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// ValidateKey checks a caller allocation: Validate's rules, plus `model` is
// allowed, `sec` is not (nothing charges dwell to a caller, so it would never
// fill), and no two entries share a model and a window (they would be one
// counter).
func (ls LimitSet) ValidateKey() error {
	seen := map[string]bool{}
	for _, l := range ls {
		if err := l.validateAmount(); err != nil {
			return err
		}
		if l.Dimension() == DimDwell {
			return fmt.Errorf("a caller allocation counts req or usd, not sec")
		}
		id := l.Model + "\x00" + l.Label()
		if seen[id] {
			if l.Model != "" {
				return fmt.Errorf("two %s limits on model %q", l.Label(), l.Model)
			}
			return fmt.Errorf("two %s limits", l.Label())
		}
		seen[id] = true
	}
	return nil
}

// ByModel splits a caller allocation into its key-wide limits and each
// model's share.
func (ls LimitSet) ByModel() (all LimitSet, byModel map[string]LimitSet) {
	for _, l := range ls {
		if l.Model == "" {
			all = append(all, l)
			continue
		}
		if byModel == nil {
			byModel = map[string]LimitSet{}
		}
		byModel[l.Model] = append(byModel[l.Model], l)
	}
	return all, byModel
}

// Validate reports the first unusable entry.
func (ls LimitSet) Validate(where string) error {
	for _, l := range ls {
		if err := l.Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// ForDimension returns just the limits constraining dim.
func (ls LimitSet) ForDimension(dim string) LimitSet {
	var out LimitSet
	for _, l := range ls {
		if l.Dimension() == dim {
			out = append(out, l)
		}
	}
	return out
}
