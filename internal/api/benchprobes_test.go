package api

import (
	"context"
	"testing"

	"github.com/iodesystems/corrallm/internal/store"
)

func probeHandlers(t *testing.T) (*Handlers, context.Context) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Handlers{Store: st}, ctx
}

func publish(t *testing.T, h *Handlers, ctx context.Context, runID string, recs ...BenchProbePublish) {
	t.Helper()
	in := &BenchProbeResultsInput{}
	in.Body.RunID = runID
	in.Body.At = 100
	in.Body.Results = recs
	out, err := h.PublishBenchProbeResults(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.OK {
		t.Fatalf("publish failed: %s", out.Body.Message)
	}
}

// The regression this whole feature exists for: an STT model's audio probes and
// a chat model's chat probes must not land in one pass rate, because the STT
// model then reads as "as good as the chat model" at chatting.
func TestBenchProbeDetail_GroupsByCapability(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "m", Probe: "stt-1", Capability: "audio.stt", Stages: 2, StagesPassed: 2, Pass: true},
		BenchProbePublish{Model: "m", Probe: "stt-2", Capability: "audio.stt", Stages: 2, StagesPassed: 2, Pass: true},
		BenchProbePublish{Model: "m", Probe: "chat-1", Capability: "chat", Stages: 4, StagesPassed: 1},
	)
	out, err := h.BenchProbeDetail(ctx, &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Capabilities) != 2 {
		t.Fatalf("want one group per capability, got %+v", out.Body.Capabilities)
	}
	byCap := map[string]BenchCapabilityView{}
	for _, c := range out.Body.Capabilities {
		byCap[c.Capability] = c
	}
	if got := byCap["audio.stt"].Score; got != 1 {
		t.Errorf("audio.stt should score 4/4 on its own probes, got %v", got)
	}
	if got := byCap["chat"].Score; got != 0.25 {
		t.Errorf("chat should score 1/4 on its own probes, got %v", got)
	}
	// The aggregate this replaces would have reported 5/8 = 62.5% overall,
	// which describes neither surface.
	if len(byCap["audio.stt"].Probes) != 2 {
		t.Errorf("probe detail must survive grouping: %+v", byCap["audio.stt"].Probes)
	}
}

// A skipped probe is a configuration fact, not a score. Counting it as a
// failure invents a capability gap; counting it as a pass invents a capability.
func TestBenchProbeDetail_SkippedProbesScoreNeitherWay(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "stt", Probe: "stt-1", Capability: "audio.stt", Stages: 2, StagesPassed: 2, Pass: true},
		BenchProbePublish{Model: "stt", Probe: "chat-1", Capability: "chat", Skipped: true,
			SkipReason: `probe needs capability "chat", model serves "audio.stt"`},
		BenchProbePublish{Model: "stt", Probe: "chat-2", Capability: "chat", Skipped: true,
			SkipReason: `probe needs capability "chat", model serves "audio.stt"`},
	)
	out, err := h.BenchProbeDetail(ctx, &BenchProbesInput{Model: "stt"})
	if err != nil {
		t.Fatal(err)
	}
	byCap := map[string]BenchCapabilityView{}
	for _, c := range out.Body.Capabilities {
		byCap[c.Capability] = c
	}
	chat, ok := byCap["chat"]
	if !ok {
		t.Fatal("the chat group must still appear, so the console can say 'not applicable' rather than leave a silent hole")
	}
	if chat.Stages != 0 || chat.StagesPassed != 0 || chat.Score != 0 {
		t.Errorf("skipped probes must not enter the score: %+v", chat)
	}
	if chat.SkippedProbes != 2 {
		t.Errorf("want 2 skipped probes recorded, got %d", chat.SkippedProbes)
	}
	if got := byCap["audio.stt"].Score; got != 1 {
		t.Errorf("audio.stt score unaffected by skips elsewhere, got %v", got)
	}
}

