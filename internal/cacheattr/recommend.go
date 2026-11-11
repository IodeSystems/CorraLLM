package cacheattr

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// Totals sums a window.
type Totals struct {
	Requests      int     `json:"requests"`
	Misses        int     `json:"misses"`
	PromptTokens  int64   `json:"promptTokens"`
	CachedTokens  int64   `json:"cachedTokens"`
	TokenHitPct   float64 `json:"tokenHitPct" doc:"cached ÷ prompt tokens, percent"`
	LostTokens    int64   `json:"lostTokens" doc:"tokens re-prefilled by misses"`
	RePrefillSec  float64 `json:"rePrefillSec" doc:"lost tokens ÷ the turn's prefill speed, seconds"`
	MissesPerHour float64 `json:"missesPerHour"`
	WindowHours   float64 `json:"windowHours"`
}

// ClassRow is one class's share. Misses counts the class's rows (for hit and
// cold too).
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

// GroupRow is one row of the --by caller/hour/affinity table. Misses counts
// the rows of that class; only the affinity table has hit rows.
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
	Groups          []GroupRow           `json:"groups"`
	ReloadReasons   []ReasonRow          `json:"reloadReasons"`
	Reloads         []store.BackendEvent `json:"reloads"`
	Settings        Settings             `json:"settings"`
	Recommendations []Recommendation     `json:"recommendations"`
}

// Rule thresholds (P75 N4 defaults; owner decision D-thresholds).
const (
	minWindow        = 6 * time.Hour
	minMisses        = 200
	r1EvictedPct     = 20
	ramFloorMiB      = 16 << 10 // MemAvailable left after a --cache-ram raise
	r2CheckpointPct  = 15
	maxCheckpoints   = 32
	ckptEntryMiB     = 350 // added cache entry size per checkpoint (1h measurement)
	r3SlotPct        = 10
	r3CoveragePct    = 90
	r4ReloadPct      = 10
	r5DivergedPct    = 25
	r6UnattributePct = 30
	r7HitPct         = 95
	defaultCkpts     = 8    // llama.cpp's --ctx-checkpoints default
	defaultCacheRAM  = 8192 // llama.cpp's --cache-ram default, MiB
)

// GapBuckets are the gap-since-previous-turn buckets, in order.
var GapBuckets = []string{"<1m", "1-5m", "5-15m", "15-60m", "1-6h", ">6h"}

func gapBucket(ms int64) string {
	switch m := time.Duration(ms) * time.Millisecond; {
	case ms < 0:
		return "first"
	case m < time.Minute:
		return "<1m"
	case m < 5*time.Minute:
		return "1-5m"
	case m < 15*time.Minute:
		return "5-15m"
	case m < time.Hour:
		return "15-60m"
	case m < 6*time.Hour:
		return "1-6h"
	default:
		return ">6h"
	}
}

