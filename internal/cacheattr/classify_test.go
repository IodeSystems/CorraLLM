package cacheattr

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/corrallm/internal/slotcache"
	"github.com/iodesystems/corrallm/internal/store"
)

// One case per class, with and without id_slot and /metrics.
func TestClassify(t *testing.T) {
	base := Input{HaveStub: true, HaveHashes: true, PromptTokens: 20000, PrevPromptTokens: 18000,
		MatchTokEst: 18000, Slot: -1, PrevSlot: -1, Evictions: -1, OtherTok: -1}
	with := func(f func(*Input)) Input { in := base; f(&in); return in }
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"cold", Input{PromptTokens: 5000}, ClassCold},
		{"hit", with(func(in *Input) { in.CachedTokens = 17900 }), ClassHit},
		{"small loss is a hit", with(func(in *Input) { in.CachedTokens = 17000 }), ClassHit},
		{"reload", with(func(in *Input) { in.Reload, in.ReloadCause = true, "crash:exit 1" }), ClassReload},
		{"no-history", with(func(in *Input) { in.HaveHashes, in.MatchTokEst = false, -1 }), ClassNoHistory},
		{"diverged", with(func(in *Input) { in.MatchTokEst, in.CachedTokens = 3000, 2900 }), ClassDiverged},
		{"slot-switch", with(func(in *Input) { in.Slot, in.PrevSlot = 1, 0 }), ClassSlotSwitch},
		{"same slot is not a switch", with(func(in *Input) { in.Slot, in.PrevSlot = 1, 1 }), ClassUnknown},
		{"checkpoint at a boundary", with(func(in *Input) {
			in.CtxCheckpoints, in.CachedTokens, in.Boundaries = true, 9000, []int{4000, 9100}
		}), ClassCheckpoint},
		{"checkpoint: none below the match", with(func(in *Input) {
			in.CtxCheckpoints, in.CachedTokens, in.MatchTokEst, in.Boundaries = true, 0, 6000, []int{9000, 18000}
		}), ClassCheckpoint},
		{"no --ctx-checkpoints", with(func(in *Input) {
			in.CachedTokens, in.Boundaries = 9000, []int{4000, 9100}
		}), ClassUnknown},
		{"evicted by /metrics", with(func(in *Input) { in.Evictions = 3 }), ClassEvicted},
		{"metrics say no eviction", with(func(in *Input) { in.Evictions, in.OtherTok = 0, 900_000 }), ClassUnknown},
		{"evicted by traffic proxy", with(func(in *Input) { in.OtherTok = 400_000 }), ClassEvicted},
		{"unknown", base, ClassUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.in); got.Class != c.want {
			t.Errorf("%s: class %s (%+v), want %s", c.name, got.Class, got, c.want)
		}
	}
	if v := Classify(base); v.LostTok != 18000 {
		t.Errorf("lost = %d, want min(prev prompt, prompt) - cached = 18000", v.LostTok)
	}
}

