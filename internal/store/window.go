package store

import (
	"database/sql"
	"errors"
)

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

// Unanswered is one served name's tally of requests that got no answer in a
// window, split by WHY — because a person reads those three differently and the
// dashboard used to report only the first.
//
// "Nobody was turned away" counted 429s alone, so a window containing eight
// `503 no backend available` said nobody was turned away while eight callers got
// nothing (Lenny run: the caller scored that sentence 0 — "I think the page told
// me something that isn't actually true of my situation"). A 503 is being turned
// away by any plain reading of the words, and a stream that dies mid-answer is
// the thing he actually came to ask about.
type Unanswered struct {
	Served       string
	ToldToReturn int64 // 429: we asked them to come back, and said when
	Refused      int64 // 5xx: nothing could serve it, and we offered no time
	EndedEarly   int64 // 499: the caller's connection went away mid-answer
}

// UnansweredByModel counts, per served name, the requests in the window that
// ended without an answer.
func (s *Store) UnansweredByModel(w Window) ([]Unanswered, error) {
	where, args := w.ts()
	rows, err := s.db.Query(
		`SELECT served,
		        SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status = 499 THEN 1 ELSE 0 END)
		   FROM activity
		  WHERE `+where+` AND status >= 429 AND served <> ''
		  GROUP BY served
		 HAVING SUM(CASE WHEN status = 429 OR status >= 499 THEN 1 ELSE 0 END) > 0
		  ORDER BY 2 + 3 + 4 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Unanswered
	for rows.Next() {
		var u Unanswered
		if err := rows.Scan(&u.Served, &u.ToldToReturn, &u.Refused, &u.EndedEarly); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SlowSpell is the worst stretch inside a window: how many requests ran far
// past the average, when it started and ended, and the worst single one.
//
// It exists because a true summary hid a true incident. An hour on this box
// reported "Everybody got an answer — nobody was told to come back, nothing was
// refused, and no answer was cut short" with a 3.8 s mean, while eight
// consecutive requests between 09:36 and 09:42 took 34.8 s to 40.0 s. Every
// word of the summary was correct and the box had been unusable for six
// minutes. The 40.0 s was on screen, in the fourth table down, next to the mean
// and with nothing saying which of the two was the news (Lenny run 8, which
// asked for the rule this serves — OB-13).
//
// Deliberately NOT a judgement. It reports what happened — this many, this
// long, between these times — and leaves "is that bad" to the reader, who knows
// what their box is for. A threshold that declared an incident would be wrong
// on somebody's batch queue within a week.
type SlowSpell struct {
	MeanMS      int64 // mean dwell of served requests in the window
	ThresholdMS int64 // what counted as "far past" — the multiple of the mean
	Count       int64 // how many exceeded it
	FirstMS     int64 // when the first of them landed
	LastMS      int64 // when the last did
	WorstMS     int64 // the slowest single request
	WorstAtMS   int64 // and when it was
}

// slowSpellMultiple is how far past the mean a request must run to be part of a
// spell.
//
// Three, because it has to clear ordinary spread rather than mark it. This box
// runs a coefficient of variation near 1.5 — a few long requests already
// dominate the average — so 2x would flag a normal hour and train the reader to
// scroll past the one sentence written to stop them scrolling past it.
const slowSpellMultiple = 3

// SlowSpell finds the slow stretch in a window, if there was one.
//
// Two passes rather than one window function: the mean is over SERVED requests
// only (a 429 is refused in milliseconds and would drag the average down, then
// make the threshold it defines too easy to clear), and SQLite's older builds
// in the field cannot be relied on for window functions.
//
// Count 0 means nothing stood out, which is the normal answer.
func (s *Store) SlowSpell(w Window) (SlowSpell, error) {
	where, args := w.ts()
	var out SlowSpell
	var mean sql.NullFloat64
	err := s.db.QueryRow(
		`SELECT AVG(dwell_ms) FROM activity
		  WHERE `+where+` AND status < 400 AND dwell_ms > 0`, args...).Scan(&mean)
	if err != nil {
		return out, err
	}
	if !mean.Valid || mean.Float64 <= 0 {
		return out, nil
	}
	out.MeanMS = int64(mean.Float64)
	out.ThresholdMS = int64(mean.Float64 * slowSpellMultiple)

	var count sql.NullInt64
	var first, last, worst, worstAt sql.NullInt64
	err = s.db.QueryRow(
		`SELECT COUNT(*), MIN(ts), MAX(ts), MAX(dwell_ms) FROM activity
		  WHERE `+where+` AND status < 400 AND dwell_ms > ?`,
		append(append([]any{}, args...), out.ThresholdMS)...).
		Scan(&count, &first, &last, &worst)
	if err != nil {
		return out, err
	}
	if !count.Valid || count.Int64 == 0 {
		return out, nil
	}
	out.Count, out.FirstMS, out.LastMS, out.WorstMS = count.Int64, first.Int64, last.Int64, worst.Int64

	// When the worst one was, which is the part a person can act on: it turns
	// "something was slow this hour" into a time they can go and look at.
	err = s.db.QueryRow(
		`SELECT ts FROM activity
		  WHERE `+where+` AND status < 400 AND dwell_ms = ?
		  ORDER BY ts LIMIT 1`,
		append(append([]any{}, args...), out.WorstMS)...).Scan(&worstAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.WorstAtMS = worstAt.Int64
	return out, nil
}

// Preemptions counts the requests ever stopped mid-answer to free a slot, and
// when the last one was.
//
// It exists to put a number beside a frightening word. `interruptible: yes` on
// a caller's own group says a request of theirs CAN be stopped mid-answer, and
// the screen said nothing about whether that had ever happened — so the reader
// could neither rule it in nor out. Lenny run 9 found his team's key in the one
// interruptible group after a colleague said the assistant "gave up on him",
// and scored the page 0 on "what is being asked of me": not because the fact
// was wrong, but because a capability with no history reads as a cause.
//
// On this box the honest answer is zero, all-time, which is a far better thing
// for the page to say than a definition of the word.
//
// Counted from the error reason rather than the status, because 499 covers both
// a preemption and a caller that simply hung up, and those are opposite stories:
// one is the box taking the slot back, the other is the caller leaving.
func (s *Store) Preemptions() (count int64, lastMS int64, err error) {
	var last sql.NullInt64
	err = s.db.QueryRow(
		`SELECT COUNT(*), MAX(ts) FROM activity WHERE error = 'preempted'`).Scan(&count, &last)
	if err != nil {
		return 0, 0, err
	}
	return count, last.Int64, nil
}
