# P34 — a turn: timesharing one card between models

> Opened 2026-09-22, from the owner: *"I'd like the image/embeddings to take turns
> with load/unload cycles. However, we don't have any good contention strategies and
> load throughput queue flushes between load/unload timeshares."*
>
> **Design only. Nothing here is built.** Three of its numbers are the owner's to set
> and are named in §9.

## 0. The problem in one sentence

corrallm queues **inside** a backend and **nowhere across** them. A request for a model
that is not resident either evicts the incumbent and waits out a 16-second load, or is
handed `retry_after=29.85s` and goes away. Nothing holds a request across a swap. That
is the "queue flush between timeshares".

## 1. Current behaviour, with citations

**There is a real queue, and it is per-backend.** `sched.Scheduler` arbitrates a
backend's admission slots across priority GROUPS: weighted fairshare on
`numerator/weight` (`internal/sched/scheduler.go:553`), a bounded queue
(`maxQueueDepth`, `scheduler.go:305`), a wait cap (`maxWait`, `scheduler.go:338`),
aging on the attempt rather than the request (`internal/sched/aging.go:32`), queue-slot
bumping so arrival order never outranks fairshare (`scheduler.go:305-318`), lane
reservations (`internal/sched/reservation.go:91`), and an honest `Retry-After` priced
off a measured dwell EWMA (`scheduler.go:729`). `backendState` is keyed by an opaque
string, and the string the proxy passes is the **served model name**
(`internal/proxy/proxy.go:832`) — so this layer is already per-model. What it is fair
*between* is groups.

**There is no queue across models.** `proc.Manager.EnsureReady` resolves a placement,
calls `makeRoomLocked`, and on failure returns `*CapacityError` straight to the caller
(`internal/proc/manager.go:488-492`). `makeRoomLocked` is all-or-nothing and never
parks anything. The proxy turns a transient miss into backpressure and **spills to the
next candidate**; when the walk runs out it answers 429 `exhausted`. There is no
residency queue, no quantum, no alternation, and nowhere for a request to wait for its
model.

**Eviction works.** P33 already corrected the opposite claim. The ledger recorded
`made room 1x — evicted local-Swift-Qwen3.8-27B for local-Qwen3.8-27B`, `model_load`
row `ms=16444`. What was mistaken for a refusal to evict is `capacityErrorLocked`'s
backoff: `wait = activeUse − (now − lastUsed)` (`manager.go:1862`), with `activeUse` =
30 s. A model used 150 ms ago produces `retry_after=29.85s`, and every request arriving
inside that window gets `exhausted`. **Do not re-derive this. It evicts.**

| measured | value | source |
|---|---|---|
| model swap on gpu0 | **16,444 ms** | `model_load` row, 2026-09-21 |
| spill backoff | **~30 s**, = `activeUse` | `manager.go:1862` + `manager.go:113` |

### 1a. Two findings the code gave up while reading it

**`minResidency` is dead arithmetic.** It is documented as the anti-thrash window
(`manager.go:101-103`, 10 s) but it is *not a filter anywhere*. `makeRoomLocked`'s
victim test is `refs == 0 && state == ready && now − lastUsed >= activeUse`
(`manager.go:1794`) — no mention of it. Its only other appearances are
`Process.protected`, used solely to ORDER victims in `sortVictims`
(`manager.go:2803`), and the `max()` in `capacityErrorLocked` (`manager.go:1863`) —
where it can never win: `lastUsed >= readyAt` always, so
`activeUse − (now−lastUsed)` >= `minResidency − (now−readyAt)` for every reachable
state.

So **today's de-facto turn floor is `activeUse` = 30 s, by accident**, set for an
unrelated reason, and the number that was *meant* to be the anti-thrash floor is 10 s
and inert — shorter than the 16 s load it is supposed to amortise.

**gpu1 already has four claimants and no pair of them fits.**

