package proc

import (
	"context"
	"testing"
)

type capturedLoads struct{ events []ModelLoadEvent }

func (c *capturedLoads) RecordLoad(ev ModelLoadEvent) { c.events = append(c.events, ev) }

// The requester rides the context because it is descriptive, not operative:
// nothing about the load depends on who asked. It must survive the trip and an
// absent one must stay absent — a preload has no caller, and inventing one
// would put a name on work nobody requested.
func TestRequesterRidesTheContext(t *testing.T) {
	ctx := WithRequester(context.Background(), "sk-aw4")
	if got := requesterFrom(ctx); got != "sk-aw4" {
		t.Errorf("got %q, want sk-aw4", got)
	}
	if got := requesterFrom(context.Background()); got != "" {
		t.Errorf("a bare context reported requester %q", got)
	}
	// An empty key must not put an empty value in the context either — the
	// absence and the empty string mean the same thing and should stay one case.
	if got := requesterFrom(WithRequester(context.Background(), "")); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if requesterFrom(nil) != "" {
		t.Error("a nil context should report no requester rather than panic")
	}
}

// A Manager with no recorder must behave exactly as before — that is what keeps
// every existing test and the agent path unchanged.
func TestNoRecorderIsSafe(t *testing.T) {
	m := &Manager{}
	m.recordLoad(ModelLoadEvent{Model: "m"}) // must not panic
	c := &capturedLoads{}
	m.SetLoadRecorder(c)
	m.recordLoad(ModelLoadEvent{Model: "m", OK: true})
	if len(c.events) != 1 || c.events[0].Model != "m" {
		t.Fatalf("events = %+v, want the one we recorded", c.events)
	}
	m.SetLoadRecorder(nil)
	m.recordLoad(ModelLoadEvent{Model: "m2"})
	if len(c.events) != 1 {
		t.Error("recording continued after the recorder was removed")
	}
}