// BuildReport aggregates rows (oldest first) and events of one window. by
// adds a group table: "caller", "hour" or "affinity" (anything else: none).
func BuildReport(served string, fromMS, toMS int64, rows []store.CacheRow, events []store.BackendEvent, s Settings, by string) Report {
	r := Report{Served: served, FromMS: fromMS, ToMS: toMS, Settings: s,
		Classes: []ClassRow{}, Gaps: []GapRow{}, Groups: []GroupRow{}, ReloadReasons: []ReasonRow{},
		Reloads: []store.BackendEvent{}, Recommendations: []Recommendation{}}
	tot := &r.Totals
	tot.WindowHours = float64(toMS-fromMS) / 3.6e6
	classes := map[string]*ClassRow{}
	gaps := map[[2]string]int{}
	groups := map[[2]string]*GroupRow{}
	reasons := map[string]*ReasonRow{}
	withSlot := 0
	for _, row := range rows {
		tot.Requests++
		tot.PromptTokens += int64(row.PromptTokens)
		tot.CachedTokens += int64(row.CachedTokens)
		if row.Slot >= 0 {
			withSlot++
		}
		c := classes[row.Class]
		if c == nil {
			c = &ClassRow{Class: row.Class}
			classes[row.Class] = c
		}
		c.Misses++
		if !IsMiss(row.Class) {
			// The affinity table is an A/B: it needs the hits beside the
			// misses, or a group's miss count has no denominator.
			if by == "affinity" {
				k := [2]string{groupOf(row, by), row.Class}
				if groups[k] == nil {
					groups[k] = &GroupRow{Group: k[0], Class: row.Class}
				}
				groups[k].Misses++
			}
			continue
		}
		tot.Misses++
		lost := int64(row.LostTok)
		sec := 0.0
		if row.PromptPerSec > 0 {
			sec = float64(row.LostTok) / row.PromptPerSec
		}
		tot.LostTokens += lost
		tot.RePrefillSec += sec
		c.LostTokens += lost
		c.Seconds += sec
		gaps[[2]string{gapBucket(row.GapMS), row.Class}]++
		if g := groupOf(row, by); g != "" {
			k := [2]string{g, row.Class}
			gr := groups[k]
			if gr == nil {
				gr = &GroupRow{Group: g, Class: row.Class}
				groups[k] = gr
			}
			gr.Misses++
			gr.LostTokens += lost
		}
		if row.Class == ClassReload {
			rr := reasons[row.Cause]
			if rr == nil {
				rr = &ReasonRow{Reason: row.Cause}
				reasons[row.Cause] = rr
			}
			rr.Count++
			rr.LostTokens += lost
		}
	}
	if tot.PromptTokens > 0 {
		tot.TokenHitPct = 100 * float64(tot.CachedTokens) / float64(tot.PromptTokens)
	}
	if tot.WindowHours > 0 {
		tot.MissesPerHour = float64(tot.Misses) / tot.WindowHours
	}
	if tot.Requests > 0 {
		r.Settings.IDSlotCoverage = 100 * float64(withSlot) / float64(tot.Requests)
	}
	for _, name := range Classes {
		if c := classes[name]; c != nil {
			if tot.LostTokens > 0 && IsMiss(name) {
				c.LostShare = 100 * float64(c.LostTokens) / float64(tot.LostTokens)
			}
			r.Classes = append(r.Classes, *c)
		}
	}
	for _, b := range append([]string{"first"}, GapBuckets...) {
		for _, name := range Classes {
			if n := gaps[[2]string{b, name}]; n > 0 {
				r.Gaps = append(r.Gaps, GapRow{Bucket: b, Class: name, Misses: n})
			}
		}
	}
	for _, g := range groups {
		r.Groups = append(r.Groups, *g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		if r.Groups[i].Group != r.Groups[j].Group {
			return r.Groups[i].Group < r.Groups[j].Group
		}
		return r.Groups[i].LostTokens > r.Groups[j].LostTokens
	})
	for _, rr := range reasons {
		r.ReloadReasons = append(r.ReloadReasons, *rr)
	}
	sort.Slice(r.ReloadReasons, func(i, j int) bool {
		if r.ReloadReasons[i].LostTokens != r.ReloadReasons[j].LostTokens {
			return r.ReloadReasons[i].LostTokens > r.ReloadReasons[j].LostTokens
		}
		return r.ReloadReasons[i].Reason < r.ReloadReasons[j].Reason
	})
	for _, e := range events {
		if e.Kind == "start" || e.Kind == "stop" {
			r.Reloads = append(r.Reloads, e)
		}
	}
	r.Recommendations = recommend(r, rows)
	return r
}

func groupOf(row store.CacheRow, by string) string {
	switch by {
	case "caller":
		c := row.Caller
		if len(c) > 12 {
			c = c[:12] + "…" // keys are secrets: enough to tell callers apart
		}
		if c == "" {
			c = "(none)"
		}
		return c
	case "hour":
		return time.UnixMilli(row.TS).Format("2006-01-02 15h")
	case "affinity":
		// What slot affinity did (P75 N9): the on/off A/B. "" is off.
		if row.Affinity == "" {
			return "(off)"
		}
		return row.Affinity
	}
	return ""
}

func (r Report) share(class string) float64 {
	for _, c := range r.Classes {
		if c.Class == class {
			return c.LostShare
		}
	}
	return 0
}

func (r Report) count(class string) int {
	for _, c := range r.Classes {
		if c.Class == class {
			return c.Misses
		}
	}
	return 0
}

