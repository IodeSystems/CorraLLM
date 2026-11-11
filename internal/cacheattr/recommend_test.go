package cacheattr

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// window builds a 24 h window of rows: n of each class given, losing lost
// tokens each, at 1000 tok/s prefill.
func window(counts map[string]int, lost int) []store.CacheRow {
	var rows []store.CacheRow
	ts := int64(1_700_000_000_000)
	i := 0
	for _, class := range Classes {
		for range counts[class] {
			i++
			cause := ""
			switch class {
			case ClassReload:
				cause = []string{"pause:build swap", "crash:exit status 139", "idle-unload"}[i%3]
			case ClassDiverged:
				cause = "system"
			}
			match := 19500
			if class == ClassDiverged {
				match = 20000 - lost + 100
			}
			slot := -1
			if i%2 == 0 {
				slot = i % 2
			}
			rows = append(rows, store.CacheRow{TS: ts + int64(i)*60_000, Served: "swift", Class: class, Cause: cause,
				GapMS: int64(i%7) * 4 * 60_000, PromptTokens: 20000, CachedTokens: 20000 - lost, LostTok: lost,
				MatchChars: 30000, MatchTokEst: match, PromptPerSec: 1000, Slot: slot,
				Caller: fmt.Sprintf("sk-caller-%d-secret", i%2)})
		}
	}
	return rows
}

const day = int64(24 * 3600 * 1000)

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden (run with -update to accept):\n%s", name, got)
	}
}

func ids(r Report) string {
	var s []string
	for _, rec := range r.Recommendations {
		s = append(s, rec.ID)
	}
	return strings.Join(s, ",")
}

// The CLI text and the rules, on a window where every miss class is present.
func TestReportGolden(t *testing.T) {
	rows := window(map[string]int{ClassReload: 60, ClassDiverged: 80, ClassEvicted: 70, ClassCheckpoint: 50,
		ClassSlotSwitch: 30, ClassUnknown: 10, ClassHit: 500, ClassCold: 20}, 8000)
	events := []store.BackendEvent{{TS: 1_700_000_100_000, Model: "swift", Instance: "pgid:1", Kind: "stop", Reason: "pause:build swap", OK: true},
		{TS: 1_700_000_160_000, Model: "swift", Instance: "pgid:2", Kind: "start", Reason: "cold-spawn", OK: true}}
	s := SettingsFromCmd("llama-server --parallel 2 --kv-unified -c 172032 --cache-ram 18432 --ctx-checkpoints 8 --slot-prompt-similarity 0.9")
	s.MemAvailableMiB = 48 << 10
	r := BuildReport("swift", 1_700_000_000_000, 1_700_000_000_000+day, rows, events, s, "caller")
	golden(t, "report.txt", Text(r))
	if got := ids(r); got != "R1,R2,R3,R4,R5" {
		t.Errorf("rules fired: %s", got)
	}
}

// R1 keeps the RAM floor: with MemAvailable 20 GiB and 18 GiB of cache-ram
// not yet used, there is no room to raise it, and the rule says so and
// proposes smaller entries instead. With room, the raise is capped by it.
func TestR1RespectsTheRAMFloor(t *testing.T) {
	rows := window(map[string]int{ClassEvicted: 250}, 8000)
	s := SettingsFromCmd("llama-server --cache-ram 18432 --ctx-checkpoints 8")
	s.MemAvailableMiB = 20 << 10
	r := BuildReport("swift", 0, day, rows, nil, s, "")
	if len(r.Recommendations) == 0 || r.Recommendations[0].ID != "R1" ||
		!strings.Contains(r.Recommendations[0].Action, "--ctx-checkpoints 8 → 4") {
		t.Fatalf("no headroom: %+v", r.Recommendations)
	}
	// The cache already holds 16 GiB of its 18: only 2 GiB of the cap is
	// unclaimed, so 40 GiB available leaves 40-16-2 = 22 GiB of room, and the
	// raise is +50% = 9216 MiB.
	s.MemAvailableMiB, s.CacheBytes = 40<<10, 16<<30
	r = BuildReport("swift", 0, day, rows, nil, s, "")
	if !strings.Contains(r.Recommendations[0].Action, "--cache-ram 18432 → 27648") {
		t.Errorf("with room: %s", r.Recommendations[0].Action)
	}
	// Not measured: no number proposed.
	s.MemAvailableMiB = -1
	r = BuildReport("swift", 0, day, rows, nil, s, "")
	if !strings.Contains(r.Recommendations[0].Action, "not measured") {
		t.Errorf("unmeasured: %s", r.Recommendations[0].Action)
	}
}

// Too little data: no rule fires but R0; a short window with a good hit rate
// is still not judged.
func TestRulesNeedEnoughData(t *testing.T) {
	r := BuildReport("swift", 0, day, window(map[string]int{ClassEvicted: 50, ClassHit: 10}, 8000), nil, Settings{}, "")
	if ids(r) != "R0" {
		t.Errorf("fired %s", ids(r))
	}
	r = BuildReport("swift", 0, day, window(map[string]int{ClassHit: 500}, 0), nil, Settings{}, "")
	if ids(r) != "R0,R7" {
		t.Errorf("all hits: fired %s", ids(r))
	}
}

func TestSettingsFromCmd(t *testing.T) {
	s := SettingsFromCmd("${tool:llama} -m x.gguf -np 2 --cache-ram=40960 --ctx-checkpoints 8 -sps 0.9")
	if s.Parallel != 2 || s.CacheRAMMiB != 40960 || s.CtxCheckpoints != 8 || s.SlotSimilarity != 0.9 {
		t.Errorf("%+v", s)
	}
	if s := SettingsFromCmd("llama-server"); s.CacheRAMMiB != -1 || s.CtxCheckpoints != -1 {
		t.Errorf("unset flags: %+v", s)
	}
}
