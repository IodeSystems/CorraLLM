package run

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// queueWait accumulates time this process spent parked on corrallm's
// backpressure — the 429 + Retry-After loop — so a probe's wall clock can have
// it subtracted back out.
//
// Process-global and atomic because the alternative is threading a counter
// through agent.LLMRunner, which is agentkit's interface and knows nothing about
// benchmarking. The matrix runs combos concurrently, so a caller reads the delta
// across its own stage rather than the absolute (see stageQueueWait).
var (
	queueWaitNS atomic.Int64
	// retry429N counts backpressure retries, so a busy box is visible and not
	// merely subtracted out of the timings.
	retry429N atomic.Int64
)

// stageQueueWait returns a function that reports the backpressure wait accrued
// since it was called. Deltas, not absolutes: concurrent combos share the
// counter and only the change across one stage belongs to that stage.
//
// It over-attributes when combos overlap — two stages waiting at once each see
// both waits. That is the honest direction to be wrong in: it under-reports
// execution time rather than inflating it, so a probe never looks faster than
// it was.
// stageRetry429 mirrors stageQueueWait for the COUNT of backpressure retries.
//
// The count and the duration answer different questions: how hard the box
// pushed back, and how long that cost. Only the second was recorded, and the
// row's Retries429 was a hardcoded 0 — so "was the box busy during this run"
// had no answer at all, on the one axis where a busy box is the likeliest
// explanation for a slow or dead-looking stage.
func stageRetry429() func() int {
	start := retry429N.Load()
	return func() int { return int(retry429N.Load() - start) }
}

func stageQueueWait() func() time.Duration {
	start := queueWaitNS.Load()
	return func() time.Duration {
		return time.Duration(queueWaitNS.Load() - start)
	}
}

// NewBenchClient builds the LLM client every probe (and the judge) runs through.
//
// Two things distinguish it from a plain llm.NewClient:
//
// RetryBudget is UNBOUNDED. corrallm answers 429 + Retry-After when the box is
// busy, and the bench's job is to wait rather than to compete: a benchmark that
// gives up under load reports a failure that says nothing about the model, and
// the alternative — taking corrallm's exclusive lease — locks every other caller
// out of the box for the length of a run. Waiting forever is safe here because
// agentkit bounds 5xx separately; only the 429 path is unbounded, and a 429 is
// by definition a server that is alive and asking for patience.
//
// OnRetry accumulates how long that waiting cost, so the wait can be taken back
// out of the measurement. Without it every timing on a busy box is really a
// measurement of the queue.
func NewBenchClient(baseURL, apiKeyEnv, model string) *llm.Client {
	c := llm.NewClient(baseURL, os.Getenv(apiKeyEnv), model)
	c.RetryBudget = -1 // negative = unbounded; see llm.Client.retryBudget
	c.OnRetry = func(e llm.RetryEvent) {
		// Only backpressure. A 5xx retry is the server being broken, and its
		// backoff is part of what the run cost — subtracting it would hide a
		// flapping backend behind a healthy-looking number.
		if e.Status == 429 && e.Delay > 0 {
			queueWaitNS.Add(int64(e.Delay))
			retry429N.Add(1)
		}
	}
	return c
}
