package store

import "strings"

// Slice narrows a rollup to some callers and some models, and says which of
// the two its rows are split by (P56). Keys and Models empty mean every caller
// and every model; ByKey and ByModel both false mean one row for the lot.
//
// The traffic question is rarely "per model" or "per caller" alone. One agent
// on two models, or two callers sharing one model, read differently from
// either axis on its own, so the rollups take both.
type Slice struct {
	Keys    []string
	Models  []string
	ByKey   bool
	ByModel bool
}

// PerModel is the split the rollups had before P56: one row per served model,
// every caller in it.
var PerModel = Slice{ByModel: true}

// Group is the identity of one row under a Slice. A dimension the slice does
// not split by is "".
type Group struct {
	Key    string
	Served string
}

// group projects a request's (key, served) onto the slice's split.
func (s Slice) group(key, served string) Group {
	var g Group
	if s.ByKey {
		g.Key = key
	}
	if s.ByModel {
		g.Served = served
	}
	return g
}

// where renders the filters as SQL appended after a WHERE clause: "" when
// nothing is filtered, else " AND <keyCol> IN (…) AND <servedCol> IN (…)".
func (s Slice) where(keyCol, servedCol string) (string, []any) {
	var b strings.Builder
	var args []any
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		b.WriteString(" AND " + col + " IN (?" + strings.Repeat(",?", len(vals)-1) + ")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in(keyCol, s.Keys)
	in(servedCol, s.Models)
	return b.String(), args
}

// Has reports whether the slice keeps a request from key on served.
func (s Slice) Has(key, served string) bool {
	return contains(s.Keys, key) && contains(s.Models, served)
}

// contains is true for an empty list: no filter keeps everything.
func contains(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (g Group) less(o Group) bool {
	if g.Key != o.Key {
		return g.Key < o.Key
	}
	return g.Served < o.Served
}
