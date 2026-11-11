package cacheattr

import (
	"database/sql"
	"fmt"
	"hash/maphash"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/iodesystems/corrallm/internal/slotcache"
	"github.com/iodesystems/corrallm/internal/store"
)

// TestReplayProduction classifies a production database's last N days of
// one model's chat turns with the tracker, READ-ONLY, and prints the class
// split — overall, and over N2's "eviction-like" subset (previous context
// > 4096 tokens, cached < 10% of it) by gap, to compare with N2's backfill.
// Skipped unless CORRALLM_REPLAY_DB names the database file.
//
//	CORRALLM_REPLAY_DB=/path/corrallm.db CORRALLM_REPLAY_MODEL=<served> \
//	  go test ./internal/cacheattr -run TestReplayProduction -v -timeout 30m
//
// History has no instance ids, id_slot or /metrics: one instance throughout,
// reloads from model_load rows only (cold spawns), evictions by the 300k
// other-traffic proxy. Bodies were captured truncated at 256 KiB, so the
// replay hashes the raw captured JSON rather than slotcache.Inspect's stream,
// and pads a truncated body's hashes with a constant so that two truncated
// turns which agree up to the cut are taken to agree after it.
func TestReplayProduction(t *testing.T) {
	path := os.Getenv("CORRALLM_REPLAY_DB")
	if path == "" {
		t.Skip("set CORRALLM_REPLAY_DB to replay a production database")
	}
	model := os.Getenv("CORRALLM_REPLAY_MODEL")
	days := 14
	if d, err := strconv.Atoi(os.Getenv("CORRALLM_REPLAY_DAYS")); err == nil && d > 0 {
		days = d
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	from := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	tr := NewTracker(0, nil)
	lrows, err := db.Query(`SELECT ts FROM model_load WHERE model = ? AND ok = 1 AND ts > ? ORDER BY ts`, model, from-day)
	if err != nil {
		t.Fatal(err)
	}
	var loads []int64
	for lrows.Next() {
		var ts int64
		if err := lrows.Scan(&ts); err != nil {
			t.Fatal(err)
		}
		loads = append(loads, ts)
	}
	_ = lrows.Close()

	rows, err := db.Query(`
		SELECT a.id, a.ts, c.conversation, a.prompt_tokens, a.cached_tokens, a.completion_tokens, a.prompt_per_sec,
		       COALESCE(p.req_body, '')
		FROM activity a JOIN activity_conversation c ON c.activity_id = a.id
		LEFT JOIN activity_payload p ON p.activity_id = a.id
		WHERE a.served = ? AND a.status = 200 AND a.prompt_tokens > 0 AND a.ts > ?
		ORDER BY a.ts, a.id`, model, from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	seed := maphash.MakeSeed()
	trunc := regexp.MustCompile(`…\(\+(\d+) bytes truncated\)$`)
	overall := map[string]int{}
	subset := map[[2]string]int{}
	noBody, n, li := 0, 0, 0
	start := time.Now()
	for rows.Next() {
		var id, ts int64
		var conv, body string
		var prompt, cached, completion int
		var pps float64
		if err := rows.Scan(&id, &ts, &conv, &prompt, &cached, &completion, &pps, &body); err != nil {
			t.Fatal(err)
		}
		n++
		for li < len(loads) && loads[li] <= ts {
			tr.NoteEvent(store.BackendEvent{TS: loads[li], Model: model, Kind: "start", Reason: "cold-spawn"})
			li++
		}
		turn := &slotcache.Turn{Key: conv, LastUserStart: -1}
		if body == "" {
			noBody++
		} else {
			raw, extra := body, 0
			if m := trunc.FindStringSubmatchIndex(body); m != nil {
				extra, _ = strconv.Atoi(body[m[2]:m[3]])
				raw = body[:m[0]]
			}
			turn.Chars = len(raw) + extra
			for i := 0; i < len(raw); i += slotcache.BlockSize {
				turn.Blocks = append(turn.Blocks, maphash.String(seed, raw[i:min(i+slotcache.BlockSize, len(raw))]))
			}
			for len(turn.Blocks)*slotcache.BlockSize < turn.Chars {
				turn.Blocks = append(turn.Blocks, 0) // past the cut: assume it agrees
			}
			if i := strings.LastIndex(raw, `"role":"user"`); i >= 0 {
				turn.LastUserStart = i
			}
		}
		row, ok := tr.Observe(Obs{Served: model, Instance: "replay", Slot: -1, TS: ts, PromptTokens: prompt,
			CachedTokens: cached, CompletionTokens: completion, PromptPerSec: pps, CtxCheckpoints: true, Turn: turn})
		if !ok {
			continue
		}
		tr.Committed(conv, ts, id)
		overall[row.Class]++
		if row.PrevCtxTokens > 4096 && float64(cached) < 0.1*float64(row.PrevCtxTokens) {
			g := "<5m"
			if row.GapMS >= 5*60_000 {
				g = ">=5m"
			}
			subset[[2]string{g, row.Class}]++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d turns of %s over %d d (%d without a body, %d loads) in %s", n, model, days, noBody, len(loads), time.Since(start).Round(time.Second))
	t.Logf("all turns: %s", split(overall))
	for _, g := range []string{"<5m", ">=5m"} {
		m := map[string]int{}
		for k, v := range subset {
			if k[0] == g {
				m[k[1]] = v
			}
		}
		t.Logf("eviction-like (N2 subset), gap %s: %s", g, split(m))
	}
}

func split(m map[string]int) string {
	keys := make([]string, 0, len(m))
	total := 0
	for k, v := range m {
		keys = append(keys, k)
		total += v
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return fmt.Sprintf("total %d: %s", total, strings.Join(parts, " · "))
}