// recommend applies R1-R7. Text only.
func recommend(r Report, rows []store.CacheRow) []Recommendation {
	out := []Recommendation{}
	s, t := r.Settings, r.Totals
	window := time.Duration(r.ToMS-r.FromMS) * time.Millisecond
	if window < minWindow || t.Misses < minMisses {
		out = append(out, Recommendation{ID: "R0", Severity: "info", Title: "Not enough data for recommendations",
			Evidence: fmt.Sprintf("%.1f h window, %d misses", window.Hours(), t.Misses),
			Action:   fmt.Sprintf("Widen the window to at least %.0f h with at least %d misses.", minWindow.Hours(), minMisses)})
		if window >= minWindow && t.Requests > 0 && t.TokenHitPct >= r7HitPct {
			out = append(out, r7(t))
		}
		return out
	}

	// R1: evicted → more --cache-ram, inside the RAM floor; else fewer checkpoints.
	if sh := r.share(ClassEvicted); sh >= r1EvictedPct {
		cur, curText := s.CacheRAMMiB, fmt.Sprintf("%d MiB", s.CacheRAMMiB)
		if cur < 0 {
			cur, curText = defaultCacheRAM, fmt.Sprintf("not set (llama.cpp default %d MiB)", defaultCacheRAM)
		}
		ev := fmt.Sprintf("%s of lost tokens; %d evicted misses; evictions/h %s; --cache-ram %s; MemAvailable %s",
			pct(sh), r.count(ClassEvicted), perHour(s.EvictionsPerHour), curText, gib(s.MemAvailableMiB))
		rec := Recommendation{ID: "R1", Severity: "warn", Title: "Prompt-cache entries are evicted", Evidence: ev}
		inc := 0
		if s.MemAvailableMiB >= 0 {
			// Raising the cap lets the cache grow to the new cap: what it does
			// not use yet must come out of MemAvailable too.
			unused := int64(cur)
			if s.CacheBytes >= 0 {
				unused = max(0, int64(cur)-s.CacheBytes>>20)
			}
			room := s.MemAvailableMiB - ramFloorMiB - unused
			inc = int(min(int64(cur/2), max(0, room)))
		}
		switch {
		case s.MemAvailableMiB < 0:
			rec.Action = "MemAvailable is not measured here; run the report on the backend's host before raising --cache-ram."
		case inc >= 1024:
			rec.Action = fmt.Sprintf("Raise --cache-ram %d → %d (MiB). Keeps MemAvailable ≥ %d GiB.", cur, cur+inc, ramFloorMiB>>10)
		default:
			n := s.CtxCheckpoints
			if n < 0 {
				n = defaultCkpts
			}
			rec.Action = fmt.Sprintf("No RAM headroom: raising --cache-ram would leave MemAvailable under %d GiB. Make entries smaller instead: --ctx-checkpoints %d → %d.",
				ramFloorMiB>>10, n, max(1, n/2))
		}
		out = append(out, rec)
	}

	// R2: checkpoint-limited → more checkpoints.
	if sh := r.share(ClassCheckpoint); sh >= r2CheckpointPct {
		n, nText := s.CtxCheckpoints, fmt.Sprint(s.CtxCheckpoints)
		if n < 0 {
			n, nText = defaultCkpts, fmt.Sprintf("not set (default %d)", defaultCkpts)
		}
		if n < maxCheckpoints {
			to := min(2*n, maxCheckpoints)
			gap := median(rows, ClassCheckpoint, func(c store.CacheRow) int { return c.MatchTokEst - c.CachedTokens })
			out = append(out, Recommendation{ID: "R2", Severity: "warn", Title: "Reuse stops at the last checkpoint",
				Evidence: fmt.Sprintf("%s of lost tokens; %d misses; median match − cached %d tok; --ctx-checkpoints %s",
					pct(sh), r.count(ClassCheckpoint), gap, nText),
				Action: fmt.Sprintf("Raise --ctx-checkpoints %d → %d. Each cache entry grows by ~%d MiB (~%d MiB per checkpoint), set against --cache-ram %s.",
					n, to, (to-n)*ckptEntryMiB, ckptEntryMiB, mib(s.CacheRAMMiB))})
		}
	}

	// R3: slot switches → affinity, once id_slot coverage can show it.
	if sh := r.share(ClassSlotSwitch); sh >= r3SlotPct {
		rec := Recommendation{ID: "R3", Severity: "warn", Title: "Turns change slot",
			Evidence: fmt.Sprintf("%s of lost tokens; %d misses; id_slot on %s of rows; --parallel %s",
				pct(sh), r.count(ClassSlotSwitch), pct(s.IDSlotCoverage), flagText(s.Parallel))}
		if s.IDSlotCoverage >= r3CoveragePct {
			rec.Action = "Enable slot affinity for this model (`slotAffinity: true`, off by default); compare with `--by affinity`."
		} else {
			rec.Action = "Deploy patch 0005 so every reply carries id_slot, then measure again."
		}
		out = append(out, rec)
	}

	// R4: reloads, by cause.
	if sh := r.share(ClassReload); sh >= r4ReloadPct {
		var lines []string
		for _, rr := range r.ReloadReasons {
			lines = append(lines, fmt.Sprintf("%s ×%d (%d tok): %s", orNone(rr.Reason), rr.Count, rr.LostTokens, reloadAdvice(rr.Reason)))
		}
		out = append(out, Recommendation{ID: "R4", Severity: "warn", Title: "Reloads empty the cache",
			Evidence: fmt.Sprintf("%s of lost tokens; %d misses", pct(sh), r.count(ClassReload)),
			Action:   strings.Join(lines, "; ")})
	}

	// R5: client-side divergence, by caller.
	if sh := r.share(ClassDiverged); sh >= r5DivergedPct {
		out = append(out, Recommendation{ID: "R5", Severity: "warn", Title: "Callers change the start of the prompt",
			Evidence: fmt.Sprintf("%s of lost tokens; %d misses", pct(sh), r.count(ClassDiverged)),
			Action:   "Client-side fix: keep earlier messages byte-identical between turns. " + divergedByCaller(rows)})
	}

	// R6: attribution incomplete.
	if sh := r.share(ClassUnknown) + r.share(ClassNoHistory); sh >= r6UnattributePct {
		out = append(out, Recommendation{ID: "R6", Severity: "info", Title: "Attribution is incomplete",
			Evidence: fmt.Sprintf("unknown + no-history are %s of lost tokens; /metrics counters %s",
				pct(sh), exposed(s.CacheEntries)),
			Action: "Deploy patch 0005 (/metrics counters, id_slot). no-history is high right after each restart: compare windows that exclude one."})
	}

	if t.TokenHitPct >= r7HitPct {
		out = append(out, r7(t))
	}
	return out
}

