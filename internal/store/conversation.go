package store

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/iodesystems/corrallm/internal/slotcache"
)

// conversationSchema is one row per chat turn: which conversation it belongs
// to, how far along it was, and how full the context was.
//
// A SIDE TABLE, not columns on activity, for two measured reasons. An activity
// row carries its captured request (~120 KB), and SQLite rewrites a whole row
// on UPDATE — backfilling a column there is a rewrite of every payload, about
// 18 GB on box1. And activity's columns past req_body are only reachable
// through its overflow pages, so even an index over new columns costs a read
// of the whole table to build. Here a row is ~60 bytes and the index covers
// every query.
//
// ctx_tokens is prompt + completion: what the context held when the turn
// ended. Each turn re-sends the whole conversation, so one turn's prompt IS
// the conversation's size at that point — never a sum across turns.
const conversationSchema = `CREATE TABLE IF NOT EXISTS activity_conversation (
  activity_id  INTEGER PRIMARY KEY, -- activity.id
  ts           INTEGER NOT NULL,    -- activity.ts, for the window and pruning
  served       TEXT    NOT NULL,
  conversation TEXT    NOT NULL,    -- slotcache.Key
  messages     INTEGER NOT NULL,    -- messages the request carried
  ctx_tokens   INTEGER NOT NULL     -- prompt + completion tokens; 0 when nothing was served
)`

func (s *Store) insertConversation(activityID, ts int64, served, conv string, messages int, ctx int64) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO activity_conversation (activity_id, ts, served, conversation, messages, ctx_tokens)
		 VALUES (?, ?, ?, ?, ?, ?)`, activityID, ts, served, conv, messages, ctx)
	return err
}

// BackfillConversations derives activity_conversation rows for activity
// recorded before the table existed, from the captured request bodies.
//
// Only as far back as payloads are kept (PrunePayloads): a row whose body was
// cleared names no conversation, and the walk stops at the first batch where
// every body is gone. It walks newest-first from the oldest row already in
// the table, so a restart resumes rather than repeats, and live inserts (which
// are newer) never get in its way.
//
// Paced in small batches because the database has ONE connection: each batch
// reads ~50 captured payloads and holds that connection while it does, and
// every activity insert on the response path waits behind it.
//
// Rows written before this existed carry no declared conversation header, so
// they are keyed by the conversation's head alone — the same thing a caller
// that sends no header gets today.
func (s *Store) BackfillConversations(ctx context.Context) {
	const batch = 50
	var cursor int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(activity_id), (SELECT COALESCE(MAX(id), 0) + 1 FROM activity))
		 FROM activity_conversation`).Scan(&cursor); err != nil {
		slog.Warn("conversation backfill", "err", err)
		return
	}
	start, done := time.Now(), 0
	for {
		n, next, empty, err := s.backfillBatch(ctx, cursor, batch)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("conversation backfill", "err", err)
			}
			return
		}
		done += n
		if empty {
			if done > 0 {
				slog.Info("conversation backfill done", "rows", done, "took", time.Since(start).Round(time.Second))
			}
			return
		}
		cursor = next
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// backfillBatch handles the batch of chat rows below cursor. empty reports
// that there is nothing left worth reading: no rows, or none with a body.
func (s *Store) backfillBatch(ctx context.Context, cursor int64, limit int) (inserted int, next int64, empty bool, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, served, key,
		        COALESCE((SELECT req_body FROM activity_payload WHERE activity_id = id), ''),
		        prompt_tokens + completion_tokens
		 FROM activity WHERE id < ? AND path = '/v1/chat/completions'
		 ORDER BY id DESC LIMIT ?`, cursor, limit)
	if err != nil {
		return 0, 0, false, err
	}
	type turn struct {
		id, ts, ctx  int64
		served, conv string
		messages     int
	}
	var turns []turn
	seen, bodies := 0, 0
	next = cursor
	for rows.Next() {
		var t turn
		var key, body string
		if err := rows.Scan(&t.id, &t.ts, &t.served, &key, &body, &t.ctx); err != nil {
			_ = rows.Close()
			return 0, 0, false, err
		}
		seen++
		next = t.id
		if body == "" {
			continue
		}
		bodies++
		conv, ok := slotcache.Key(key, "", []byte(body))
		if !ok {
			continue // truncated at the capture cap, or not a conversation
		}
		t.conv, t.messages = conv, slotcache.Messages([]byte(body))
		turns = append(turns, t)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, false, err
	}
	if seen == 0 || bodies == 0 {
		return 0, next, true, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range turns {
		// OR IGNORE: a live insert already wrote this row, and its key — which
		// saw the caller's declared header — is the better one.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO activity_conversation (activity_id, ts, served, conversation, messages, ctx_tokens)
			 VALUES (?, ?, ?, ?, ?, ?)`, t.id, t.ts, t.served, t.conv, t.messages, t.ctx); err != nil {
			return 0, 0, false, err
		}
	}
	return len(turns), next, false, tx.Commit()
}

