package cacheattr

import (
	"container/list"
	"fmt"
	"hash/maphash"
	"sync"
	"time"

	"github.com/iodesystems/corrallm/internal/slotcache"
	"github.com/iodesystems/corrallm/internal/store"
)

// Obs is one served chat turn, as the proxy saw it.
type Obs struct {
	Served           string
	Instance         string // the backend process (proc.Process.Instance)
	Slot             int    // id_slot from the reply; -1 when absent
	TS               int64  // unix ms
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	PromptPerSec     float64
	CtxCheckpoints   bool
	Turn             *slotcache.Turn
}

// Evictions reads prompt-cache evictions on one instance between two
// moments; -1 when the backend exposes no counter (no 0005 /metrics).
type Evictions interface {
	EvictionsBetween(instance string, fromMS, toMS int64) int
}

// DefaultCapBytes caps the tracker's memory: ~13k conversations at 600 KB.
const DefaultCapBytes = 64 << 20

const (
	stubTTL       = 24 * time.Hour
	maxBoundaries = 16 // the last 8 turns, two boundaries each
	maxEvents     = 4096
	shards        = 16
	stubOverhead  = 256 // bytes a stub costs beyond its hashes
)

// stub is a conversation's previous turn.
type stub struct {
	key        string
	served     string
	blocks     []uint64
	chars      int
	hashes     bool
	promptTok  int
	ctxTok     int
	boundaries []int
	instance   string
	slot       int
	ts         int64
	instTok    int64 // the instance's token counter after this turn
	activityID int64
	elem       *list.Element
}

func (s *stub) size() int64 { return int64(len(s.blocks)*8+len(s.boundaries)*8) + stubOverhead }

type shard struct {
	mu    sync.Mutex
	m     map[string]*stub
	lru   *list.List // front = newest
	bytes int64
}

// Tracker remembers each conversation's previous turn, in memory only.
//
// Sharded by key and LRU-capped: a conversation not seen for a while is the
// one to forget, and forgetting it costs a no-history verdict, never a
// request. Seeded at startup with stubs that carry no hashes (Seed), so the
// first turn after a restart is a reload or no-history rather than cold.
type Tracker struct {
	shards   [shards]shard
	capShard int64
	seed     maphash.Seed
	ev       Evictions

	emu    sync.Mutex
	events []store.BackendEvent // the last 24 h, oldest first

	imu  sync.Mutex
	inst map[string]int64 // instance → tokens served (prompt + completion)
}

// NewTracker makes a tracker capped at capBytes (0 = DefaultCapBytes). ev may
// be nil.
func NewTracker(capBytes int64, ev Evictions) *Tracker {
	if capBytes <= 0 {
		capBytes = DefaultCapBytes
	}
	t := &Tracker{capShard: capBytes / shards, seed: maphash.MakeSeed(), ev: ev, inst: map[string]int64{}}
	for i := range t.shards {
		t.shards[i].m = map[string]*stub{}
		t.shards[i].lru = list.New()
	}
	return t
}

func (t *Tracker) shardOf(key string) *shard {
	return &t.shards[maphash.String(t.seed, key)%shards]
}

