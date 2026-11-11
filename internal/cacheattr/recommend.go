package cacheattr

import "github.com/iodesystems/corrallm/internal/store"

// Totals sums a window.
type Totals struct {
	Requests      int     `json:"requests"`
	Misses        int     `json:"misses"`
	PromptTokens  int64   `json:"promptTokens"`
	CachedTokens  int64   `json:"cachedTokens"`
	TokenHitPct   float64 `json:"tokenHitPct" doc:"cached ÷ prompt tokens, percent"`
	LostTokens    int64   `json:"lostTokens"`
	RePrefillSec  float64 `json:"rePrefillSec" doc:"lost tokens ÷ the turn's prefill speed, seconds"`
	MissesPerHour float64 `json:"missesPerHour"`
	WindowHours   float64 `json:"windowHours"`
}

// ClassRow is one class's share.
type ClassRow struct {
	Class      string  `json:"class"`
	Misses     int     `json:"misses"`
	LostTokens int64   `json:"lostTokens"`
	LostShare  float64 `json:"lostShare" doc:"percent of the window's lost tokens"`
	Seconds    float64 `json:"seconds" doc:"re-prefill seconds"`
}

// GapRow is one (gap bucket, class) cell.
type GapRow struct {
	Bucket string `json:"bucket"`
	Class  string `json:"class"`
	Misses int    `json:"misses"`
}

// GroupRow is one row of the --by caller/hour table.
type GroupRow struct {
	Group      string `json:"group"`
	Class      string `json:"class"`
	Misses     int    `json:"misses"`
	LostTokens int64  `json:"lostTokens"`
}

// ReasonRow is reloads grouped by cause.
type ReasonRow struct {
	Reason     string `json:"reason"`
	Count      int    `json:"count"`
	LostTokens int64  `json:"lostTokens"`
}

// Settings are the backend's cache flags and the host's memory, read from
// the model's cmd, sysmem and the /metrics poller. -1 = not known.
type Settings struct {
	CacheRAMMiB      int     `json:"cacheRamMiB" doc:"--cache-ram, MiB; -1 = not set"`
	CtxCheckpoints   int     `json:"ctxCheckpoints" doc:"--ctx-checkpoints; -1 = not set"`
	Parallel         int     `json:"parallel" doc:"--parallel; -1 = not set"`
	SlotSimilarity   float64 `json:"slotSimilarity" doc:"--slot-prompt-similarity; -1 = not set"`
	MemAvailableMiB  int64   `json:"memAvailableMiB" doc:"host MemAvailable, MiB; -1 = not measured"`
	CacheEntries     int64   `json:"cacheEntries" doc:"prompt-cache entries (0005 /metrics); -1 = not exposed"`
	CacheBytes       int64   `json:"cacheBytes" doc:"prompt-cache bytes (0005 /metrics); -1 = not exposed"`
	EvictionsPerHour float64 `json:"evictionsPerHour" doc:"-1 = not exposed"`
	IDSlotCoverage   float64 `json:"idSlotCoverage" doc:"percent of rows whose reply carried id_slot"`
}

// Recommendation is one rule that fired. Text only: nothing is applied.
type Recommendation struct {
	ID       string `json:"id"`
	Severity string `json:"severity" enum:"info,warn"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	Action   string `json:"action"`
}

// Report is a window's attribution.
type Report struct {
	Served          string               `json:"served"`
	FromMS          int64                `json:"fromMs"`
	ToMS            int64                `json:"toMs"`
	Totals          Totals               `json:"totals"`
	Classes         []ClassRow           `json:"classes"`
	Gaps            []GapRow             `json:"gaps"`
	Groups          []GroupRow           `json:"groups,omitempty"`
	ReloadReasons   []ReasonRow          `json:"reloadReasons"`
	Reloads         []store.BackendEvent `json:"reloads"`
	Settings        Settings             `json:"settings"`
	Recommendations []Recommendation     `json:"recommendations"`
}

// BuildReport aggregates rows (oldest first) and events of one window.
func BuildReport(served string, fromMS, toMS int64, rows []store.CacheRow, events []store.BackendEvent, s Settings, by string) Report {
	return Report{Served: served, FromMS: fromMS, ToMS: toMS, Settings: s,
		Classes: []ClassRow{}, Gaps: []GapRow{}, ReloadReasons: []ReasonRow{},
		Reloads: []store.BackendEvent{}, Recommendations: []Recommendation{}}
}

// Text renders a report for the CLI.
func Text(r Report) string { return "" }
