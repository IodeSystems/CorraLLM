package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proc"
)

func pauseInput(model, resumeAt, reason string) *PauseModelInput {
	in := &PauseModelInput{}
	in.Body.Model, in.Body.ResumeAt, in.Body.Reason = model, resumeAt, reason
	return in
}

// TestPauseModelHandler covers the op's result mapping: an unknown model and a
// malformed/past resumeAt come back as OK:false + message (the convention for
// this control surface), and a good pause round-trips through Unpause.
func TestPauseModelHandler(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Type: "local"}}}
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	ctx := context.Background()

	if out, err := h.PauseModel(ctx, pauseInput("nope", "", "")); err != nil {
		t.Fatal(err)
	} else if out.Body.OK || !strings.Contains(out.Body.Message, "unknown") {
		t.Errorf("pause unknown = %+v", out.Body)
	}

	if out, err := h.PauseModel(ctx, pauseInput("m", "tomorrow-ish", "")); err != nil {
		t.Fatal(err)
	} else if out.Body.OK || !strings.Contains(out.Body.Message, "bad resumeAt") {
		t.Errorf("pause with unparseable resumeAt = %+v", out.Body)
	}

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if out, err := h.PauseModel(ctx, pauseInput("m", past, "")); err != nil {
		t.Fatal(err)
	} else if out.Body.OK || !strings.Contains(out.Body.Message, "not in the future") {
		t.Errorf("pause with a past resumeAt = %+v", out.Body)
	}

	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	out, err := h.PauseModel(ctx, pauseInput("m", future, "gpu needed"))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.OK {
		t.Fatalf("pause = %+v", out.Body)
	}
	if !h.Mgr.IsPaused("m") {
		t.Fatal("model is not paused after a successful pause")
	}

	up, err := h.UnpauseModel(ctx, actionInput("m"))
	if err != nil {
		t.Fatal(err)
	}
	if !up.Body.OK || !strings.Contains(up.Body.Message, "resumed") {
		t.Errorf("unpause = %+v", up.Body)
	}
	if h.Mgr.IsPaused("m") {
		t.Error("model still paused after unpause")
	}

	// Unpausing something that was not paused is a success, not an error — the
	// operator got the state they asked for.
	if up, err := h.UnpauseModel(ctx, actionInput("m")); err != nil {
		t.Fatal(err)
	} else if !up.Body.OK || !strings.Contains(up.Body.Message, "was not paused") {
		t.Errorf("redundant unpause = %+v", up.Body)
	}
}

// TestOverviewReportsPause: a paused model has no process, so its pause has to
// ride on the model definition — the residency snapshot cannot carry it.
func TestOverviewReportsPause(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Type: "local"}}}
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	ctx := context.Background()

	resume := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := h.PauseModel(ctx, pauseInput("m", resume.Format(time.RFC3339), "maintenance")); err != nil {
		t.Fatal(err)
	}

	out, err := h.Overview(ctx, &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, md := range out.Body.Models {
		if md.Name != "m" {
			continue
		}
		found = true
		if !md.Paused {
			t.Error("paused = false")
		}
		if md.PauseReason != "maintenance" {
			t.Errorf("pauseReason = %q", md.PauseReason)
		}
		if md.PauseResumeMS != resume.UnixMilli() {
			t.Errorf("pauseResumeMs = %d, want %d", md.PauseResumeMS, resume.UnixMilli())
		}
		if md.PausedAtMS == 0 {
			t.Error("pausedAtMs = 0")
		}
		if md.PauseScope != "model" || md.PausedByExtension != "" {
			t.Errorf("scope = %q/%q, want model/\"\"", md.PauseScope, md.PausedByExtension)
		}
	}
	if !found {
		t.Fatal("model m missing from overview")
	}
}

// TestPauseExtensionHandler: pausing an extension-hosted model reports the
// WIDER blast radius in its message and target, so an operator who clicked
// "pause oidio-tts" learns that all four audio models went with it.
func TestPauseExtensionHandler(t *testing.T) {
	cfg := &config.Config{
		Extensions: map[string]config.Extension{"ext": {Cmd: "true"}},
		Models: map[string]config.Model{
			"x": {Extension: "ext", ExtensionHosted: true, Type: "local"},
			"y": {Extension: "ext", ExtensionHosted: true, Type: "local"},
		},
	}
	h := &Handlers{Cfg: cfg, Mgr: proc.NewManager(cfg)}
	ctx := context.Background()

	out, err := h.PauseModel(ctx, pauseInput("x", "", "gpu"))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Body.OK {
		t.Fatalf("pause = %+v", out.Body)
	}
	if out.Body.Target != "extension:ext" {
		t.Errorf("target = %q, want extension:ext", out.Body.Target)
	}
	if len(out.Body.Affected) != 2 {
		t.Errorf("affected = %v, want both models", out.Body.Affected)
	}
	if !strings.Contains(out.Body.Message, "paused extension") || !strings.Contains(out.Body.Message, "hosted by it") {
		t.Errorf("message does not name the wider blast radius: %q", out.Body.Message)
	}

	// The sibling reports paused, and says WHY — via the extension, not on its own.
	ov, err := h.Overview(ctx, &OverviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range ov.Body.Models {
		if !md.Paused {
			t.Errorf("%s should be paused via its extension", md.Name)
			continue
		}
		if md.PauseScope != "extension" || md.PausedByExtension != "ext" {
			t.Errorf("%s scope = %q/%q, want extension/ext", md.Name, md.PauseScope, md.PausedByExtension)
		}
	}

	// Addressing the extension directly works too, and resumes everything.
	ei := &ExtensionActionInput{}
	ei.Body.Extension = "ext"
	if up, err := h.UnpauseExtension(ctx, ei); err != nil {
		t.Fatal(err)
	} else if !up.Body.OK || !strings.Contains(up.Body.Message, "resumed") {
		t.Errorf("unpause extension = %+v", up.Body)
	}
	if h.Mgr.IsPaused("x") || h.Mgr.IsPaused("y") {
		t.Error("both models should be back in service")
	}

	pe := &PauseExtensionInput{}
	pe.Body.Extension = "nope"
	if out, err := h.PauseExtension(ctx, pe); err != nil {
		t.Fatal(err)
	} else if out.Body.OK || !strings.Contains(out.Body.Message, "unknown") {
		t.Errorf("pause unknown extension = %+v", out.Body)
	}
}

// TestResidencyExposesStopping: the residency op forwards the stopping set, so
// the dashboard can disable Load instead of offering one that always fails.
func TestResidencyExposesStopping(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Type: "local"}}}
	mgr := proc.NewManager(cfg)
	h := &Handlers{Cfg: cfg, Mgr: mgr}

	out, err := h.Residency(context.Background(), &ResidencyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Stopping) != 0 {
		t.Errorf("Stopping = %v, want empty", out.Body.Stopping)
	}
}
