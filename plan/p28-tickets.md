# P28 — tickets: demand, patience, and who is actually waiting

> Started 2026-08-24, from an operator looking at /activity and finding a row
> that read `1 / 1` while callers were being turned away by the dozen.

## 0. What was wrong

Three separate things, all of which made the same box look healthier than it was.

1. **The panel hid the demand it had already fetched.** `Utilization` queried
   `turnedAway` and never rendered it, so a row could read "1 / 1, queue 0"
   with 42 rejections behind it in the same window. Fixed first, UI-only.
2. **One slot was reported twice.** A lane's live load is summed from the
   members it resolves to, so `chat` and `local-Qwen3.8-27B` each reported the
   same busy slot and the column summed to nothing real.
3. **A rejection was anonymous.** The caller came back and nothing linked the
   return to the refusal — not in the log (correlation was "next request from
   the same key", a heuristic that breaks the moment a caller has two requests
   in flight) and not in the scheduler, which could not tell a caller returning
   for the fourth time from one arriving for the first.

The third is the one that changes behaviour. On a one-slot backend it is how a
caller starves: every time it returns, somebody's fresh request is exactly as
deserving, and arrival order decides against it. Measured over 7 days before any
of this: **dun 175 queue-timeouts, yscr 157, life-raglit 58**, all against
`maxConcurrent: 1`.

## 1. The ticket

One caller's ATTEMPT, across every rejection it takes.

```
caller ──POST──▶ corrallm            no id, at capacity
       ◀──429──  X-Corrallm-Ticket: tkt_2f8k3_a91c4_9d2e11f0a3bc
                 { "error": { …, "ticket": "tkt_…" } }
       ──POST──▶ X-Corrallm-Request-Id: tkt_…      ← the retry names itself
```

- A caller MAY bring its own id (`X-Corrallm-Request-Id`, or `X-Request-Id`).
  We keep it; minting over it would break correlation on the caller's side.
- We mint only when we reject a request that has none.
- Returned in the header AND the body: the header for a proxy-aware client, the
  body for a human reading a log line and for clients that only unmarshal the
  OpenAI error shape.
- Echoing is optional. A client that ignores it is served exactly as before.

**Format.** `tkt_<mint ms base36>_<nonce>_<hmac12>`. The mint time is IN the id
because that makes age free to compute — the alternative is a map that grows
with traffic, needs expiry, is consulted on the admission path and has to
survive restarts, all to recover a number the caller is already carrying. The
HMAC (process-local secret) means age cannot be SET by the caller: an unsigned,
foreign or forged id is a fine identity for grouping the log and buys no
priority.

## 2. Aging: what the ticket is spent on

Age discounts the fairshare ratio — twice as deserving at 30s (`agingUnit`),
capped at 4x (`maxAgingBoost`). A discount rather than a priority tier, so a
weight the operator chose survives any amount of patience; otherwise aging just
trades one starvation for another.

**The discount alone is not enough, and the test that proves it failed first.**
Two callers in one group with nothing in flight both have numerator 0, so both
ratios are 0 however long either has been trying — exactly the starvation case.
So the comparison is (ratio, then earlier attempt start). Two fresh requests
still fall back to arrival order, which is what every existing caller gets.

Every pick goes through one comparison (`moreDeserving`). When only some of them
aged, queue admission and slot admission would rank the same pair differently
and an aged waiter could be bumped OUT of the queue by an arrival it would have
beaten to the next free slot.

The mark rides on the context (`sched.WithWaitingSince`) rather than Admit's
signature: sixty-odd call sites, optional per-request metadata, and an unmarked
context behaves exactly as before.

## 3. Journeys

A rejection count cannot say whether it was one caller refused eight times or
eight callers refused once. `store.Journeys` groups activity by ticket — attempts,
rejections, whether it ever got an answer, and the wall clock from first attempt
to last, which is the number the CALLER would recognise (much larger than any
single request's queue wait). Surfaced at `/api/v1/activity/journeys` and as a
panel on Activity.

A caller that never echoes appears as a run of one-attempt journeys. That is
worth seeing on its own: it means its retries are invisible to the scheduler
too, so patience is buying that caller nothing.

## 4. Phases

- ✅ **P28a — mint, return, record** (`7c48517`). Ticket minting on rejection,
  header + body, `activity.ticket` with a partial index.
- ✅ **P28d — aging** (`986796e`). Discount + tie-break, one comparison for
  every pick.
- ✅ **P28b/P28e — journeys + lane rows** (`e422a27`). Journeys op and panel;
  `UtilizationRow.lane/members` so a lane stops reading as separate capacity.
- ◐ **P28c — agentkit echoes the ticket.** `Backpressure.Ticket` +
  `postWithRetry` carrying it across attempts, with tests. **Written, NOT
  committed:** that tree has unrelated work in progress (`agent/overflow.go`,
  `llm/repetition.go`, a changed `streamChat` signature) and `./agent` does not
  currently compile. `./llm` builds and its tests pass. Committing there would
  bundle someone else's half-finished feature.

## 5. Deploy order (matters)

The schema and the GraphQL surface both changed, so: **restart the server, then
rebuild the UI.** A new UI against an old server queries fields it does not have
and renders its error state. See the note in
`memory/corrallm-ui-dist-is-live-web-root.md` — `ui/dist` IS the live web root.

## 6. Open

- **Capacity is still not recorded.** `Scheduler.Snapshot()` knows it per
  backend; the lane sampler in `cmd/corrallm/main.go` reads `active`/`waiting`
  and throws capacity away, so `lane_samples` has a numerator and no
  denominator. A real demand/capacity time series needs it, and it has to be
  SAMPLED — capacity changes with residency.
- **Aging is unmeasured in production.** It should show up as fewer repeat
  rejections per ticket. The journeys view is the instrument; read it after a
  day of real traffic before tuning `agingUnit`.
- **Raising capacity is still the other lever.** All six local models are
  `maxConcurrent: 1` and llama-server runs `--parallel 1`. `--parallel 2` with
  halved context is a measurable trade nobody has made yet.
