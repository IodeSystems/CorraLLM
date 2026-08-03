package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func shortTick(t *testing.T) {
	t.Helper()
	prev := watchdogTick
	watchdogTick = 20 * time.Millisecond
	t.Cleanup(func() { watchdogTick = prev })
}

// A combo that is genuinely stuck must still be cancelled — that is the whole
// reason the watchdog exists (an observed 87-minute hang).
func TestExecBudgetCancelsAHang(t *testing.T) {
	shortTick(t)
	ctx, cancel := execBudgetContext(context.Background(), 50*time.Millisecond, nil, "m1", nil)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errExecBudget) {
			t.Errorf("cause = %v, want errExecBudget", context.Cause(ctx))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung combo was never cancelled")
	}
}

// The point of the change: time spent parked on backpressure must not spend the
// budget. Before this, a shared run queueing for 73% of its wall clock had under
// a third of its budget left for work, and stages died for being polite.
func TestExecBudgetExcludesBackpressureWait(t *testing.T) {
	shortTick(t)
	ctx, cancel := execBudgetContext(context.Background(), 200*time.Millisecond, nil, "m1", nil)
	defer cancel()

	// The wait has to accrue DURING the combo — only waiting this combo did
	// belongs to it, which is why stageQueueWait reads a delta and not a total.
	// A 429 loop parking the process looks exactly like this.
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(20 * time.Millisecond)
			queueWaitNS.Add(int64(20 * time.Millisecond))
		}
	}()

	// Far past the budget on the wall, but nothing has executed.
	select {
	case <-ctx.Done():
		t.Fatalf("cancelled while only waiting: cause = %v", context.Cause(ctx))
	case <-time.After(600 * time.Millisecond):
	}
}

// The caller's own cancel must not be reported as a spent budget, or every
// cancelled run would blame the model's pace.
func TestCallerCancelIsNotReportedAsBudget(t *testing.T) {
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m1", nil)
	cancel()
	<-ctx.Done()
	if errors.Is(context.Cause(ctx), errExecBudget) {
		t.Error("caller cancellation was reported as an execution-budget overrun")
	}
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Errorf("cause = %v, want context.Canceled", context.Cause(ctx))
	}
}

// A cancelled parent must still stop the combo.
func TestParentCancelPropagates(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := execBudgetContext(parent, time.Hour, nil, "m1", nil)
	defer cancel()
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancellation did not propagate")
	}
}

// classifyErr must name the execution budget rather than falling back to the
// generic "parent context cancelled", which would send a reader after a wall
// clock that had plenty left.
func TestClassifyErrNamesTheExecutionBudget(t *testing.T) {
	shortTick(t)
	ctx, cancel := execBudgetContext(context.Background(), 10*time.Millisecond, nil, "m1", nil)
	defer cancel()
	<-ctx.Done()

	breached, note := classifyErr(errors.New("boom"), ctx, "", "")
	if !breached {
		t.Error("breached = false, want true")
	}
	if !strings.Contains(note, "EXECUTION budget") {
		t.Errorf("note does not name the execution budget: %q", note)
	}
	if strings.Contains(note, "wall clock") {
		t.Errorf("note blames the wall clock, which is the misattribution being fixed: %q", note)
	}
}

// A combo hung inside its first request looks exactly like one still working:
// turns and tokens move only when a turn COMPLETES. Measured on a real run —
// ocr-survey-corners burned the entire 10-minute budget in all three arms with
// turns=0 and tokens=0, 30 of the run's 46 minutes, producing nothing.
func TestWatchdogCancelsASilentCombo(t *testing.T) {
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, 40*time.Millisecond
	defer func() { watchdogTick, stallTimeout = old, oldStall }()

	hb := &heartbeat{}
	hb.mark() // a request opened, then the model went quiet
	// A budget far longer than the test: only silence can end this.
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m", hb.idleFor)
	defer cancel()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a silent combo must be cancelled without waiting out the whole budget")
	}
	if !errors.Is(context.Cause(ctx), errStalled) {
		t.Errorf("cause = %v, want errStalled — the reason must say it hung, not that it ran out of time",
			context.Cause(ctx))
	}
}