// Observe classifies a turn against the conversation's previous one and
// records it as the new previous turn. ok is false when there is nothing to
// attribute (no conversation key). The row's ActivityID is 0; Committed gives
// the tracker the id once the row is written.
func (t *Tracker) Observe(o Obs) (store.CacheRow, bool) {
	if t == nil || o.Turn == nil || o.Turn.Key == "" {
		return store.CacheRow{}, false
	}
	turn := o.Turn
	t.imu.Lock()
	instBefore := t.inst[o.Instance]
	instAfter := instBefore + int64(o.PromptTokens+o.CompletionTokens)
	t.inst[o.Instance] = instAfter
	t.imu.Unlock()

	row := store.CacheRow{
		TS: o.TS, Served: o.Served, Conversation: turn.Key, Instance: o.Instance, Slot: o.Slot,
		GapMS: -1, PromptTokens: o.PromptTokens, CachedTokens: o.CachedTokens, PrevCtxTokens: -1,
		PromptChars: turn.Chars, MatchChars: -1, MatchTokEst: -1, DivergeMsg: -1,
		OtherTok: -1, EvictionsBetween: -1, CkptDistTok: -1, PromptPerSec: o.PromptPerSec,
	}
	in := Input{PromptTokens: o.PromptTokens, CachedTokens: o.CachedTokens, Slot: o.Slot,
		PrevSlot: -1, MatchTokEst: -1, Evictions: -1, OtherTok: -1, CtxCheckpoints: o.CtxCheckpoints}

	sh := t.shardOf(turn.Key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	s := sh.m[turn.Key]
	if s != nil && o.TS-s.ts > stubTTL.Milliseconds() {
		t.dropLocked(sh, s)
		s = nil
	}
	if s != nil {
		in.HaveStub = true
		in.PrevPromptTokens = s.promptTok
		in.PrevSlot = s.slot
		in.Boundaries = s.boundaries
		row.PrevActivityID, row.GapMS, row.PrevCtxTokens = s.activityID, o.TS-s.ts, s.ctxTok
		if s.instance != "" && s.instance != o.Instance {
			in.Reload, in.ReloadCause = true, "instance changed"
		}
		if cause, ok := t.restartBetween(o.Served, s.ts, o.TS); ok {
			in.Reload, in.ReloadCause = true, cause
		}
		if s.instance == o.Instance {
			in.OtherTok = max(0, instBefore-s.instTok)
			if t.ev != nil {
				in.Evictions = t.ev.EvictionsBetween(o.Instance, s.ts, o.TS)
			}
		}
		row.OtherTok, row.EvictionsBetween = in.OtherTok, in.Evictions
		if s.hashes && len(turn.Blocks) > 0 && turn.Chars > 0 {
			in.HaveHashes = true
			chars, seg := slotcache.MatchChars(s.blocks, s.chars, *turn)
			row.MatchChars = chars
			in.MatchTokEst = int(int64(chars) * int64(o.PromptTokens) / int64(turn.Chars))
			row.MatchTokEst = in.MatchTokEst
			if seg >= 0 {
				row.DivergeMsg = seg
				if turn.Tools {
					row.DivergeMsg = seg - 1 // message index; -1 → tools, stored as -2
					if seg == 0 {
						row.DivergeMsg = -2
					}
				}
				in.DivergeCause = divergeCause(turn, seg)
			}
		}
	}
	v := Classify(in)
	row.Class, row.Cause, row.LostTok, row.CkptDistTok = v.Class, v.Cause, v.LostTok, v.CkptDist

	// This turn becomes the previous one.
	if s == nil {
		s = &stub{key: turn.Key}
		sh.m[turn.Key] = s
		s.elem = sh.lru.PushFront(s)
	} else {
		sh.lru.MoveToFront(s.elem)
		sh.bytes -= s.size()
	}
	bounds := append(s.boundaries, o.PromptTokens)
	if turn.LastUserStart >= 0 && turn.Chars > 0 {
		bounds = append(bounds, int(int64(turn.LastUserStart)*int64(o.PromptTokens)/int64(turn.Chars)))
	}
	if len(bounds) > maxBoundaries {
		bounds = append([]int(nil), bounds[len(bounds)-maxBoundaries:]...)
	}
	s.served, s.blocks, s.chars, s.hashes = o.Served, turn.Blocks, turn.Chars, len(turn.Blocks) > 0
	s.promptTok, s.ctxTok, s.boundaries = o.PromptTokens, o.PromptTokens+o.CompletionTokens, bounds
	s.instance, s.slot, s.ts, s.instTok, s.activityID = o.Instance, o.Slot, o.TS, instAfter, 0
	sh.bytes += s.size()
	for sh.bytes > t.capShard && sh.lru.Len() > 1 {
		t.dropLocked(sh, sh.lru.Back().Value.(*stub))
	}
	return row, true
}

func (t *Tracker) dropLocked(sh *shard, s *stub) {
	sh.lru.Remove(s.elem)
	delete(sh.m, s.key)
	sh.bytes -= s.size()
}

func divergeCause(turn *slotcache.Turn, seg int) string {
	if seg < 0 || seg >= len(turn.Roles) {
		return ""
	}
	role := turn.Roles[seg]
	switch role {
	case "tools", "system":
		return role
	}
	msg := seg
	if turn.Tools {
		msg--
	}
	return fmt.Sprintf("%s message %d", role, msg)
}

// restartBetween reports a backend start for served in (fromMS, toMS], and
// why: the reason of the stop before it, when there is one.
func (t *Tracker) restartBetween(served string, fromMS, toMS int64) (string, bool) {
	t.emu.Lock()
	defer t.emu.Unlock()
	for i := len(t.events) - 1; i >= 0; i-- {
		e := t.events[i]
		if e.TS <= fromMS {
			return "", false
		}
		if e.TS > toMS || e.Model != served || e.Kind != "start" {
			continue
		}
		for j := i - 1; j >= 0 && t.events[j].TS > fromMS; j-- {
			if p := t.events[j]; p.Model == served && p.Kind == "stop" {
				return p.Reason, true
			}
		}
		return "start:" + e.Reason, true
	}
	return "", false
}

// Committed records the activity id of the turn Observe returned for
// conversation conv at moment ts. A later turn having replaced it is fine:
// the id is then simply not needed.
func (t *Tracker) Committed(conv string, ts, activityID int64) {
	if t == nil || conv == "" {
		return
	}
	sh := t.shardOf(conv)
	sh.mu.Lock()
	if s := sh.m[conv]; s != nil && s.ts == ts {
		s.activityID = activityID
	}
	sh.mu.Unlock()
}

// NoteEvent records a backend lifecycle event.
func (t *Tracker) NoteEvent(e store.BackendEvent) {
	if t == nil {
		return
	}
	t.emu.Lock()
	defer t.emu.Unlock()
	t.events = append(t.events, e)
	cut := 0
	for cut < len(t.events) && (len(t.events)-cut > maxEvents || e.TS-t.events[cut].TS > stubTTL.Milliseconds()) {
		cut++
	}
	if cut > 0 {
		t.events = append([]store.BackendEvent(nil), t.events[cut:]...)
	}
}

// Seed loads conversation stubs without hashes, and the recent events, at
// startup. Turns are expected oldest first; a later one for the same
// conversation replaces an earlier one.
func (t *Tracker) Seed(turns []store.LastTurn, events []store.BackendEvent) {
	if t == nil {
		return
	}
	for _, e := range events {
		t.NoteEvent(e)
	}
	for _, lt := range turns {
		sh := t.shardOf(lt.Conversation)
		sh.mu.Lock()
		s := sh.m[lt.Conversation]
		if s == nil {
			s = &stub{key: lt.Conversation}
			sh.m[lt.Conversation] = s
			s.elem = sh.lru.PushBack(s) // seeded stubs are the first to go
		} else {
			sh.bytes -= s.size()
		}
		s.served, s.ts, s.activityID, s.slot = lt.Served, lt.TS, lt.ActivityID, -1
		s.promptTok, s.ctxTok = lt.PromptTokens, lt.PromptTokens+lt.CompletionTokens
		s.boundaries = []int{lt.PromptTokens}
		sh.bytes += s.size()
		for sh.bytes > t.capShard && sh.lru.Len() > 1 {
			t.dropLocked(sh, sh.lru.Back().Value.(*stub))
		}
		sh.mu.Unlock()
	}
}

// Len is the number of conversations held, for tests and the report.
func (t *Tracker) Len() int {
	n := 0
	for i := range t.shards {
		t.shards[i].mu.Lock()
		n += len(t.shards[i].m)
		t.shards[i].mu.Unlock()
	}
	return n
}
