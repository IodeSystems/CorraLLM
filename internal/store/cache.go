package store

// Prompt-cache attribution (P75): why a chat turn did not find its prefix in
// the backend's KV cache.
//
// activity_cache is one row per served chat turn that has a conversation key,
// on a local backend. A SIDE TABLE for the same reason activity_conversation
// is one (conversation.go): activity rows carry their payload pages, a new
// column there is a rewrite of a 9 GB table, and an index over it costs a read
// of the whole thing. A row here is ~100 bytes and pruned with
// activity_conversation (PruneActivity).
//
// Token figures are the backend's; match_chars and the estimates derived from
// it are corrallm's, from comparing this turn's messages with the previous
// turn's in 1 KiB blocks (internal/slotcache.Inspect). -1 means "not known":
// no hashes (the previous turn was seen before a restart, or had no body),
// no id_slot in the reply, no /metrics on the backend.
const cacheSchema = `CREATE TABLE IF NOT EXISTS activity_cache (
  activity_id       INTEGER PRIMARY KEY, -- activity.id
  ts                INTEGER NOT NULL,
  served            TEXT    NOT NULL,
  conversation      TEXT    NOT NULL,
  instance          TEXT    NOT NULL DEFAULT '', -- the backend process (handle id)
  slot              INTEGER NOT NULL DEFAULT -1, -- id_slot; -1 when the reply has none
  prev_activity_id  INTEGER NOT NULL DEFAULT 0,  -- the previous turn; 0 = none
  gap_ms            INTEGER NOT NULL DEFAULT -1, -- since the previous turn
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  prev_ctx_tokens   INTEGER NOT NULL DEFAULT -1, -- previous turn's prompt + completion
  prompt_chars      INTEGER NOT NULL DEFAULT 0,
  match_chars       INTEGER NOT NULL DEFAULT -1, -- leading chars equal to the previous turn
  match_tok_est     INTEGER NOT NULL DEFAULT -1, -- match_chars x prompt_tokens / prompt_chars
  diverge_msg       INTEGER NOT NULL DEFAULT -1, -- message where the match ended; -2 = tools
  lost_tok          INTEGER NOT NULL DEFAULT 0,  -- reusable tokens that were re-prefilled
  other_tok         INTEGER NOT NULL DEFAULT -1, -- tokens the instance served to others between turns
  evictions_between INTEGER NOT NULL DEFAULT -1, -- prompt-cache evictions between turns (0005 /metrics)
  ckpt_dist_tok     INTEGER NOT NULL DEFAULT -1, -- cached tokens' distance to the nearest earlier boundary
  prompt_per_sec    REAL    NOT NULL DEFAULT 0,  -- for re-prefill seconds
  class             TEXT    NOT NULL,
  cause             TEXT    NOT NULL DEFAULT ''
)`

// backendEventSchema is one row per backend process start, adoption or stop,
// with why. model_load keeps its job (who asked, what was evicted, how long
// it took); this is the lifecycle the cache classifier needs: a reload empties
// the cache, and model_load has no row for a crash, an adoption or a stop.
const backendEventSchema = `CREATE TABLE IF NOT EXISTS backend_event (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       INTEGER NOT NULL,
  model    TEXT    NOT NULL,
  server   TEXT    NOT NULL DEFAULT '',
  instance TEXT    NOT NULL DEFAULT '',
  kind     TEXT    NOT NULL,            -- start | adopt | stop
  reason   TEXT    NOT NULL DEFAULT '',
  ok       INTEGER NOT NULL DEFAULT 1,
  err      TEXT    NOT NULL DEFAULT ''
)`

// CacheRow is one activity_cache row.
type CacheRow struct {
	ActivityID       int64   `json:"activityId"`
	TS               int64   `json:"ts"`
	Served           string  `json:"served"`
	Conversation     string  `json:"conversation"`
	Instance         string  `json:"instance"`
	Slot             int     `json:"slot"`
	PrevActivityID   int64   `json:"prevActivityId"`
	GapMS            int64   `json:"gapMs"`
	PromptTokens     int     `json:"promptTokens"`
	CachedTokens     int     `json:"cachedTokens"`
	PrevCtxTokens    int     `json:"prevCtxTokens"`
	PromptChars      int     `json:"promptChars"`
	MatchChars       int     `json:"matchChars"`
	MatchTokEst      int     `json:"matchTokEst"`
	DivergeMsg       int     `json:"divergeMsg"`
	LostTok          int     `json:"lostTok"`
	OtherTok         int64   `json:"otherTok"`
	EvictionsBetween int     `json:"evictionsBetween"`
	CkptDistTok      int     `json:"ckptDistTok"`
	PromptPerSec     float64 `json:"promptPerSec"`
	Class            string  `json:"class"`
	Cause            string  `json:"cause"`
	// Caller is activity.key, read by join for the per-caller report. Not a
	// column here: keys are long and already stored once.
	Caller string `json:"-"`
}