// A model that is streaming is WORKING, however slow the whole answer is.
// Killing it would be worse than the waste the guard prevents.
func TestWatchdogLeavesAStreamingComboAlone(t *testing.T) {
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, 60*time.Millisecond
	defer func() { watchdogTick, stallTimeout = old, oldStall }()

	hb := &heartbeat{}
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m", hb.idleFor)
	defer cancel()

	// Chunks arriving steadily, slower than a tick but inside the timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			hb.mark()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	<-done
	if ctx.Err() != nil {
		t.Fatalf("a streaming combo was cancelled: %v", context.Cause(ctx))
	}
}

// Before any request opens there is nothing to be silent about. Treating an
// unset heartbeat as infinite silence would cancel every combo during setup.
func TestWatchdogIgnoresSetupBeforeTheFirstRequest(t *testing.T) {
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, 20*time.Millisecond
	defer func() { watchdogTick, stallTimeout = old, oldStall }()

	hb := &heartbeat{} // never marked
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m", hb.idleFor)
	defer cancel()

	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("setup was mistaken for a stall: %v", context.Cause(ctx))
	}
}

// The hole the first version shipped with: agentkit RETRIES a failed request,
// so marking the heartbeat on every stream open meant open/fail/reopen
// refreshed the clock forever. A combo making no progress at all looked
// perfectly healthy, and the guard that exists to catch exactly that sat
// through it.
func TestWatchdogCatchesARetryLoopThatNeverProduces(t *testing.T) {
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, 80*time.Millisecond
	defer func() { watchdogTick, stallTimeout = old, oldStall }()

	hb := &heartbeat{}
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m", hb.idleFor)
	defer cancel()

	// A request that opens, fails, and is reopened — forever, producing nothing.
	go func() {
		for i := 0; i < 200; i++ {
			hb.open()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a retry loop that never produces a chunk must still be caught")
	}
	if !errors.Is(context.Cause(ctx), errStalled) {
		t.Errorf("cause = %v, want errStalled", context.Cause(ctx))
	}
}

// And the converse: a request that opens once and then streams is progress,
// however long the whole answer takes.
func TestWatchdogTreatsChunksAsProgressNotOpens(t *testing.T) {
	old, oldStall := watchdogTick, stallTimeout
	watchdogTick, stallTimeout = 10*time.Millisecond, 80*time.Millisecond
	defer func() { watchdogTick, stallTimeout = old, oldStall }()

	hb := &heartbeat{}
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m", hb.idleFor)
	defer cancel()

	hb.open()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			hb.mark()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	<-done
	if ctx.Err() != nil {
		t.Fatalf("a streaming request was cancelled: %v", context.Cause(ctx))
	}
}

// The guard's whole purpose is a request that never comes back — and the first
// two versions could not see it, because the clock was armed AFTER
// ChatStream returned. A call that blocks inside never reached the arming line,
// so `started` stayed zero, idleFor reported 0 forever, and the combo sat out
// its entire 10-minute budget in silence. Arm on ISSUE, not on acceptance.
func TestHeartbeatArmsWhenTheRequestIsIssued(t *testing.T) {
	hb := &heartbeat{}
	// Nothing issued yet: setup, not silence.
	if d := hb.idleFor(time.Now()); d != 0 {
		t.Errorf("idle before any request = %v, want 0", d)
	}
	// Issue a request that never returns a chunk.
	hb.open()
	if d := hb.idleFor(time.Now().Add(time.Minute)); d < time.Minute {
		t.Errorf("idle a minute after issuing = %v, want >= 1m — the clock must run "+
			"from the request going out, not from a reply that never came", d)
	}
	// A chunk resets it.
	hb.mark()
	if d := hb.idleFor(time.Now()); d > time.Second {
		t.Errorf("idle right after a chunk = %v, want ~0", d)
	}
}

// A bench whose job is to WAIT rather than compete must not kill a caller for
// being polite. corrallm answers 429 + Retry-After when the box is busy, and a
// request backing off is silent for exactly the reason a hung one is — no bytes
// arrive. The plain deadline this watchdog replaced already made that mistake
// once: stages died "limit breached" for queueing.
//
// PER GAP: only the backoff inside this silence counts. Correcting by the whole
// combo's queueing made the guard forgiving in proportion to history, so on a
// contended box it would rarely fire at all.
func TestStallGuardForgivesBackpressureSilence(t *testing.T) {
	base := queueWaitNS.Load()
	defer queueWaitNS.Store(base)

	hb := &heartbeat{}
	hb.open()
	from, _ := hb.silentSince()

	// Ten minutes of silence, every second of it 429 backoff: not a stall.
	queueWaitNS.Add(int64(10 * time.Minute))
	if d := hb.idleFor(from.Add(10 * time.Minute)); d != 0 {
		t.Errorf("idle = %v, want 0 — all of that silence was backoff", d)
	}

	// Ten minutes of silence with only one minute of backoff: nine minutes of
	// real silence, and a stall.
	queueWaitNS.Store(base)
	hb.mark()
	from, _ = hb.silentSince()
	queueWaitNS.Add(int64(time.Minute))
	if d := hb.idleFor(from.Add(10 * time.Minute)); d < 8*time.Minute {
		t.Errorf("idle = %v, want ~9m — only the backoff is forgiven", d)
	}

	// PER GAP, not cumulative: backoff accrued BEFORE this silence began must
	// not excuse it. This is the case that made the cumulative version go quiet
	// under load.
	queueWaitNS.Store(base)
	queueWaitNS.Add(int64(time.Hour)) // an hour of queueing earlier in the combo
	hb.mark()                         // then a chunk arrived: a new gap starts here
	from, _ = hb.silentSince()
	if d := hb.idleFor(from.Add(5 * time.Minute)); d < 4*time.Minute {
		t.Errorf("idle = %v, want ~5m — an hour queued EARLIER excuses nothing now", d)
	}
}

// "Was the box busy during this run" had no answer: the row's Retries429 was a
// hardcoded 0, on the one axis where a busy box is the likeliest explanation
// for a stage that looks slow or dead.
func TestRetry429CountIsObservable(t *testing.T) {
	base := retry429N.Load()
	defer retry429N.Store(base)

	since := stageRetry429()
	if since() != 0 {
		t.Fatalf("a fresh delta should start at 0, got %d", since())
	}
	retry429N.Add(3)
	if got := since(); got != 3 {
		t.Errorf("retries = %d, want 3", got)
	}
	// Deltas, not absolutes: a later stage sees only its own.
	later := stageRetry429()
	retry429N.Add(2)
	if got, all := later(), since(); got != 2 || all != 5 {
		t.Errorf("later=%d all=%d, want 2 and 5", got, all)
	}
}

// The relationship that makes the stall guard safe, asserted rather than
// remembered.
//
// Legitimate silence inside ONE request is bounded by scheduler.maxWait: a
// waiter not granted within it is handed a 429 instead of being held. If
// maxWait ever exceeds stallTimeout, the bench starts killing requests that are
// merely queued — the "stages died for being polite" regression, again, and
// invisible until a busy box makes it fire.
//
// The box's maxWait is operator config and not readable from here, so this pins
// the contract in the direction that matters: stallTimeout must clear the
// documented bound with room for time-to-first-token (measured ~11s on a large
// image against a resident 27B).
func TestStallTimeoutClearsTheQueueBound(t *testing.T) {
	const (
		documentedMaxWait = 15 * time.Second // scheduler.maxWait
		measuredTTFT      = 11 * time.Second // 3400x4400 page, resident 27B
	)
	floor := documentedMaxWait + measuredTTFT
	if stallTimeout <= floor {
		t.Fatalf("stallTimeout %s must exceed maxWait+TTFT (%s), or a queued request "+
			"is indistinguishable from a hung one and gets killed for waiting",
			stallTimeout, floor)
	}
	// And not so large it stops being a guard: the execution budget is the
	// backstop, and a stall the operator waits minutes for is the failure this
	// replaced.
	if stallTimeout > 2*time.Minute {
		t.Errorf("stallTimeout %s is long enough that a hang looks like a slow run", stallTimeout)
	}
}
