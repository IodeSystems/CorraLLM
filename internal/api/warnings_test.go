package api

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// The case on the box that produced the control: depth 1, one slot, a model
// slow enough that the box-wide wait fits none. The wait written must make the
// configured depth reachable again — reachable = capacity × wait / mean ≥ depth.
func TestWaitForDepthClearsTheClash(t *testing.T) {
	const mean = 23_400.0
	got := waitForDepthMS(1, 1, mean)
	if got != 24_000 {
		t.Errorf("wait = %d, want 24000 (23.4 s rounded up to a whole second)", got)
	}
	if reach := int(float64(1) * float64(got) / mean); reach < 1 {
		t.Errorf("reachable depth at that wait = %d, still short of 1", reach)
	}
	if waitForDepthMS(4, 2, 10_000) != 20_000 {
		t.Error("two slots halve the wait four waiters need")
	}
	if waitForDepthMS(0, 1, 1000) != 0 || waitForDepthMS(1, 0, 1000) != 0 || waitForDepthMS(1, 1, 0) != 0 {
		t.Error("an uncomputable case must report 0, not a number")
	}
}

// Setting the wait keeps a depth set beside it; clearing it keeps the depth
// too, and removes the override only when nothing is left in it.
func TestSetModelMaxWaitKeepsTheDepth(t *testing.T) {
	c := providerCfg()
	if err := editAuthoredModel(c, "local-Qwen3.8-27B", func(m *config.Model) {
		m.Scheduler = &config.SchedulerConfig{MaxQueueDepth: 1}
	}); err != nil {
		t.Fatal(err)
	}
	if err := editAuthoredModel(c, "local-Qwen3.8-27B", func(m *config.Model) { setModelMaxWait(m, "30s") }); err != nil {
		t.Fatal(err)
	}
	if got := c.SchedulerFor("local-Qwen3.8-27B"); got.MaxWait != "30s" || got.MaxQueueDepth != 1 {
		t.Errorf("resolved = %+v, want 30s and depth 1", got)
	}
	if c.Scheduler.MaxWait != "15s" {
		t.Errorf("box-wide wait changed to %q", c.Scheduler.MaxWait)
	}
	authored := c.Providers["local"].Models["Qwen3.8-27B"]
	if authored.Scheduler == nil || authored.Scheduler.MaxWait != "30s" {
		t.Errorf("authored = %+v, want the wait written under the provider", authored.Scheduler)
	}

	_ = editAuthoredModel(c, "local-Qwen3.8-27B", func(m *config.Model) { setModelMaxWait(m, "") })
	if got := c.SchedulerFor("local-Qwen3.8-27B"); got.MaxWait != "15s" || got.MaxQueueDepth != 1 {
		t.Errorf("after clear = %+v, want the inherited 15s and depth 1 kept", got)
	}
	m := c.Models["local-Qwen3.8-27B"]
	m.Scheduler.MaxQueueDepth = 0
	setModelMaxWait(&m, "")
	if m.Scheduler != nil {
		t.Error("an override with nothing left in it survived")
	}
}

func TestSuppressWarningDefaultsToADay(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Handlers{Store: st}

	in := &SuppressWarningInput{}
	in.Body.Kind, in.Body.Subject = WarnQueueClash, "local-Qwen3.8-27B"
	if _, err := h.SuppressWarning(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	out, err := h.WarningPrefs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Body.Prefs) != 1 {
		t.Fatalf("prefs = %+v", out.Body.Prefs)
	}
	p := out.Body.Prefs[0]
	day := time.Now().Add(24 * time.Hour).UnixMilli()
	if !p.Suppressed || p.SuppressedForever || p.SuppressedUntil < day-60_000 || p.SuppressedUntil > day+60_000 {
		t.Errorf("pref = %+v, want suppressed for about a day", p)
	}

	in.Body.Lift = true
	if _, err := h.SuppressWarning(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if out, _ := h.WarningPrefs(context.Background(), nil); len(out.Body.Prefs) != 0 {
		t.Errorf("lifted suppression still listed: %+v", out.Body.Prefs)
	}
}

func TestWarningKindRules(t *testing.T) {
	h := &Handlers{}
	bad := []struct{ kind, subject string }{
		{"nope", ""},
		{WarnQueueClash, ""},           // per-model kind with no model
		{WarnTurnedAway, "some-model"}, // box-wide kind with a model
	}
	for _, b := range bad {
		in := &SuppressWarningInput{}
		in.Body.Kind, in.Body.Subject = b.kind, b.subject
		if _, err := h.SuppressWarning(context.Background(), in); err == nil {
			t.Errorf("suppress %q/%q was accepted", b.kind, b.subject)
		}
	}
	th := &SetWarningThresholdInput{}
	th.Body.Kind, th.Body.Threshold = WarnQueueClash, 3
	if _, err := h.SetWarningThreshold(context.Background(), th); err == nil {
		t.Error("queue-clash took a threshold; its tolerance is maxWait")
	}
	th.Body.Kind, th.Body.Threshold = WarnSlowStretch, 1
	if _, err := h.SetWarningThreshold(context.Background(), th); err == nil {
		t.Error("a slow-stretch multiple of 1 was accepted")
	}
}