// CtxStat is how full the context was, for one served model (or, with Served
// empty, all of them), in two readings.
//
// PER REQUEST counts every turn: "when a request arrives, how much context
// does it carry". A long conversation weighs in once per turn, which is the
// right weighting for sizing a model's context window — most requests ARE the
// late turns of long conversations.
//
// PER SESSION TAIL counts each conversation once, at its last turn: "how big
// does a conversation get by the end". Each turn overwrites the one before it,
// so a 40-turn conversation is one number here, not 40.
type CtxStat struct {
	Group
	Requests   int64
	RequestAvg float64
	RequestP90 int64
	RequestP99 int64
	Sessions   int64
	TailAvg    float64
	TailP90    int64
	TailP99    int64

	// WHAT THE CONTEXT IS MADE OF, per request (means over the same turns as
	// RequestAvg). Prompt is what the caller sent, Cached the part of it the
	// backend already held. Thinking and Answer split the completion, and are
	// over ThinkingN turns only — the ones whose thinking was measured — so
	// Prompt + Thinking + Answer equals RequestAvg only when ThinkingN equals
	// Requests.
	PromptAvg   float64
	CachedAvg   float64
	ThinkingN   int64
	ThinkingAvg float64
	AnswerAvg   float64

	// HOW LONG CONVERSATIONS RUN: served turns per session (same sessions as
	// Sessions). SingleTurn counts sessions that never came back after one
	// turn; the rest are repeat customers.
	TurnsAvg   float64
	TurnsP90   int64
	TurnsP99   int64
	TurnsMax   int64
	SingleTurn int64
}

func fillTurns(st *CtxStat, t []int64) {
	if len(t) == 0 {
		return
	}
	st.TurnsAvg, st.TurnsP90, st.TurnsP99 = avgTails(t) // sorts t
	st.TurnsMax = t[len(t)-1]
	for _, n := range t {
		if n == 1 {
			st.SingleTurn++
		}
	}
}