func r7(t Totals) Recommendation {
	return Recommendation{ID: "R7", Severity: "info", Title: "The cache is doing its job",
		Evidence: fmt.Sprintf("token hit %s", pct(t.TokenHitPct)), Action: "No action."}
}

func reloadAdvice(reason string) string {
	switch {
	case strings.HasPrefix(reason, "pause:") || strings.Contains(reason, "build"):
		return "move the build window or pre-warm after it"
	case strings.HasPrefix(reason, "crash"):
		return "see the backend log (Models → logs) for the exit"
	case reason == "idle-unload":
		return "raise the sticky idle TTL"
	case strings.HasPrefix(reason, "evicted-for:"):
		return "change residency or pin this model"
	case reason == "refresh":
		return "the non-ECC refresh; lengthen its period if the cost is too high"
	case reason == "handoff":
		return "a corrallm upgrade; expected"
	}
	return "check backend_event for the cause"
}

func divergedByCaller(rows []store.CacheRow) string {
	type agg struct {
		n      int
		causes map[string]int
		match  []int
	}
	by := map[string]*agg{}
	for _, row := range rows {
		if row.Class != ClassDiverged {
			continue
		}
		g := groupOf(row, "caller")
		a := by[g]
		if a == nil {
			a = &agg{causes: map[string]int{}}
			by[g] = a
		}
		a.n++
		a.causes[row.Cause]++
		a.match = append(a.match, row.MatchChars)
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return by[keys[i]].n > by[keys[j]].n || (by[keys[i]].n == by[keys[j]].n && keys[i] < keys[j])
	})
	var parts []string
	for _, k := range keys {
		a := by[k]
		top, topN := "", 0
		for c, n := range a.causes {
			if n > topN || (n == topN && c < top) {
				top, topN = c, n
			}
		}
		sort.Ints(a.match)
		parts = append(parts, fmt.Sprintf("%s: %d misses, mostly at %s, median match %d chars", k, a.n, orNone(top), a.match[len(a.match)/2]))
	}
	return strings.Join(parts, "; ")
}

func median(rows []store.CacheRow, class string, f func(store.CacheRow) int) int {
	var v []int
	for _, row := range rows {
		if row.Class == class {
			v = append(v, f(row))
		}
	}
	if len(v) == 0 {
		return 0
	}
	sort.Ints(v)
	return v[len(v)/2]
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v) }