```
gpu1 usable                     9,877 MiB
  nomic-embed-text    816 MiB   persistent (pinned)
  ---------------------------   so 9,061 MiB timeshared
  chandra-ocr-2     8,240 MiB   sticky, input-driven peak
  deepseek          6,676 MiB
  image (CPU text encoder)  ~6,144 MiB   estimated, UNMEASURED
  image (full)             ~16,384 MiB   > 9,877 even with nomic evicted
```

chandra+deepseek 14,916 · chandra+image 14,384 · deepseek+image 12,820 — every pair
exceeds 9,061. **gpu1 is one pinned embedder plus exactly one of three rotating
models.** That is a three-way timeshare, not a two-way one.

gpu0 is the degenerate case: two 30.7 GiB models against a 31.5 GiB budget, strictly
one at a time. P33 already answered it — **settle on one name; scheduling is not the
fix there.**

## 2. Where the residency queue lives

**In `internal/sched`, as an arbiter over `(server, pool)`, called by
`proc.Manager.EnsureReady` at the point where it calls `makeRoomLocked` today.**

- **Above `proc.Manager`:** rejected. It would re-derive which models are warm and which
  pools they touch — facts `proc.Manager` owns — and would have to run *before*
  `selectPlacement`, which is what decides which pool is even in question.
- **Inside `proc.Manager`:** rejected on the repo's own rule. A queue there needs
  waiters, a fairness comparison, an aging discount, a depth bound, a wait cap and a
  `Retry-After` estimator. All six exist, tested, in `sched`. Building a second set is
  two ideas for one job.
- **In `sched`, arbitrating a second resource kind:** taken. `backendState` is generic
  over an opaque key; the ratio/aging/decay machinery is generic over a numerator.

**The split of duty is exact.** `sched` decides *whose turn it is*. `proc` keeps
everything about bytes and processes — `fitsLocked`, `sortVictims`, the reservation
ledger, the spawn. The arbiter never names a victim; it says "model M may take the pool
now", and `makeRoomLocked` decides what to unload to honour that.

It owns three things the slot scheduler does not: the right to occupy **bytes in a
pool** rather than a slot on a process; the **turn**, a lease held by a MODEL and
outliving any one request; and the **swap budget**, how much of a pool's wall clock may
go to loading rather than serving.

## 3. The quantum: demand-driven, floor-bounded, ceiling-bounded

Neither pure time nor pure work. A pure time quantum is not work-conserving — the
incumbent holds the card through a quiet tail while a rival waits. A pure work quantum
("drain the queue, then yield") is unbounded — one caller in a loop keeps the queue
non-empty and the rival never runs.

**A turn ends when the incumbent has nothing to do and somebody else does, or when the
ceiling elapses — whichever comes first — and never before the floor.**

**`minTurn` — the floor.** `minTurn = k x swap` makes the pool's load overhead exactly
`1/(k+1)`:

| k | turn @ 16 s swap | pool time spent loading |
|---|---|---|
| 2 | 32 s | 33% |
| 4 | 64 s | 20% |
| 8 | 128 s | 11% |

This is the extension of `minResidency`, which was supposed to be this and is inert
(§1a). It replaces it; it is not a fourth timer.

**`maxTurn` — the ceiling.** It exists so the rival's wait is a computable number.
Under saturated three-way contention on gpu1, a model's worst wait for its turn is
`2 x (maxTurn + swap)` — 160 s at `maxTurn = 64 s`, 288 s at 128 s.

**Yield-on-demand, and only on demand.** The turn ends early when the incumbent has
`refs == 0`, no queued requests, and a rival is waiting. The idle test is `activeUse`,
which is already exactly this predicate — reuse it, do not mint a second definition of
idle. **An incumbent with no rival keeps its turn forever**, so this feature is
invisible until contention exists.

### The millisecond-after-yield case

A request arriving 1 ms after its model yielded **queues for the next turn; it does not
re-trigger a swap.** The yield is *committed at the moment the eviction is issued*, not
when the replacement becomes ready.

