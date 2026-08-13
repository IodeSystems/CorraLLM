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
// honestly. A caller who needs billing-cycle semantics needs a different
// mechanism, not a different string here.
func (l Limit) Window() (time.Duration, bool) {
	d, ok := windowUnits[strings.ToLower(strings.TrimSpace(l.Per))]
	return d, ok
}

// Label is the counter key for this limit: dimension + window, so several
// windows on one dimension keep independent state.
func (l Limit) Label() string {
	return l.Dimension() + "/" + strings.ToLower(strings.TrimSpace(l.Per))
}

// Validate reports why this limit is unusable, or nil.
func (l Limit) Validate() error {
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
