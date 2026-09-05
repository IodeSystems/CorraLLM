package store

// Window is the slice of time a read of the activity log covers.
//
// It replaces a bare `sinceMS int64` on every windowed query, and the reason is
// a UX finding rather than a database one (plan/p30-information-architecture.md
// §0). Every panel on the dashboard carried its OWN "since N ago" constant — 60
// minutes for utilization, 24 hours for the rollups, 6h/24h/7d for the key
// charts, the newest 100 rows for the log — so the product could answer "the
// last hour" and "the last day" and could not answer "this morning", which is
// the question a person actually arrives with when something went wrong at 09:00.
//
// A relative window cannot express that. An absolute one can express both, so
// this is the type every panel now shares.
//
// Zero values mean "unbounded on that side": Window{} reads the whole log.
type Window struct {
	FromMS int64 // inclusive lower bound; 0 = from the start of the log
	ToMS   int64 // exclusive upper bound; 0 = up to now
}

// Since is the old behaviour, named: everything from a moment until now.
func Since(fromMS int64) Window { return Window{FromMS: fromMS} }

// Between is the new one: a fixed span that does not move when the clock does.
// Two runs of the same query over the same Between return the same rows, which
// is what makes a number quotable — "it was 43% at 09:00" stops being true the
// moment the window slides.
func Between(fromMS, toMS int64) Window { return Window{FromMS: fromMS, ToMS: toMS} }

// where renders the window against the timestamp column, as a SQL fragment plus
// its arguments. It always returns a usable predicate — `1=1` for the unbounded
// case — so callers can concatenate without branching, and cannot accidentally
// emit `WHERE  AND key = ?`.
//
// The upper bound is EXCLUSIVE so that adjacent windows tile: [09:00, 10:00) and
// [10:00, 11:00) share no row, and summing them equals the two-hour window. An
// inclusive bound double-counts every request that landed exactly on the seam,
// which is rare, real, and impossible to spot in a chart.
func (w Window) where(col string) (string, []any) {
	switch {
	case w.FromMS > 0 && w.ToMS > 0:
		return col + " >= ? AND " + col + " < ?", []any{w.FromMS, w.ToMS}
	case w.FromMS > 0:
		return col + " >= ?", []any{w.FromMS}
	case w.ToMS > 0:
		return col + " < ?", []any{w.ToMS}
	default:
		return "1=1", nil
	}
}

// ts is the common case: the activity log's own timestamp column.
func (w Window) ts() (string, []any) { return w.where("ts") }