This is the whole mechanism, not a concession. Without commitment the fast model wins
every race by arriving during the rival's 16 s load, which is precisely how the big
model starves today. And the arriving request's wait is, for the first time, a number
we own: `remaining rival turn + swap`.

The corollary, stated rather than hidden: **a turn cannot be revoked from an in-flight
request.** You cannot pause a diffusion step. `refs > 0` stays absolute, so `maxTurn` is
a target and the real turn is `max(maxTurn, end of the in-flight request)`.

## 4. Fairness across models, not just across callers

The two layers **compose**, because they arbitrate different resources and the
dependency runs one way: the arbiter's decision is an input to admission, never the
reverse.

**The composition is free.** The proxy admits a slot *before* `EnsureReady`. A request
waiting for a turn is therefore holding a slot, so **at most `sum of slots` of the
contending models can ever be waiting on residency.** The residency queue needs no
depth parameter of its own.

### The hard case: 5 minutes of diffusion against 40 ms of embedding

**It is not achievable by scheduling.** Round-robin turns are catastrophic for the
embedder — a 40 ms request waits up to `maxTurn + swap` to be served for 40 ms, a
latency amplification near 1000x. Meanwhile the image model gets a turn it cannot use,
because one generation exceeds any `maxTurn` worth setting. Any currency that equalises
these two makes one of them useless.

**So do not make them compete.** `nomic-embed-text` is 816 MiB measured. Buying its
permanent residency costs 8% of gpu1 and deletes the problem. That is §7, and it is the
answer, not a workaround.

Timesharing is right between models of **comparable service time and footprint** —
chandra-ocr (8.2 GB, seconds), deepseek (6.7 GB), the image model (6.1 GB, minutes).
There the currency is **decayed pool-seconds**, and the machinery exists
(`decayedShare`/`addShare`/`shareHalfLife`, `scheduler.go:754-797`): the `dwell`
currency generalised from "this group's recent time" to "this model's recent
occupancy". Lowest `occupancy/weight` takes the next turn.

**The load is charged to the model that caused it.** Otherwise a model that is expensive
to load gets its 16 s free and the cheap model subsidises it. This also produces the
right pressure without a rule about it: the 6 GB CPU-text-encoder image variant is
cheaper in the currency than the 16 GB one, so the arbiter prefers it because it is
cheaper, not because anybody wrote "prefer CPU text encoders".

## 5. What `Retry-After` becomes

Today it is honest about the wrong thing. `activeUse − sinceLastUse` truthfully reports
**when the incumbent stops being protected** and silently implies **when you will be
served** — two different claims, and it makes no promise you will get the room when it
frees. It is also the number that made "corrallm won't evict" look true twice.

With a queue, backpressure is for requests we **refuse**, and three refusals remain:

1. **Permanent** — does not fit on an empty pool. 503, unchanged. The 16 GB image model
   on gpu1 is this case.
2. **Queue full** — handled before residency by `maxQueueDepth`. Nothing new.
3. **The promised wait exceeds the caller's patience.** The new refusal, and what stops
   the queue being a memory leak with a latency tail. The bound is `scheduler.maxWait`
   and its per-model override, which already exist. A request whose estimated turn is
   further away than its bound is refused *up front* with `Retry-After` = the estimate,
   rather than parked.

The estimate is finally arithmetic over a queue we own: `remaining incumbent turn
(<= maxTurn) + measured swap + sum of turns of rivals ahead of you`. `swap` is measured
already — `Swap.LoadSeconds` seeds it and `model_load.ms` records the real one.

### The collision this creates, stated rather than buried

Every local model is `maxConcurrent: 1` and `maxWait` is 15 s. **A 64 s turn is longer
than the queue bound.** So while model B is off-turn, B's one slot is held by the
request waiting for the turn, and every *other* B caller queue-times-out at 15 s exactly
as they do today.

That is correct and should not be argued away: **a card that can hold one of three
models cannot promise three models' worth of service.** What changes is that the request
at the head gets *served* instead of refused, and the 429s behind it carry an
appointment instead of a protection-window guess. That makes it the same question
`open-questions.md` §2 already asks — *is an honest wait estimate even wanted* — with
teeth, because here the honest number is minutes.

