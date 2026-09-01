package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/sched"
	"github.com/iodesystems/corrallm/internal/store"
	"gopkg.in/yaml.v3"
)

// TestUtilization: the row set is what was ASKED FOR in the window (not the
// catalog), promise outcomes are counted per model, and the measured wait
// excludes both instant admissions and rejections.
func TestUtilization(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	rows := []store.Activity{
		// big: two queued-then-admitted requests (200ms, 800ms) → mean 500ms.
		{TS: now - 300_000, Served: "big", Status: 200, QueuedMS: 200},
		{TS: now - 290_000, Served: "big", Status: 200, QueuedMS: 800},
		// Admitted instantly: excluded, or a quiet hour drags the mean to zero.
		{TS: now - 280_000, Served: "big", Status: 200, QueuedMS: 0},
		// A rejection's queued_ms measures waiting before being turned AWAY —
		// a different quantity, and huge. Must not pollute the mean.
		{TS: now - 270_000, Served: "big", Status: 429,
			Error: "queue-timeout", QueuedMS: 60_000, RetryAfterMS: 5000},
		{TS: now - 265_000, Served: "big", Status: 200}, // the return → honored
		// gone: promised 3s long ago, never returned.
		{TS: now - 200_000, Served: "big", Key: "ghost", Status: 429,
			Error: "exhausted", RetryAfterMS: 3000},
		// small: called, never queued, never refused.
		{TS: now - 100_000, Served: "small", Status: 200, QueuedMS: 0},
	}
	for _, a := range rows {
		if err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{Models: map[string]config.Model{"big": {}, "small": {}, "never-called": {}}}
	h := &Handlers{Store: st, Sched: sched.New(), Cfg: cfg}

	out, err := h.Utilization(context.Background(), &UtilizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]UtilizationRow{}
	for _, r := range out.Body.Rows {
		got[r.Served] = r
	}
	// A model nobody called is absent, not a zero row.
	if _, ok := got["never-called"]; ok {
		t.Errorf("never-called must not appear: %+v", out.Body.Rows)
	}
	if len(out.Body.Rows) != 2 {
		t.Fatalf("want rows for big+small only, got %d: %+v", len(out.Body.Rows), out.Body.Rows)
	}

	big := got["big"]
	if big.RealWaitMS != 500 || big.MaxWaitMS != 800 || big.QueuedSamples != 2 {
		t.Errorf("big wait = mean %d / max %d / n %d; want 500 / 800 / 2 (instant admits and the 429 excluded)",
			big.RealWaitMS, big.MaxWaitMS, big.QueuedSamples)
	}
	if big.Turned != 2 {
		t.Errorf("big turned away = %d, want 2", big.Turned)
	}
	if big.NotHonored != 1 {
		t.Errorf("big notHonored = %d, want 1 (the ghost)", big.NotHonored)
	}
	if big.Promised != 0 {
		t.Errorf("big promised = %d, want 0 (both promises are long past due)", big.Promised)
	}

	// small queued for nothing and was never refused: a real row, all zeros.
	small := got["small"]
	if small.QueuedSamples != 0 || small.Turned != 0 || small.RealWaitMS != 0 {
		t.Errorf("small = %+v; want an all-zero row", small)
	}

	// Busiest first. With no live load, the tiebreak is who was turned away most.
	if out.Body.Rows[0].Served != "big" {
		t.Errorf("want big first (2 turned away), got %s", out.Body.Rows[0].Served)
	}
	if out.Body.Minutes != 60 {
		t.Errorf("minutes = %d, want the 60 default", out.Body.Minutes)
	}
}

// TestUtilizationLaneMembers: a lane row carries its rungs in fall-through
// order, each tagged with whether it can take work — loaded, loadable, remote,
// or shut out by a pause. Without this the lane chip could only say "this row
// is an aggregate", which is not the question a lane raises.
func TestUtilizationLaneMembers(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	if err := st.InsertActivity(store.Activity{TS: now - 60_000, Served: "chat", Status: 200}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Models: map[string]config.Model{
			// Spawnable, never loaded: the cold rung a caller pays a spawn for.
			"local-a": {Cmd: "run-a", Server: "box1"},
			// Paused: still spawnable on paper, but the walk skips it.
			"local-b": {Cmd: "run-b", Server: "box1"},
			// Remote: holds no residency, so it reports no state at all.
			"remote-c": {Proxy: hostNode("https://api.example.com")},
		},
		Lanes: map[string]config.Lane{
			"chat": {Members: []config.LaneMember{{Model: "local-a"}, {Model: "local-b"}, {Model: "remote-c"}}},
		},
	}
	mgr := proc.NewManager(cfg)
	h := &Handlers{Store: st, Sched: sched.New(), Cfg: cfg, Mgr: mgr}
	if out, err := h.PauseModel(context.Background(), pauseInput("local-b", "", "gpu needed")); err != nil {
		t.Fatal(err)
	} else if !out.Body.OK {
		t.Fatalf("pause local-b = %+v", out.Body)
	}

	out, err := h.Utilization(context.Background(), &UtilizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	var lane UtilizationRow
	for _, r := range out.Body.Rows {
		if r.Served == "chat" {
			lane = r
		}
	}
	if !lane.Lane {
		t.Fatalf("chat is not marked a lane: %+v", out.Body.Rows)
	}
	if len(lane.Members) != 3 {
		t.Fatalf("members = %+v, want 3 rungs", lane.Members)
	}
	// Fall-through order, not map order: the first rung is the one tried first.
	if lane.Members[0].Model != "local-a" || lane.Members[2].Model != "remote-c" {
		t.Errorf("members out of ladder order: %+v", lane.Members)
	}
	if a := lane.Members[0]; a.State != "absent" || !a.Spawnable || a.Remote || a.Paused {
		t.Errorf("local-a = %+v; want an absent, spawnable, unpaused local rung", a)
	}
	if b := lane.Members[1]; !b.Paused || b.PauseReason != "gpu needed" {
		t.Errorf("local-b = %+v; want paused with the operator's reason", b)
	}
	if c := lane.Members[2]; !c.Remote || c.State != "" || c.Spawnable {
		t.Errorf("remote-c = %+v; want remote with no residency state", c)
	}
}

// hostNode builds the scalar `proxy:` node a pure-proxy model carries.
func hostNode(s string) yaml.Node {
	var n yaml.Node
	n.SetString(s)
	return n
}

// TestUtilizationLaneDedupesCandidatesByName: a provider holding several
// credentials expands ONE served name into one candidate per account. That is a
// routing fact — which key pays — and not a second rung of the ladder.
//
// Listed raw, the same model appears twice with one residency between them, so
// a lane of one model reads "0/2 ready" and the chip's own count contradicts
// what is loaded.
func TestUtilizationLaneDedupesCandidatesByName(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now().UnixMilli()
	if err := st.InsertActivity(store.Activity{TS: now - 60_000, Served: "chat", Status: 200}); err != nil {
		t.Fatal(err)
	}

	// Two accounts on one provider, then a lane over the single model they
	// serve. Loaded through the real resolution path — Validate alone leaves the
	// provided models unmaterialised, which would assert nothing.
	cfg, err := config.LoadBytesForTest([]byte(`
extensions:
  free:
    providers:
      openrouter:
        proxy: {host: openrouter.ai, port: 443, basePath: /api}
        provides:
          m: {type: chat, upstream: vendor/m}
        credentials:
          - name: personal
            headers: {authorization: "Bearer P"}
          - name: work
            headers: {authorization: "Bearer W"}
lanes:
  chat:
    members:
      - model: openrouter-m
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Guard the premise: without two candidates this test proves nothing.
	cands, ok := cfg.ResolveServed("openrouter-m")
	if !ok || len(cands) != 2 {
		t.Fatalf("fixture did not expand across credentials: ok=%v cands=%d", ok, len(cands))
	}

	mgr := proc.NewManager(cfg)
	h := &Handlers{Store: st, Sched: sched.New(), Cfg: cfg, Mgr: mgr}
	out, err := h.Utilization(context.Background(), &UtilizationInput{})
	if err != nil {
		t.Fatal(err)
	}
	var lane UtilizationRow
	for _, r := range out.Body.Rows {
		if r.Served == "chat" {
			lane = r
		}
	}
	if !lane.Lane {
		t.Fatalf("chat is not marked a lane: %+v", out.Body.Rows)
	}
	if len(lane.Members) != 1 {
		t.Fatalf("members = %+v; two credentials on one model must list ONE rung", lane.Members)
	}
	if lane.Members[0].Model != "openrouter-m" {
		t.Errorf("member = %q, want openrouter-m", lane.Members[0].Model)
	}
}
