package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeActive serves /api/v1/active. readyAt (unix nanos, 0 = still queued)
// says when this bench's request reached the backend; before that it is
// queued. A noise row from another caller and another model is always present.
type fakeActive struct {
	readyAt atomic.Int64
	fail    atomic.Bool
	none    atomic.Bool
}

func (f *fakeActive) client(t *testing.T) *overheadClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/active" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		noise := `{"served":"m","key":"sk-other","state":"queued","stateMs":1}` +
			`,{"served":"other-model","key":"bench-k","state":"loading","stateMs":1}`
		mine := ""
		switch at := f.readyAt.Load(); {
		case f.none.Load():
		case at == 0:
			mine = `,{"served":"m","key":"bench-k","state":"queued","stateMs":5}`
		default:
			ms := time.Since(time.Unix(0, at)).Milliseconds()
			mine = fmt.Sprintf(`,{"served":"m","key":"bench-k","state":"streaming","stateMs":%d}`, ms)
		}
		_, _ = fmt.Fprintf(w, `{"requests":[%s%s]}`, noise, mine)
	}))
	t.Cleanup(srv.Close)
	return &overheadClient{base: srv.URL, token: "tok", key: "bench-k", http: srv.Client()}
}

func fastStall(t *testing.T, stall time.Duration) {
	t.Helper()
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, stall
	t.Cleanup(func() { watchdogTick, stallTimeout = old, oldStall })
}

// A request corrallm still holds in its queue is not model silence, however
// long it waits. Live run 20261020-223350 aborted 12 stages this way.
func TestWatchdogSparesARequestQueuedInCorrallm(t *testing.T) {
	fastStall(t, 40*time.Millisecond)
	f := &fakeActive{}
	hb := &heartbeat{}
	hb.open()
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, f.client(t), "m", hb.idleFor)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatalf("queued request aborted: cause = %v", context.Cause(ctx))
	case <-time.After(400 * time.Millisecond): // 10x the stall timeout
	}
}

// Silence after the request reaches the backend still counts, from that moment:
// a backend that accepts and never streams is aborted stallTimeout after
// admission, not stallTimeout after the request was sent.
func TestWatchdogCountsSilenceFromAdmission(t *testing.T) {
	fastStall(t, 100*time.Millisecond)
	f := &fakeActive{}
	hb := &heartbeat{}
	hb.open()
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, f.client(t), "m", hb.idleFor)
	defer cancel()

	time.Sleep(300 * time.Millisecond) // queued 3x the stall timeout
	if ctx.Err() != nil {
		t.Fatalf("aborted while queued: %v", context.Cause(ctx))
	}
	admitted := time.Now()
	f.readyAt.Store(admitted.UnixNano())
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a backend that never streams was not aborted")
	}
	if !errors.Is(context.Cause(ctx), errStalled) {
		t.Fatalf("cause = %v, want errStalled", context.Cause(ctx))
	}
	if after := time.Since(admitted); after < 100*time.Millisecond {
		t.Fatalf("aborted %s after admission, before the stall timeout", after)
	}
}

// Nothing in flight in corrallm: the silence is the bench's own, and is a stall.
func TestWatchdogAbortsWhenCorrallmHoldsNothing(t *testing.T) {
	fastStall(t, 40*time.Millisecond)
	f := &fakeActive{}
	f.none.Store(true)
	hb := &heartbeat{}
	hb.open()
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, f.client(t), "m", hb.idleFor)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("silence with nothing in flight was not aborted")
	}
	if !errors.Is(context.Cause(ctx), errStalled) {
		t.Fatalf("cause = %v, want errStalled", context.Cause(ctx))
	}
}

// A failed lookup keeps the old behavior instead of disabling the guard.
func TestWatchdogFailedLookupStillAborts(t *testing.T) {
	fastStall(t, 40*time.Millisecond)
	f := &fakeActive{}
	f.fail.Store(true)
	hb := &heartbeat{}
	hb.open()
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, f.client(t), "m", hb.idleFor)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a failed lookup disabled the stall guard")
	}
}

// Live filters by this bench's key and the model; other callers' and other
// models' requests say nothing about this combo.
func TestLiveFiltersByKeyAndModel(t *testing.T) {
	f := &fakeActive{}
	f.none.Store(true)
	st, err := f.client(t).Live(context.Background(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if st.Waiting || st.Streaming {
		t.Fatalf("foreign rows counted: %+v", st)
	}
}
