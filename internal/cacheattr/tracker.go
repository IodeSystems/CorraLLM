package cacheattr

import (
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
	CachedTokens     int
	CompletionTokens int
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

// Tracker remembers each conversation's previous turn.
type Tracker struct{}

// NewTracker makes a tracker capped at capBytes (0 = DefaultCapBytes).
func NewTracker(capBytes int64, ev Evictions) *Tracker { return &Tracker{} }

// Observe classifies a turn against the conversation's previous one and
// records it as the new previous turn. The row's ActivityID is 0; Committed
// gives the tracker the id once the row is written.
func (t *Tracker) Observe(o Obs) (store.CacheRow, bool) { return store.CacheRow{}, false }

// Committed records the activity id of the turn Observe last returned for
// conversation conv at moment ts.
func (t *Tracker) Committed(conv string, ts, activityID int64) {}

// NoteEvent records a backend lifecycle event.
func (t *Tracker) NoteEvent(e store.BackendEvent) {}

// Seed loads conversation stubs (no hashes) and recent events at startup.
func (t *Tracker) Seed(turns []store.LastTurn, events []store.BackendEvent) {}