// BackendEvent is one backend_event row.
type BackendEvent struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Model    string `json:"model"`
	Server   string `json:"server"`
	Instance string `json:"instance"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	OK       bool   `json:"ok"`
	Err      string `json:"err"`
}

// insertCache writes the cache row of activity id.
func (s *Store) insertCache(id, ts int64, c *CacheRow) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO activity_cache (activity_id, ts, served, conversation, instance, slot, prev_activity_id, gap_ms, prompt_tokens, cached_tokens, prev_ctx_tokens, prompt_chars, match_chars, match_tok_est, diverge_msg, lost_tok, other_tok, evictions_between, ckpt_dist_tok, prompt_per_sec, class, cause)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, ts, c.Served, c.Conversation, c.Instance, c.Slot, c.PrevActivityID, c.GapMS,
		c.PromptTokens, c.CachedTokens, c.PrevCtxTokens, c.PromptChars, c.MatchChars, c.MatchTokEst,
		c.DivergeMsg, c.LostTok, c.OtherTok, c.EvictionsBetween, c.CkptDistTok, c.PromptPerSec, c.Class, c.Cause)
	return err
}

// InsertBackendEvent records one lifecycle event. Never fatal to what it
// describes.
func (s *Store) InsertBackendEvent(e BackendEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO backend_event (ts, model, server, instance, kind, reason, ok, err)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, e.Model, e.Server, e.Instance, e.Kind, e.Reason, boolToInt(e.OK), e.Err)
	return err
}

// BackendEvents returns the events in [fromMS, toMS), oldest first. An empty
// model reads every model's.
func (s *Store) BackendEvents(model string, fromMS, toMS int64) ([]BackendEvent, error) {
	q := `SELECT id, ts, model, server, instance, kind, reason, ok, err FROM backend_event
	      WHERE ts >= ? AND ts < ?`
	args := []any{fromMS, toMS}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	rows, err := s.db.Query(q+` ORDER BY ts, id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BackendEvent
	for rows.Next() {
		var e BackendEvent
		var ok int
		if err := rows.Scan(&e.ID, &e.TS, &e.Model, &e.Server, &e.Instance, &e.Kind, &e.Reason, &ok, &e.Err); err != nil {
			return nil, err
		}
		e.OK = ok != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// CacheRows returns the attribution rows in [fromMS, toMS), oldest first. An
// empty served reads every model's.
//
// The caller comes from activity by primary key: one lookup per row, never a
// scan of activity.
func (s *Store) CacheRows(served string, fromMS, toMS int64) ([]CacheRow, error) {
	q := `SELECT c.activity_id, c.ts, c.served, c.conversation, c.instance, c.slot, c.prev_activity_id, c.gap_ms, c.prompt_tokens, c.cached_tokens, c.prev_ctx_tokens, c.prompt_chars, c.match_chars, c.match_tok_est, c.diverge_msg, c.lost_tok, c.other_tok, c.evictions_between, c.ckpt_dist_tok, c.prompt_per_sec, c.class, c.cause, COALESCE(a.key, '')
	      FROM activity_cache c LEFT JOIN activity a ON a.id = c.activity_id
	      WHERE c.ts >= ? AND c.ts < ?`
	args := []any{fromMS, toMS}
	if served != "" {
		q += ` AND c.served = ?`
		args = append(args, served)
	}
	rows, err := s.db.Query(q+` ORDER BY c.ts, c.activity_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CacheRow
	for rows.Next() {
		var c CacheRow
		if err := rows.Scan(&c.ActivityID, &c.TS, &c.Served, &c.Conversation, &c.Instance, &c.Slot,
			&c.PrevActivityID, &c.GapMS, &c.PromptTokens, &c.CachedTokens, &c.PrevCtxTokens,
			&c.PromptChars, &c.MatchChars, &c.MatchTokEst, &c.DivergeMsg, &c.LostTok, &c.OtherTok,
			&c.EvictionsBetween, &c.CkptDistTok, &c.PromptPerSec, &c.Class, &c.Cause, &c.Caller); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastTurn is the newest turn of one conversation, for seeding the cache
// tracker at startup.
type LastTurn struct {
	Conversation     string
	Served           string
	ActivityID       int64
	TS               int64
	PromptTokens     int
	CompletionTokens int
}

// LastTurns returns each conversation's newest served turn since fromMS.
// Oldest first, one query; the activity columns by primary key.
func (s *Store) LastTurns(fromMS int64) ([]LastTurn, error) {
	rows, err := s.db.Query(`
		SELECT c.conversation, c.served, c.activity_id, c.ts, a.prompt_tokens, a.completion_tokens
		FROM activity_conversation c
		JOIN (SELECT conversation, MAX(activity_id) AS id FROM activity_conversation
		      WHERE ts >= ? AND conversation NOT LIKE 'req:%' AND ctx_tokens > 0 GROUP BY conversation) l
		  ON l.id = c.activity_id
		JOIN activity a ON a.id = c.activity_id
		ORDER BY c.ts`, fromMS)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LastTurn
	for rows.Next() {
		var t LastTurn
		if err := rows.Scan(&t.Conversation, &t.Served, &t.ActivityID, &t.TS, &t.PromptTokens, &t.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
