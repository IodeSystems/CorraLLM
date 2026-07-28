package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Cancelling must work from ANY state, including the one that matters most: a
// request already streaming from the backend. That is the case an operator
// actually hits — a runaway generation nobody is waiting for.
func TestCancelInflight_AbortsAStreamingRequest(t *testing.T) {
	p := &Proxy{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	e := &inflightEntry{id: 1, state: inflightStreaming, startedAt: time.Now(), cancel: cancel}
	p.inflight = map[int64]*inflightEntry{1: e}

	if !p.CancelInflight(1) {
		t.Fatal("CancelInflight reported not found for a live request")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the request context was never cancelled")
	}
	// The cause must say an operator did this — distinct from a preemption or a
	// client disconnect, because the three deserve different answers.
	if !errors.Is(context.Cause(ctx), ErrCanceled) {
		t.Errorf("cause = %v, want ErrCanceled", context.Cause(ctx))
	}
}

// A queued request has no slot and no backend, and is exactly the one an
// operator wants to be able to kill. Registration happens before admission so
// that it is addressable.
func TestCancelInflight_AbortsAQueuedRequest(t *testing.T) {
	p := &Proxy{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	p.inflight = map[int64]*inflightEntry{
		7: {id: 7, state: inflightQueued, startedAt: time.Now(), cancel: cancel},
	}
	if !p.CancelInflight(7) {
		t.Fatal("a queued request must be cancellable")
	}
	<-ctx.Done()
}

// Cancelling something that already finished is not an error: between reading
// the list and acting on it, the request may have ended on its own — which is
// the outcome the operator wanted anyway.
func TestCancelInflight_UnknownIDIsNotAnError(t *testing.T) {
	p := &Proxy{}
	if p.CancelInflight(999) {
		t.Error("reported success for an id that is not live")
	}
}

// The retryable header only ever WIDENS interruptibility. A caller must not be
// able to opt out and pin a slot against the operator's policy.
func TestRetryableHeader_ParsesOnlyAffirmatives(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", " true "} {
		if !retryableRequest(reqWithHeader(v)) {
			t.Errorf("%q should mark the request retryable", v)
		}
	}
	// Anything else, including an explicit "false", leaves the group's policy
	// untouched — there is no way to say "do not preempt me".
	for _, v := range []string{"", "false", "0", "no", "maybe"} {
		if retryableRequest(reqWithHeader(v)) {
			t.Errorf("%q should NOT mark the request retryable", v)
		}
	}
}

func reqWithHeader(v string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://x/v1/chat/completions", nil)
	if v != "" {
		r.Header.Set(RetryableHeader, v)
	}
	return r
}