## 6. Starvation and thrash

**Thrash is bounded by construction.** Overhead is `1/(k+1)` under saturated demand and
lower otherwise, because yield-on-demand only fires when the incumbent has no work — a
handoff nobody is paying for. The residual case is alternating demand with no idle gaps,
where turns are exactly `maxTurn` and overhead is `swap/(maxTurn+swap)`.

**The big model never loading** is the real starvation, and three mechanisms answer it,
all already in the codebase — which is the argument for putting this in `sched`:

1. **Decayed occupancy.** A model that held the pool for the last five minutes has a
   high numerator and loses the next turn.
2. **Aging on the attempt.** A caller rejected three times carries its ticket and its own
   age (`sched.WithWaitingSince`, `internal/proxy/ticket.go`). Apply the **same**
   `agingBoost` to the turn pick, as a bounded discount rather than a priority tier.
3. **Commitment** (§3). Without it the fast model wins every race during the 16 s load.
   This is the mechanism actually missing today; the other two only sharpen it.

Byte-stealing during a load is already handled: `reserveLocked` runs before the spawn
goroutine. The gap is time, not bytes.

## 7. Pinning: `persistent` already is this idea

**"Resident alongside" has a name and it is `persistent: true`.** It is declared,
carried onto the process, excluded from every victim scan (`manager.go:1790`, `:1836`),
refused by unload, skipped by the idle sweeper and preloaded at boot.
`nomic-embed-text` already uses it. **Do not invent a second concept.**

Nor a soft pin. "Resist eviction but yield under pressure" is `sticky.evictCost: high`
(`evictRank` -> `sortVictims`). A third tier between those two would be a fourth idea for
one job.

**How it interacts with the turn:** a pinned model is not a participant. Its bytes leave
the pool before the arbiter sees it, and the arbiter's capacity is
`budget − sum of pinned`. gpu1 becomes "9,877 total, 816 pinned, 9,061 timeshared
between three", which is a sentence an operator can act on.

**The one genuine gap:** `persistent` is per-model, the budget is per-pool, and
**nothing validates that the pinned set fits.** Pin 16 GB on a 10 GB card and every
other model on it answers "exceeds pool budget" with no hint that a pin caused it. That
is a config-time check, small and independent.

## 8. What is observable

The box already learned this the expensive way: the 2026-09-05 slowdown had to be
reconstructed from a database session because no screen said what kept displacing what.
A turn that is invisible is worse than no turn, because "waiting for its turn" and
"hung" are the same experience from outside.

1. **A turn is a record, on the row that already exists.** `ModelLoadEvent` carries
   `Evicted`, `Requester` and `MS`. Add why the turn was granted (demand / yield /
   floor-expiry), the occupancy ratio at grant, and who was waiting. Not a parallel
   table.
2. **"Whose turn is it" per pool, live.** `Handlers.Residency` already reports pool
   budget/used and resident models. Add turn holder, turn started, turn ends by, and the
   queue behind it **as models, not requests**. Belongs on `/machines` beside the pool
   bars, summarised in the Traffic header.
3. **A fourth in-flight state.** `markInflight` has `loading`/`queued`/`streaming`. A
   request waiting for a turn is none of them. Add `waitingForTurn` with the model and
   the ETA — exactly the failure `SchedulerConfig.MaxWait`'s own comment warns about.
4. **A fourth timing header.** `setTimingHeaders(queuedMS, loadMS, upstreamMS)`. A turn
   wait is neither queue nor load; folded into `loadMS` it makes `llm-bench` read a 64 s
   wait as a slow model. **`turnMS`, or the bench numbers lie.**
5. **A counter for the thing that must not happen.** Swaps per pool per hour against the
   computed ceiling `3600/(minTurn+swap)`. Exceeding it means the policy is not being
   honoured, and somebody should be told without writing SQL.

