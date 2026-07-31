package run

import (
	"context"
	"errors"
	"time"
)

// errExecBudget cancels a combo that spent its EXECUTION budget, as opposed to
// one cancelled by the caller or by the run's own deadline. Reported through
// context.Cause so a stage note can say which of those happened instead of
// guessing from a bare context.Canceled.
var errExecBudget = errors.New("combo exceeded its execution budget")

// watchdogTick bounds how stale the queue correction may be. Small enough that
// a genuinely hung combo is not held open much past its budget, large enough
// that polling corrallm costs nothing next to a 10-minute budget.
// A var so tests can shrink it; nothing else reassigns it.
var watchdogTick = 10 * time.Second

// execBudgetContext bounds a combo by how long it spent WORKING, not by the
// wall clock.
//
// The watchdog exists to catch a hang — a stuck MCP discovery or a wedged LLM
// retry that would otherwise silently eat the matrix (observed: 87 minutes). A
// plain deadline confused that with WAITING, which was harmless while the bench
// held corrallm's exclusive lease and nothing could queue ahead of it. Sharing
// the box broke that assumption: a measured run spent 73% of its wall clock
// queueing, so a 10-minute budget really allowed under three minutes of work,
// and stages died with "limit breached" for being polite.
//
// Both waits are excluded, and they are disjoint so they add: the 429 backoff
// this process slept through (corrallm never saw those requests) and the
// admission and cold-load time inside the requests it did accept (which the
// client cannot see at all, and which was ALL of the queueing in the run that
// exposed this).
//
// The correction can over-count when combos of one model overlap, since the
// query is scoped by model and window rather than by request. That makes the
// watchdog too lenient rather than too strict, which is the right way to be
// wrong: the run's own context still bounds everything, so the cost is a late
// cancellation, not a lost one.
func execBudgetContext(parent context.Context, budget time.Duration, oh *overheadClient, model string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	start := time.Now()
	sinceStart := stageQueueWait()

	go func() {
		t := time.NewTicker(watchdogTick)
		defer t.Stop()
		// Held across ticks: a failed lookup must not reset the correction to
		// zero, which would make the watchdog abruptly stricter exactly when
		// corrallm is too busy to answer — the moment queueing is worst.
		var serverWait time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if oh != nil {
					if d, err := oh.Between(ctx, model, start, time.Now()); err == nil {
						serverWait = d
					}
				}
				if time.Since(start)-(sinceStart()+serverWait) > budget {
					cancel(errExecBudget)
					return
				}
			}
		}
	}()
	// The caller's cancel must not claim the budget was spent.
	return ctx, func() { cancel(context.Canceled) }
}