// CtxStats reads per-request and per-session-tail context size over a window,
// narrowed and split by sl, plus a total across the rows (Group zero).
//
// A session is one run of a conversation: the same conversation key with a
// message count that never goes down. A count that drops starts a new session
// — the same task restarted, or the caller compacting its history; both leave
// the context smaller than it was, and neither can be told from the other
// here. A session still running at the end of the window is counted at its
// latest turn. Turns nobody served (ctx_tokens 0: a 429, a 503, a hang-up)
// are left out of both readings.
func (s *Store) CtxStats(w Window, sl Slice) (rows []CtxStat, total CtxStat, err error) {
	wWhere, wArgs := w.ts()
	// The caller lives on activity, so a key filter needs the join to match;
	// without one the LEFT JOIN keeps turns whose activity row is gone.
	sWhere, sArgs := sl.where("COALESCE(a.key, '')", "c.served")
	// The window filter runs inside the subquery: both tables have a ts column.
	q, err := s.db.Query(
		`SELECT COALESCE(a.key, ''), c.served, c.conversation, c.messages, c.ctx_tokens,
		        COALESCE(a.prompt_tokens, 0), COALESCE(a.cached_tokens, 0),
		        COALESCE(a.completion_tokens, 0), COALESCE(a.reasoning_tokens, -1)
		 FROM (SELECT activity_id, ts, served, conversation, messages, ctx_tokens
		       FROM activity_conversation WHERE `+wWhere+` AND ctx_tokens > 0) c
		 LEFT JOIN activity a ON a.id = c.activity_id
		 WHERE 1=1`+sWhere+`
		 ORDER BY c.ts`, append(wArgs, sArgs...)...)
	if err != nil {
		return nil, CtxStat{}, err
	}
	defer func() { _ = q.Close() }()

	type open struct {
		group    Group
		messages int
		ctx      int64
		turns    int64
	}
	reqs := map[Group][]int64{}
	tails := map[Group][]int64{}
	turns := map[Group][]int64{}
	parts := map[Group]*ctxParts{}
	var allParts ctxParts
	sessions := map[string]*open{}
	for q.Next() {
		var key, served, conv string
		var messages int
		var ctx, prompt, cached, completion, thinking int64
		if err := q.Scan(&key, &served, &conv, &messages, &ctx, &prompt, &cached, &completion, &thinking); err != nil {
			return nil, CtxStat{}, err
		}
		g := sl.group(key, served)
		reqs[g] = append(reqs[g], ctx)
		pp := parts[g]
		if pp == nil {
			pp = &ctxParts{}
			parts[g] = pp
		}
		pp.add(prompt, cached, completion, thinking)
		allParts.add(prompt, cached, completion, thinking)
		n := int64(1)
		if o, ok := sessions[conv]; ok {
			if messages < o.messages {
				tails[o.group] = append(tails[o.group], o.ctx)
				turns[o.group] = append(turns[o.group], o.turns)
			} else {
				n = o.turns + 1
			}
		}
		sessions[conv] = &open{g, messages, ctx, n}
	}
	if err := q.Err(); err != nil {
		return nil, CtxStat{}, err
	}
	for _, o := range sessions {
		tails[o.group] = append(tails[o.group], o.ctx)
		turns[o.group] = append(turns[o.group], o.turns)
	}

	var allReqs, allTails, allTurns []int64
	for g, r := range reqs {
		st := ctxStat(g, r, tails[g])
		parts[g].fill(&st)
		fillTurns(&st, turns[g])
		rows = append(rows, st)
		allReqs = append(allReqs, r...)
		allTails = append(allTails, tails[g]...)
		allTurns = append(allTurns, turns[g]...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group.less(rows[j].Group) })
	tot := ctxStat(Group{}, allReqs, allTails)
	fillTurns(&tot, allTurns)
	allParts.fill(&tot)
	return rows, tot, nil
}

// ctxParts sums what each request's context is made of.
type ctxParts struct {
	n, prompt, cached         int64
	thinkN, thinking, answers int64
}

func (p *ctxParts) add(prompt, cached, completion, thinking int64) {
	p.n++
	p.prompt += prompt
	p.cached += cached
	if thinking >= 0 { // -1 = not measured
		p.thinkN++
		p.thinking += thinking
		p.answers += max(completion-thinking, 0)
	}
}

func (p *ctxParts) fill(st *CtxStat) {
	if p.n > 0 {
		st.PromptAvg = float64(p.prompt) / float64(p.n)
		st.CachedAvg = float64(p.cached) / float64(p.n)
	}
	st.ThinkingN = p.thinkN
	if p.thinkN > 0 {
		st.ThinkingAvg = float64(p.thinking) / float64(p.thinkN)
		st.AnswerAvg = float64(p.answers) / float64(p.thinkN)
	}
}

func ctxStat(g Group, reqs, tails []int64) CtxStat {
	st := CtxStat{Group: g, Requests: int64(len(reqs)), Sessions: int64(len(tails))}
	st.RequestAvg, st.RequestP90, st.RequestP99 = avgTails(reqs)
	st.TailAvg, st.TailP90, st.TailP99 = avgTails(tails)
	return st
}

// avgTails is the mean and the nearest-rank 90th and 99th percentiles. Sorts
// in place.
func avgTails(v []int64) (avg float64, p90, p99 int64) {
	if len(v) == 0 {
		return 0, 0, 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	var sum int64
	for _, x := range v {
		sum += x
	}
	rank := func(p float64) int64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
	return float64(sum) / float64(len(v)), rank(0.9), rank(0.99)
}