func TestBenchProbeDetail_DefaultsToLatestRun(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1", BenchProbePublish{Model: "m", Probe: "p", Capability: "chat", Stages: 2, StagesPassed: 2, Pass: true})
	in := &BenchProbeResultsInput{}
	in.Body.RunID = "r2"
	in.Body.At = 200
	in.Body.Results = []BenchProbePublish{{Model: "m", Probe: "p", Capability: "chat", Stages: 2, StagesPassed: 0}}
	if _, err := h.PublishBenchProbeResults(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchProbeDetail(ctx, &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.RunID != "r2" {
		t.Fatalf("want the newest run, got %q", out.Body.RunID)
	}
	if out.Body.Capabilities[0].Score != 0 {
		t.Errorf("a regression must not be averaged against the run before it: %+v", out.Body.Capabilities[0])
	}
}

// An empty capability on a row means the probe predates the field; it drives a
// chat session, matching Requires.EffectiveCapability.
func TestBenchProbeDetail_BlankCapabilityIsChat(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1", BenchProbePublish{Model: "m", Probe: "p", Stages: 1, StagesPassed: 1, Pass: true})
	out, err := h.BenchProbeDetail(ctx, &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Capabilities) != 1 || out.Body.Capabilities[0].Capability != "chat" {
		t.Fatalf("want chat, got %+v", out.Body.Capabilities)
	}
}

func TestBenchProbeDetail_NoStoreIsEmptyNotNil(t *testing.T) {
	h := &Handlers{}
	out, err := h.BenchProbeDetail(context.Background(), &BenchProbesInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Capabilities == nil {
		t.Error("capabilities must serialize as [] not null")
	}
}

// The cross-model ranking bug: an STT model that passed its 4 audio probes
// outranked a chat model that passed 18 of 20, because the flat table compared
// pass rates computed over different probe sets.
func TestBenchCapabilityMatrix_RanksOnlyWithinACapability(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "stt", Probe: "stt-1", Capability: "audio.stt", Stages: 4, StagesPassed: 4, Pass: true},
		BenchProbePublish{Model: "stt", Probe: "chat-1", Capability: "chat", Skipped: true, SkipReason: "wrong capability"},
		BenchProbePublish{Model: "qwen", Probe: "chat-1", Capability: "chat", Stages: 20, StagesPassed: 18},
	)
	out, err := h.BenchCapabilityMatrix(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byCap := map[string][]BenchCapabilityModelView{}
	for _, c := range out.Body.Capabilities {
		byCap[c.Capability] = c.Models
	}
	if len(byCap["chat"]) != 1 || byCap["chat"][0].Model != "qwen" {
		t.Fatalf("only qwen was measured on chat: %+v", byCap["chat"])
	}
	// The STT model skipped every chat probe, so it must not appear in the chat
	// ranking at all — listing it at 0%% would assert a failure that never ran.
	if len(byCap["audio.stt"]) != 1 || byCap["audio.stt"][0].Model != "stt" {
		t.Fatalf("stt should rank under audio.stt only: %+v", byCap["audio.stt"])
	}
	if byCap["audio.stt"][0].Score != 1 || byCap["chat"][0].Score != 0.9 {
		t.Errorf("scores should be within-capability: %+v", byCap)
	}
}

func TestBenchCapabilityMatrix_BestFirstThenStableByName(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "r1",
		BenchProbePublish{Model: "b", Probe: "p", Capability: "chat", Stages: 10, StagesPassed: 5},
		BenchProbePublish{Model: "a", Probe: "p", Capability: "chat", Stages: 10, StagesPassed: 5},
		BenchProbePublish{Model: "c", Probe: "p", Capability: "chat", Stages: 10, StagesPassed: 9},
	)
	out, err := h.BenchCapabilityMatrix(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range out.Body.Capabilities[0].Models {
		got = append(got, m.Model)
	}
	// Ties break by name so map iteration cannot reorder the ranking per request.
	if len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Errorf("want [c a b], got %v", got)
	}
}

// Models are benched at different times; scoping to one run id would drop every
// model that was not in the newest run and show it as having no data.
func TestBenchCapabilityMatrix_UsesEachModelsOwnLatestRun(t *testing.T) {
	h, ctx := probeHandlers(t)
	publish(t, h, ctx, "old", BenchProbePublish{Model: "a", Probe: "p", Capability: "chat", Stages: 2, StagesPassed: 2, Pass: true})
	in := &BenchProbeResultsInput{}
	in.Body.RunID = "new"
	in.Body.At = 500
	in.Body.Results = []BenchProbePublish{{Model: "b", Probe: "p", Capability: "chat", Stages: 2, StagesPassed: 1}}
	if _, err := h.PublishBenchProbeResults(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := h.BenchCapabilityMatrix(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Capabilities[0].Models) != 2 {
		t.Fatalf("both models should appear despite different runs: %+v", out.Body.Capabilities[0].Models)
	}
}

func TestBenchCapabilityMatrix_NoStoreIsEmptyNotNil(t *testing.T) {
	out, err := (&Handlers{}).BenchCapabilityMatrix(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Capabilities == nil {
		t.Error("capabilities must serialize as [] not null")
	}
}