func body(t testing.TB, msgs ...string) []byte {
	t.Helper()
	var ms []map[string]string
	for i, m := range msgs {
		role := "user"
		if i == 0 {
			role = "system"
		} else if i%2 == 0 {
			role = "assistant"
		}
		ms = append(ms, map[string]string{"role": role, "content": m})
	}
	b, err := json.Marshal(map[string]any{"messages": ms})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func turnOf(t testing.TB, b []byte) *slotcache.Turn { return declaredTurn(t, "", b) }

// declaredTurn names the conversation, as X-Corrallm-Conversation does: an
// edited system message changes the inferred key, never a declared one.
func declaredTurn(t testing.TB, declared string, b []byte) *slotcache.Turn {
	t.Helper()
	tn, ok := slotcache.Inspect("c", declared, b)
	if !ok {
		t.Fatal("no conversation")
	}
	return &tn
}

type fixedEvictions int

func (f fixedEvictions) EvictionsBetween(string, int64, int64) int { return int(f) }

// The tracker carries a conversation from turn to turn: the second turn is
// measured against the first, a changed system message is a divergence
// located in it, and an instance change is a reload.
func TestTrackerSequence(t *testing.T) {
	tr := NewTracker(0, fixedEvictions(-1))
	sys := strings.Repeat("s", 40_000)
	conv := "task-1"
	obs := func(ts int64, inst string, prompt, cached int, b []byte) store.CacheRow {
		row, ok := tr.Observe(Obs{Served: "swift", Instance: inst, Slot: -1, TS: ts,
			PromptTokens: prompt, CachedTokens: cached, CompletionTokens: 100, Turn: declaredTurn(t, conv, b)})
		if !ok {
			t.Fatal("not observed")
		}
		tr.Committed(row.Conversation, ts, ts) // activity id = ts, for the test
		return row
	}
	r1 := obs(1000, "pgid:1", 10000, 0, body(t, sys, "u1"))
	if r1.Class != ClassCold {
		t.Fatalf("first turn: %s", r1.Class)
	}
	r2 := obs(2000, "pgid:1", 12000, 9990, body(t, sys, "u1", "a1", "u2"))
	if r2.Class != ClassHit || r2.PrevActivityID != 1000 || r2.GapMS != 1000 || r2.PrevCtxTokens != 10100 {
		t.Fatalf("second turn: %+v", r2)
	}
	if r2.OtherTok != 0 {
		t.Errorf("other tokens = %d, want 0 (no other traffic)", r2.OtherTok)
	}
	// Another conversation on the same instance.
	conv = "task-2"
	obs(2500, "pgid:1", 50000, 0, body(t, "other system", "x"))
	conv = "task-1"
	// The client edits the system message at char ~20k: the cache keeps what
	// still matched (~6.8k tokens) and no more.
	edited := sys[:20_000] + "EDIT" + sys[20_004:]
	r3 := obs(3000, "pgid:1", 14000, 6700, body(t, edited, "u1", "a1", "u2", "a2", "u3"))
	if r3.Class != ClassDiverged || r3.Cause != "system" || r3.DivergeMsg != 0 {
		t.Errorf("edited system: %+v", r3)
	}
	if r3.OtherTok != 50100 {
		t.Errorf("other tokens = %d, want 50100", r3.OtherTok)
	}
	r4 := obs(4000, "pgid:2", 15000, 0, body(t, edited, "u1", "a1", "u2", "a2", "u3", "a3", "u4"))
	if r4.Class != ClassReload || r4.Cause != "instance changed" || r4.OtherTok != -1 {
		t.Errorf("new instance: %+v", r4)
	}
}

// A backend start between two turns is a reload even on the same instance
// id, with the stop's reason as the cause; a seeded stub without hashes is
// no-history.
func TestTrackerEventsAndSeed(t *testing.T) {
	tr := NewTracker(0, nil)
	b1 := body(t, strings.Repeat("s", 9000), "u1")
	tn := turnOf(t, b1)
	tr.Seed([]store.LastTurn{{Conversation: tn.Key, Served: "swift", ActivityID: 7, TS: 1000, PromptTokens: 9000, CompletionTokens: 50}}, nil)
	row, _ := tr.Observe(Obs{Served: "swift", Instance: "pgid:1", Slot: -1, TS: 2000, PromptTokens: 9500, CachedTokens: 0, Turn: turnOf(t, body(t, strings.Repeat("s", 9000), "u1", "a", "u2"))})
	if row.Class != ClassNoHistory || row.PrevActivityID != 7 {
		t.Fatalf("seeded: %+v", row)
	}
	tr.NoteEvent(store.BackendEvent{TS: 2500, Model: "swift", Kind: "stop", Reason: "build-swap"})
	tr.NoteEvent(store.BackendEvent{TS: 2600, Model: "swift", Kind: "start", Reason: "cold-spawn"})
	row, _ = tr.Observe(Obs{Served: "swift", Instance: "pgid:1", Slot: -1, TS: 3000, PromptTokens: 9800, CachedTokens: 0, Turn: turnOf(t, body(t, strings.Repeat("s", 9000), "u1", "a", "u2", "b", "u3"))})
	if row.Class != ClassReload || row.Cause != "build-swap" {
		t.Errorf("after a restart: %+v", row)
	}
}

// The LRU cap holds: past it, the least recently seen conversations go.
func TestTrackerLRUCap(t *testing.T) {
	const capBytes = 16 * (4 * 1024) // 4 KiB per shard
	tr := NewTracker(capBytes, nil)
	for i := range 400 {
		b := body(t, strings.Repeat(fmt.Sprint(i), 4000), "u")
		tr.Observe(Obs{Served: "m", Instance: "i", Slot: -1, TS: int64(i), PromptTokens: 1000, Turn: turnOf(t, b)})
	}
	var bytes int64
	for i := range tr.shards {
		bytes += tr.shards[i].bytes
		if tr.shards[i].bytes > tr.capShard && tr.shards[i].lru.Len() > 1 {
			t.Errorf("shard %d holds %d bytes over its %d cap", i, tr.shards[i].bytes, tr.capShard)
		}
	}
	if n := tr.Len(); n >= 400 || n == 0 {
		t.Errorf("held %d conversations; the cap should have dropped most", n)
	}
	// The newest is still there.
	last := turnOf(t, body(t, strings.Repeat(fmt.Sprint(399), 4000), "u"))
	row, _ := tr.Observe(Obs{Served: "m", Instance: "i", Slot: -1, TS: 1000, PromptTokens: 1000, CachedTokens: 990, Turn: last})
	if row.Class == ClassCold {
		t.Error("the newest conversation was dropped")
	}
}

// The tracker's share of the hot path at a 600 KB turn: match the previous
// turn's ~600 block hashes and classify. Inspect's cost is in slotcache.
func BenchmarkObserve600KB(b *testing.B) {
	tr := NewTracker(0, nil)
	msgs := []string{strings.Repeat("s", 30_000)}
	for len(msgs) < 300 {
		msgs = append(msgs, strings.Repeat("m", 2000))
	}
	prev := declaredTurn(b, "c", body(b, msgs...))
	next := declaredTurn(b, "c", body(b, append(msgs, "a", "u")...))
	tr.Observe(Obs{Served: "m", Instance: "i", Slot: -1, TS: 1, PromptTokens: 150000, Turn: prev})
	b.ReportAllocs()
	ts := int64(2)
	for b.Loop() {
		turn := prev
		if ts%2 == 0 {
			turn = next
		}
		tr.Observe(Obs{Served: "m", Instance: "i", Slot: -1, TS: ts, PromptTokens: 150000, CachedTokens: 149000, Turn: turn})
		ts++
	}
}
