package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
	"github.com/iodesystems/corrallm/internal/store"
)

func disabledOverviewCfg() *config.Config {
	return &config.Config{
		Models: map[string]config.Model{
			"dead": {Type: "chat", Quality: 3, Disabled: true},
			"live": {Type: "chat", Quality: 1},
		},
		Lanes: map[string]config.Lane{
			"free": {Members: []config.LaneMember{{Model: "dead"}, {Model: "live"}}},
		},
	}
}

func modelDef(t *testing.T, out *OverviewOutput, name string) ModelDef {
	t.Helper()
	for _, md := range out.Body.Models {
		if md.Name == name {
			return md
		}
	}
	t.Fatalf("model %q missing from overview", name)
	return ModelDef{}
}

// The model stays in the overview — it is still in the config, and the form
// that turns it back on reads from here.
func TestOverviewReportsDisabled(t *testing.T) {
	cfg := disabledOverviewCfg()
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	out, err := h.Overview(context.Background(), &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !modelDef(t, out, "dead").Disabled {
		t.Error("a disabled model must say so")
	}
	if modelDef(t, out, "live").Disabled {
		t.Error("a live model reported as disabled")
	}
}

// THE POINT OF THE WHOLE CHANGE. A backend the operator took out of the setup
// must stop raising things. Both of the two ways it could — the refusal that
// prompted the decision, and a pause set before it — go quiet.
func TestDisabledBackendRaisesNothing(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UnixMilli()
	if err := st.NoteBackendRefusal("dead", 403, now); err != nil {
		t.Fatal(err)
	}
	if err := st.NoteBackendRefusal("live", 401, now); err != nil {
		t.Fatal(err)
	}

	cfg := disabledOverviewCfg()
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg), Store: st}
	ctx := context.Background()

	// A pause set before the model was disabled. It is deliberately NOT
	// cleared — disabling is reversible and enabling must hand back the state
	// the operator left — but it stops being reported.
	if _, err := h.PauseModel(ctx, pauseInput("dead", "", "403 since Sunday")); err != nil {
		t.Fatal(err)
	}
	if !h.Mgr.IsPaused("dead") {
		t.Fatal("precondition: the pause must still exist")
	}

	out, err := h.Overview(ctx, &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if md := modelDef(t, out, "dead"); md.Paused || md.PauseReason != "" {
		t.Errorf("a disabled model still raised a pause card: %+v", md)
	}
	for _, r := range out.Body.Refusing {
		if r.Backend == "dead" {
			t.Error("a disabled backend is still in the refusing list on Overview")
		}
	}
	// The one that is still in the setup keeps its fault — the filter must not
	// have silenced the panel.
	var sawLive bool
	for _, r := range out.Body.Refusing {
		if r.Backend == "live" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Error("a live refusing backend was dropped too")
	}

	// The budgets page reads the same ledger and must agree about what counts.
	q, err := h.QuotaLedger(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range q.Body.Refusing {
		if r.Backend == "dead" {
			t.Error("a disabled backend is still in the refusing list on the budgets page")
		}
	}

	// The RECORD survives. It is the evidence the decision rests on, and
	// enabling the model must bring back the honest answer: this is still the
	// thing that was broken, and since when.
	rows, err := st.RefusingBackends()
	if err != nil {
		t.Fatal(err)
	}
	var kept bool
	for _, r := range rows {
		if r.Backend == "dead" {
			kept = true
		}
	}
	if !kept {
		t.Error("the refusal row was deleted — the API must filter, not falsify the record")
	}
}

// An extension pause covers a PROCESS and therefore several models. Disabling
// one of them must not silence the pause card on its siblings.
func TestDisablingOneModelKeepsItsSiblingsPauseVisible(t *testing.T) {
	cfg, err := config.LoadBytesForTest([]byte(`
extensions:
  oidio:
    cmd: run-oidio
    server: box
    proxy: 5900
    provides:
      stt: {type: stt}
      tts: {type: tts}
servers:
  box:
    pools: {gpu0: 24GB}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["oidio-stt"]
	m.Disabled = true
	cfg.Models["oidio-stt"] = m

	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	ctx := context.Background()
	if _, err := h.PauseModel(ctx, pauseInput("oidio-tts", "", "upgrading")); err != nil {
		t.Fatal(err)
	}
	out, err := h.Overview(ctx, &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if md := modelDef(t, out, "oidio-stt"); md.Paused || !md.Disabled {
		t.Errorf("disabled sibling = %+v, want disabled and silent", md)
	}
	if md := modelDef(t, out, "oidio-tts"); !md.Paused {
		t.Error("disabling one model of an extension silenced its sibling's pause")
	}
}

// The ladder view keeps a declared rung it will not walk, marked — so a dialog
// counting what a change costs counts only rungs that would take the work.
func TestOverviewLadderMarksADisabledRung(t *testing.T) {
	cfg := disabledOverviewCfg()
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	out, err := h.Overview(context.Background(), &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	var ladder []LaneRungDef
	for _, ld := range out.Body.Lanes {
		if ld.Name == "free" {
			ladder = ld.Ladder
		}
	}
	if len(ladder) != 2 {
		t.Fatalf("ladder = %+v, want both rungs listed", ladder)
	}
	if ladder[0].Model != "dead" || !ladder[0].Disabled {
		t.Errorf("rung 0 = %+v, want dead marked disabled", ladder[0])
	}
	if ladder[1].Disabled {
		t.Errorf("rung 1 = %+v, want live", ladder[1])
	}
}

// The flag has to survive the round trip the dashboard makes, or the button
// that sets it cannot be built.
func TestModelSpecRoundTripsDisabled(t *testing.T) {
	spec := ModelSpec{Name: "m", Proxy: "5800", Type: "chat", Quality: 2, Disabled: true}
	m, err := specToModel(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Disabled {
		t.Fatal("specToModel dropped disabled")
	}
	if got := modelToSpec("m", m); !got.Disabled {
		t.Error("modelToSpec dropped disabled")
	}

	// The merge is what a PUT actually does. It must be able to set the flag
	// AND clear it again — a one-way toggle would be a trap.
	prev := config.Model{Type: "chat", ContextPerRequest: 220_000}
	merged := applySpec(prev, m)
	if !merged.Disabled {
		t.Error("applySpec did not set disabled")
	}
	if merged.ContextPerRequest != 220_000 {
		t.Error("applySpec destroyed a field the form does not model")
	}
	spec.Disabled = false
	back, err := specToModel(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if applySpec(merged, back).Disabled {
		t.Error("applySpec could not clear disabled — enabling must work through the same door")
	}
}