func gib(mib int64) string {
	if mib < 0 {
		return "not measured"
	}
	return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
}

func mib(v int) string {
	if v < 0 {
		return "not set"
	}
	return fmt.Sprintf("%d MiB", v)
}

func perHour(v float64) string {
	if v < 0 {
		return "not exposed (no 0005)"
	}
	return fmt.Sprintf("%.1f", v)
}

func flagText(v int) string {
	if v < 0 {
		return "not set"
	}
	return fmt.Sprint(v)
}

func exposed(v int64) string {
	if v < 0 {
		return "not exposed"
	}
	return "exposed"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Text renders a report for the CLI.
func Text(r Report) string {
	var b strings.Builder
	t := r.Totals
	fmt.Fprintf(&b, "%s  %s → %s (%.1f h)\n", orNone(r.Served),
		time.UnixMilli(r.FromMS).UTC().Format("2006-01-02 15:04Z"), time.UnixMilli(r.ToMS).UTC().Format("2006-01-02 15:04Z"), t.WindowHours)
	fmt.Fprintf(&b, "requests %d · misses %d (%.1f/h) · prompt %d tok · cached %s · lost %d tok · re-prefill %.0f s\n\n",
		t.Requests, t.Misses, t.MissesPerHour, t.PromptTokens, pct(t.TokenHitPct), t.LostTokens, t.RePrefillSec)

	fmt.Fprintf(&b, "%-20s %8s %9s %12s %10s\n", "class", "turns", "% lost", "lost tok", "re-prefill")
	for _, c := range r.Classes {
		fmt.Fprintf(&b, "%-20s %8d %8.1f%% %12d %9.0fs\n", c.Class, c.Misses, c.LostShare, c.LostTokens, c.Seconds)
	}

	b.WriteString("\nmisses by gap since the previous turn\n")
	fmt.Fprintf(&b, "%-20s", "class")
	buckets := append([]string{"first"}, GapBuckets...)
	for _, g := range buckets {
		fmt.Fprintf(&b, " %7s", g)
	}
	b.WriteString("\n")
	for _, name := range Classes {
		if !IsMiss(name) {
			continue
		}
		row, any := fmt.Sprintf("%-20s", name), false
		for _, g := range buckets {
			n := 0
			for _, gr := range r.Gaps {
				if gr.Bucket == g && gr.Class == name {
					n = gr.Misses
				}
			}
			any = any || n > 0
			row += fmt.Sprintf(" %7d", n)
		}
		if any {
			b.WriteString(row + "\n")
		}
	}

	if len(r.Groups) > 0 {
		b.WriteString("\nmisses by group\n")
		for _, g := range r.Groups {
			fmt.Fprintf(&b, "  %-16s %-20s %6d %12d tok\n", g.Group, g.Class, g.Misses, g.LostTokens)
		}
	}

	if len(r.ReloadReasons) > 0 {
		b.WriteString("\nreloads by reason\n")
		for _, rr := range r.ReloadReasons {
			fmt.Fprintf(&b, "  %-36s %4d × %12d tok\n", orNone(rr.Reason), rr.Count, rr.LostTokens)
		}
	}

	s := r.Settings
	fmt.Fprintf(&b, "\nsettings: --cache-ram %s · --ctx-checkpoints %s · --parallel %s · --slot-prompt-similarity %s · MemAvailable %s · id_slot on %s of rows · cache entries %s\n",
		mib(s.CacheRAMMiB), flagText(s.CtxCheckpoints), flagText(s.Parallel), simText(s.SlotSimilarity),
		gib(s.MemAvailableMiB), pct(s.IDSlotCoverage), countText(s.CacheEntries))

	b.WriteString("\nrecommendations (text only; nothing is applied)\n")
	for _, rec := range r.Recommendations {
		fmt.Fprintf(&b, "  %s [%s] %s\n      evidence: %s\n      action:   %s\n", rec.ID, rec.Severity, rec.Title, rec.Evidence, rec.Action)
	}
	return b.String()
}

func simText(v float64) string {
	if v < 0 {
		return "not set"
	}
	return fmt.Sprint(v)
}

func countText(v int64) string {
	if v < 0 {
		return "not exposed"
	}
	return fmt.Sprint(v)
}