## 9. What this does NOT propose, and why

- **No preemptible or checkpointed model.** You cannot suspend a diffusion step.
  `refs > 0` stays absolute and `maxTurn` stays a target (§3).
- **No cross-pool scheduler.** `fitsLocked` is per-pool and gpu0/gpu1 have no observed
  coupling. Making them agree would couple two independent decisions for nothing.
- **No second queue in `proc`** (§2).
- **No `timeshare:` config block.** Every knob has a home already: `sticky.ttl` /
  `evictCost`, `persistent`, `swap.loadSeconds`, `scheduler.maxWait` / `maxQueueDepth`.
  Only `minTurn` and `maxTurn` are new, and they are per-pool, so they belong on
  `servers.<s>` beside `pools` and `reserve`.
- **No fairness between the embedder and the image model.** Argued in §4: unachievable
  by scheduling, solved by pinning.
- **No change to the Admit-before-EnsureReady order.** It is what bounds the residency
  queue for free (§4).
- **No predictive placement.** Measure first.

## 10. Phases

**Phase 1 — the turn exists, is enforced as protection, and is visible. Nothing waits
for it yet.** The smallest independently shippable and testable slice, valuable alone:
it stops the 16 s thrash without introducing any new latency tail or unbounded queue.

- `minTurn` / `maxTurn` per pool on `servers.<s>`, `minTurn` defaulting to `k x swap`
  from the measured load.
- `makeRoomLocked`'s victim filter (`manager.go:1794`) gains the turn floor as a **real
  filter**. `minResidency` is deleted or promoted — it cannot stay as it is (§1a).
- `capacityErrorLocked` returns *when the incumbent's turn ends* instead of when its
  protection window lapses. The number stops being a coincidence.
- Turn reason on the existing `model_load` row; turn holder and turn-ends-by on
  `/machines`.
- Testable end to end in `internal/proc` with the existing harness. No UI blocker, no new
  package, no request-path risk. It also produces the measurement phase 2's estimate
  needs.

**Phase 2 — requests wait for a turn.** The arbiter in `sched`, called from
`EnsureReady`. `waitingForTurn` in-flight state, `turnMS` header, `residencyMaxWait`
riding the existing `maxWait` knob. Read §5's collision before starting: most of the
value here is the honest estimate, not the waiting.

**Phase 3 — fairness across models.** Decayed occupancy as the turn numerator plus
`agingBoost` on the pick. Meaningless before phase 2 — there is nothing to be fair
between until something can wait.

**Phase 4 — pinning made honest.** Config-time validation that
`sum of persistent <= budget` per pool; the pool view reports pinned vs timeshared
bytes. Independent of 1-3, may land any time, and is the smallest thing here.

## 11. Decisions the owner owns

1. **`k`, and therefore `minTurn`.** 3 (21% of pool time loading, 64 s turns) or 8 (11%,
   128 s)? A throughput-vs-latency preference, not derivable.
2. **`maxTurn`** — equivalently, the worst head-of-line wait acceptable for a rival
   model. gpu1 three-way saturated: `2 x (maxTurn + 16 s)`.
3. **gpu1 composition.** nomic stays pinned (816 MiB) — confirm. Then which of chandra
   (8,240) / deepseek (6,676) / image (~6,144) rotate, and whether any should also be
   pinned. Pinning chandra means the image model never runs on gpu1.
4. **Image model variant.** ~6 GB with the text encoder on CPU (timeshares on gpu1) or
   ~16 GB (does not fit gpu1 at all — it would have to displace a 27B on gpu0).
5. **Wait, or come back?** Phase 2 assumes waiting is wanted. This is
   `open-questions.md` §2 with the numbers made concrete: the honest answer becomes
   minutes. If the answer is "come back", phase 1 is the whole feature and phases 2-3
   are dropped.
6. **May an image generation be refused rather than queued?** It holds an admission slot
   AND a turn for minutes. Refusing it early is defensible; so is letting one caller own
   the card. Not guessed here.
