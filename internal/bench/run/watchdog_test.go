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
	ctx, cancel := execBudgetContext(context.Background(), 50*time.Millisecond, nil, "m1")
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
	ctx, cancel := execBudgetContext(context.Background(), 200*time.Millisecond, nil, "m1")
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
	ctx, cancel := execBudgetContext(context.Background(), time.Hour, nil, "m1")
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
	ctx, cancel := execBudgetContext(parent, time.Hour, nil, "m1")
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
	ctx, cancel := execBudgetContext(context.Background(), 10*time.Millisecond, nil, "m1")
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
