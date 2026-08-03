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

// errStalled cancels a combo whose model has gone SILENT — no stream chunk,
// token or tool call for stallTimeout.
//
// The execution budget alone could not catch this cheaply. A combo hung inside
// its very first request looks exactly like one still working: turns and tokens
// only move when a turn completes, so nothing distinguishes them until the full
// budget is spent. Measured: ocr-survey-corners burned the entire 10-minute
// budget in all three arms with turns=0 and tokens=0 — 30 minutes of a
// 46-minute run, producing nothing at all.
//
// Silence is the right signal rather than "no completed turns": a model that
// is streaming slowly is working and must not be killed, and a stream that
// wedges mid-generation is a hang the budget would otherwise sit through.
var errStalled = errors.New("model produced nothing for the stall timeout")

// stallTimeout is how long the model may produce NOTHING before the combo is
// abandoned. Generous on purpose — time-to-first-token on a large image prompt
// against a resident 27B is minutes, and killing a slow prompt would be worse
// than the waste it prevents. A var so tests can shrink it.
var stallTimeout = 4 * time.Minute

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
func execBudgetContext(parent context.Context, budget time.Duration, oh *overheadClient, model string, idleFor func(time.Time) time.Duration) (context.Context, context.CancelFunc) {
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
				// Silence first: it is the cheaper and more specific
				// diagnosis, and a stalled combo would otherwise sit here
				// until the whole budget drained.
				//
				// idleFor already excludes the 429 backoff slept through
				// during THIS silence — per gap, not cumulative. A bench whose
				// job is to wait rather than compete must not kill a caller
				// for being polite; that regression already happened once to
				// the plain deadline this replaced, when stages died "limit
				// breached" for queueing.
				//
				// Correcting by the whole combo's queueing instead made the
				// guard forgiving in proportion to history: a combo that
				// queued early and then wedged got a grace as long as
				// everything it had ever waited for, so on a contended box the
				// guard would rarely fire at all.
				if idleFor != nil && idleFor(time.Now()) > stallTimeout {
					cancel(errStalled)
					return
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
