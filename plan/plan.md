# corrallm — design & roadmap

> Corral + LLM. An OpenAI-compatible reverse proxy + model lifecycle manager +
> priority/fairshare scheduler with cost-aware overflow. Successor in spirit to
> llama-swap (clean-room; reuse *patterns* from redline2, not code).

Status: **MVP shipped + P8 complete; P7 next.** Engine is runnable: OpenAI proxy
+ spawn lifecycle + fairshare scheduler + ordered fall-through + residency/
eviction + preemption + cost model — and observable: activity log, residency/
pool usage, per-model cost rollup, lanes/backend-health live view, all updated
live over SSE. **MVP = through P6 + the observability UI slice** — done, plus the
P8-beyond polish. Remaining: **P7 (quality degrade)** and Later (multi-node).
How to work this plan is §0; roadmap is §6; decisions/extensions/deferred are §7.

> **Progress (updated 2026-06-23)**
> - ✅ **P0 scaffold** — `fdf90b9`
> - ✅ **P1 proxy core** — `566b888`
> - ✅ **P2 scheduler engine** — `13f15df`
> - ✅ **P3 backend-list fall-through** — `ebcff81`
> - ✅ **P4 residency** — `ec1bcfb`
> - ✅ **P5 preemption** — `8b8b218` cooperative streaming-safe cancel; preempt-before-spill
> - ✅ **P6 cost model** — `7bfdbad`/`84f4f70`/`d1091f1`/`e93bf2f`/`1e6ee19`/`c18a698`:
>   energy/paid/swap → $, per-request metering+persist, sliding-window limits,
>   configurable share currency. Two adversarial-review passes; fixes folded in.
> - ✅ **P8 MVP slice** — `dc9ffd3`/`b7d8dcc`/`b7e1b92`: recentActivity op +
>   activity table; residency read op (`Manager.Snapshot`) + usage view (pool
>   bars + resident models); usageRollup op + per-model 24h cost rollup. **MVP reached.**
> - ✅ **P8-beyond** — `adf7483`/`45c93d0`: lanes op (`Scheduler.Snapshot`) — groups
>   live view + backend-health/utilization; live SSE events (`internal/events`) replace
>   polling (proxy publishes activity/changed; UI invalidates on push, 15s fallback).
> - ▶ **next** — P7 quality-degrade.
> - ☐ P7 quality-degrade · Later: multi-node
>
> All shipped phases: `go build`/`vet`/`test` (incl `-race`) green, gofmt clean.
> Deviations from design: (1) UI served from `--web-root` dir (not `go:embed`),
> matching redline2; (2) live events use **SSE**, not WebSocket (server→client
> only, no dependency, EventSource auto-reconnects — subBroker fan-out preserved).
> Store is minimal (activity log + rollup query); no sqlc.

---

## 0. Working this plan

This file is the single source of truth for status. Keep it honest and current —
update it in the **same commit** as the code it describes.

**Checkbox legend.** ☐ not started · ▶ in progress (exactly one Pn at a time) · ✅ shipped.
A box is checked **only** when its functional unit meets the Definition of done below.

**A phase is a functional unit.** Each `Pn` is an independently shippable slice: it
compiles, its behavior is tested, and the engine still runs with it landed. Don't
start `Pn+1` until `Pn` is ✅. A phase too big to land at once → split it into
sub-units (still each a green, tested commit), not a half-done checkbox.

**Definition of done (per functional unit) — all must hold before ✅:**
1. `go build ./...`, `go vet ./...`, `go test ./...` green; `gofmt -l` reports nothing.
2. New behavior has tests: a unit test for logic, an integration/e2e test for any
   request-path change. A bug fix lands with the regression test that catches it.
3. UI changes: `bin/gen` re-run, `tsc`/eslint clean, the SDL snapshot committed.
4. This plan updated: phase ✅ + commit hash + one-line "what shipped"; resolved
   decisions moved to §7 Resolved; new discoveries filed (rules below); the Status
   line and the Progress block synced.

**Committing.** Conventional commits. Scaffolding and implementation are **separate**
commits (`chore: scaffold X` then `feat: X`). Commit each functional unit on its own —
never batch unrelated phases. The plan-doc update rides with its phase's commit or a
trailing `docs(plan):` commit (as the P0–P5 history shows). **Don't push unless asked.**

**Filing new work as you discover it — put it in exactly one place:**
- Needed for the *current* phase → add a sub-item to that phase's checklist and do it.
- A follow-on the *next* phase needs → **§7 Next steps**.
- Improves the product but no phase requires it → **§7 Optional extensions**.
- Out of scope until much later → **§7 Deferred**.
- A shortcut / known gap in code already shipped → **§7 Deferred work / known gaps**.

**MVP boundary.** MVP = **P0–P6 + the observability UI slice** (activity, residency,
and cost visible). The MVP line in §6 marks it; everything below the line is post-MVP
polish and may be reordered as needs dictate.

---

## 1. Vision

One control plane that "herds" many LLM backends — local processes it spawns and
remote/paid endpoints it forwards to — behind a single OpenAI-compatible surface.
It decides **who gets served, on which backend, at what quality, and at what cost**,
under contention, per caller identity.

It must support the full set of fairshare semantics (the "farewell post"):

1. **Lane priority** — higher classes move ahead; may preempt lower (interrupt optionality).
2. **Constrained throughput ratio** — under capacity pressure, weave admission by identity weight.
3. **Unconstrained / cost-shaping** — mostly TCO/$ shaping; always emit backoff info.

…across two cost dimensions (**request count** and **time-in-request / request cost**),
plus **load-spreading** (local saturated → spill to remote → spill to paid) and a
fourth saturation exit we surfaced: **service-quality degradation** (serve a smaller/
cheaper variant).

The engine is one pipeline; every flavor above is a *configuration* of it.

---

## 2. Stack (mid-weight reuse of redline2 patterns)

| Layer | Choice | Why |
|---|---|---|
| API | **Huma + gwag/`gat`** | one Go handler → REST + GraphQL (+ gRPC later). The "register once, typed everywhere" loop. |
| UI client | **graphql-codegen + graphql-request**, `gql` tagged templates, graphql-eslint | typed React call sites from the dumped SDL; no hand-written DTOs |
| Frontend | **React 19 + Vite + TanStack Router/Query + MUI** | matches redline2; file-based routes |
| Store | **SQLite / embedded** (config in YAML), metrics = in-mem ring + persisted rollups | a proxy is mostly stateless; no Postgres |
| Codegen | single **`bin/gen`** orchestrator (sdl dump → graphql-codegen → lint) | offline, deterministic |
| Config | YAML primary (llama-swap-style), layered `.properties` for secrets/env | operator-friendly |
| Dev | air (Go) + Vite, proxied; UI `go:embed` in prod binary | single binary ships the UI |

`gat` decode: register a handler with Huma once; `gat` projects it to GraphQL/gRPC,
`dump-graphql` writes a committed SDL snapshot, codegen turns it into a typed TS client.
That handler→typed-call-site loop is the redline2 pattern we're carrying over.

---

## 3. Core concepts

### Served model → ordered backend list
A served name (what clients put in `"model"`) maps to an **ordered list of backends**.
A backend optionally spawns a command and always has a proxy target:

```yaml
backend:
  cmd?:   string        # optional: spawn it; proxy points at the port it binds
  proxy:  number | "host:port" | { host?, port?, headers? }   # forward target
  type:   string        # cost class: local | claude | … (keys into commandCosts)
  quality: int          # relative quality rank (higher = better)
```
- `cmd` present → spawn + health-check + proxy to local port; absent → pure proxy (remote/paid).
- `headers` → auth for remote ($) endpoints.
- **Fall-through** (overflow *and* degrade) = accepting a backend further down the list.
- **Round-robin within the same `type`** (cost-equivalent); **ordered across types**.

### Cost model — everything resolves to $
```yaml
costPerKwh: 0.14          # configurable; converts local energy → $
commandCosts:
  local:  { generateWattsPerToken: 0.9, processWattsPerToken: 0.3 }   # → kWh → $
  claude: { extract: { costFactor: 0.8 } }                            # $ from response usage
```
- **Local** cost = (gen_tokens·genW + prompt_tokens·procW) → kWh × `costPerKwh` → $.
- **Paid** cost = extracted usage × `costFactor` → $.
- **Swap/load** cost = load energy → kWh × `costPerKwh` → $ (plus latency, a scheduling input).
  Charged to whoever triggered the load (or amortized across the coalesced batch).
- Two *uses* of cost, kept distinct:
  - **Share currency** (fairshare ordering): default **request-count**; per-group override to `dwell` or `cost`.
  - **Cost/$ ** (TCO limits, budgets, cost-shaping, reporting): always computed via the above.

### priorityGroup — the single policy unit
A key maps to exactly one group. The group bundles all policy:
```yaml
priorityGroups:
  interactive:
    weight: 10                       # share under contention (in share currency)
    shareCurrency?: requests         # optional override of global default
    interruptible: false             # may a higher group preempt it?
    onSaturated:                     # per backend-type stage policy, walked along the list
      local:  { preempt: true, then: fallThrough }      # take a local slot, else move on
      claude: { spill: true, limits: { cost: "$20/hr" } }# may use paid, budget-capped
      default: reject
  batch:
    weight: 1
    interruptible: true
    onSaturated:
      local:  { queue: true }        # wait for local capacity only
      default: reject                # never spends money
    limits: { dwell: "600s/min" }    # group-wide TCO cap (in addition to per-type)
  default:                           # global default lane for unkeyed/unlisted callers
    weight: 1
    onSaturated: { default: reject }

keys: { aw3: interactive, ragtag: batch }
```
`onSaturated` exits (composable, per type): **preempt** (cancel a lower interruptible
group's slot) · **spill/fallThrough** (advance to next backend) · **queue** (wait + Retry-After)
· **reject** (429). Over-budget (a `limits` cap) feeds the *same* sequence — it's just one
more reason a stage fails and we advance/queue/reject.

### Servers, residency & swap — the resource layer
Beneath scheduling sits **residency**: which models are *loaded where*, bounded by host
capacity, with swap cost and stickiness shaping load/evict decisions. Scheduling decides
*who/where*; residency decides *what's warm*. The two interact every request.

Capacity is a **vector over named memory pools** (each GPU's VRAM + system RAM + …), and a
backend draws from several at once (CPU/GPU offload, multi-GPU split, KV in RAM). A backend
*fits* iff for **every** pool `Σ(resident usage) + this ≤ capacity − reserve`.

```yaml
servers:
  box1:
    pools:   { gpu0: 24GB, gpu1: 48GB, system: 128GB }
    reserve: { system: 16GB }                    # headroom for OS + other procs
    maxConcurrent?: 4                            # optional throughput/power cap

models:
  qwen3-coder:
    sticky: { ttl: "5m", evictCost: high }       # keep warm; resist eviction; anti-thrash
    backends:
      - { cmd: "… -ngl 60", server: box1, ramUsage: { gpu0: 16GB, system: 8GB },
          swap: { loadSeconds: 18 }, proxy: 8081, type: local, quality: 100 }
      - { cmd: "… -ts 10,40", server: box1, ramUsage: { gpu0: 10GB, gpu1: 40GB }, proxy: 8082, type: local, quality: 100 }
```
- **Server capacity** = a **vector over named pools** (per-GPU VRAM + `system` RAM + …) →
  which *spawned* backends can co-reside. Fit = per-pool `Σresident + new ≤ capacity − reserve`
  (vector bin-packing). Mutual exclusivity is **emergent** and **multi-dimensional** (two models
  may share `gpu0` but collide on `system`), not hand-declared groups. Eviction is driven by the
  **binding** pool — only free what relieves the constrained dimension. Proxy/remote backends
  consume no local pools.
  - **Capacity is a declared budget, not a live probe** — vendor-neutral, deterministic,
    testable without hardware, and what actually gates admission/eviction. `server.pools` totals
    + `reserve`; each spawned backend declares its `ramUsage` vector; accounting keeps each pool
    within budget. Apple = a `system` slice (unified); CPU-only = just `system`.
  - **Usage is partly dynamic** — a backend's footprint = weights (static) + KV cache (scales
    with `--parallel` slots × context). `ramUsage` declares the **max at configured concurrency**
    (worst-case reservation); refine later with `{base, perSlot}` if needed.
  - **Probing is optional, pluggable, never authoritative.** Per-pool: the `system` pool is
    universally probeable (`/proc/meminfo`, `sysctl`, `GlobalMemoryStatusEx`); GPU pools use a
    `CapacityProbe` provider — `nvidia` (nvidia-smi/NVML) · `drm` (linux sysfs, amdgpu+intel) ·
    `amd` (amd-smi) · `metal` (darwin) · `none`; `capacity.probe: auto` tries in order, falls to
    `none`. Probe only **auto-fills** undeclared totals, **drift-guards** external pressure, and
    feeds dashboards — correctness never depends on it. (Linux DRM fdinfo gives per-process GPU
    memory for amdgpu/intel to refine a backend's declared footprint over time.)
- **Swap cost** per backend = load latency + load energy (measured EWMA, seeded by config;
  energy → $ via `costPerKwh`). Input to two decisions:
  - **swap vs spill**: target model cold + host full → evict+load (swap cost) **or** fall
    through to a warm/remote backend (spill cost $). Weigh swap-$+latency vs spill-$.
  - **eviction**: which resident model to evict — `evictCost`/stickiness + recency weight it
    (the llama-swap `evict_cost` solver analog).
- **Stickiness**: `ttl` keeps a model warm (idle, not evicted) after last use; `evictCost`
  resists eviction; **affinity** — a latency-sensitive group prefers an already-warm backend
  over paying a cold load, even if it's higher in the ordered list. Per-group: interactive
  avoids swaps; batch tolerates them.
- **Model states**: `absent → loading → ready → idle(warm) → evicting`. Requests for a
  *loading* model **coalesce** behind the single in-flight load (no duplicate loads), then admit.
- **Pinned/preload**: a model may be `persistent: true` (always resident, e.g. embeddings) or
  preloaded at boot; pinned models are exempt from eviction and reserve their VRAM.

---

## 4. Request decision pipeline
```
req → resolve served model, caller key → priorityGroup
for backend in model.backends (ordered; rr within a type):
    stage = group.onSaturated[backend.type] or .default
    if group over a `limits` budget for this type → honor stage (advance/queue/reject)
    try admit on backend (fairshare among groups for its slots, by share currency + weight)
        admitted → proxy, meter (dwell, tokens → $), return
    saturated → apply stage:
        preempt: cancel a lower interruptible group's in-flight slot here, admit
        spill/fallThrough: continue to next backend
        queue: hold with Retry-After backoff
        reject: 429 + structured backoff (X-RateLimit-*, Retry-After, JSON hint)
exhausted → backoff per terminal stage
```
**Backoff is always informative** (Retry-After + capacity/inflight/waiting + reason),
the BackpressureError shape we already validated.

---

## 5. Lessons carried from the llama-swap work
- **Resource/UI passthrough must bypass the scheduler.** The model's own web UI (`/upstream/<model>/…`)
  and other non-inference paths get an *untracked* serve once the backend is up — they
  must not consume admission/concurrency. (The gatedPaths lesson — make it structural here.)
- **Dwell-time, not request count, is the honest cost** for mixed workloads — but make it a
  configurable share currency, default request-count, with measured-dwell available.
- **Interactive ≠ streaming.** Identify interactive by browser signal (Sec-Fetch + Origin)
  if/when relevant, but in corrallm the first-class notion is the **priorityGroup**, not "interactive."
- **Clean-room.** Reimplement from these patterns; do not copy llama-swap source.

---

## 6. Delivery roadmap (sequenced; engine general from day 1)

- ✅ **P0 — Scaffold.** `fdf90b9`. Go module `github.com/iodesystems/corrallm`, Huma+gat wired,
  `dump-graphql`, React/Vite/codegen, `bin/gen`, YAML config loader + `.properties` layering,
  SQLite store, air+vite dev. *(UI via `--web-root`, not `go:embed`.)*
- ✅ **P1 — Proxy core.** `566b888`. Served model → single local backend: spawn `cmd` (own process
  group), health-check, load-coalescing, OpenAI passthrough (chat/completions, completions,
  embeddings, rerank, models). Untracked `/upstream/<model>/…` bypass. Activity log. Graceful
  SIGTERM shutdown reaps spawned children. `internal/proc`, `internal/proxy`.
- ✅ **P2 — Scheduler engine.** `13f15df`. priorityGroups + keys + synthesized default group.
  Weighted-fairshare admission (request-count share) over **per-backend slots** (`maxConcurrent`),
  queue + reject stages, informative backoff (429 + `Retry-After` + `X-RateLimit-*` + JSON).
  Caller key = `X-Corrallm-Key` or bearer token. `internal/sched`.
- ✅ **P3 — Backend list + fall-through.** `ebcff81`. Ordered walk of a model's backends:
  rr-within-`type`, ordered across types; per-type `onSaturated` spill/fallThrough advances,
  queue waits, reject is terminal, exhausted list → 429. `orderBackends()` + `Stage.Spill` wired.
  Quality carried but not a sort key (list order authoritative; per-quality routing is P7).
  *(preempt-vs-spill fork deferred to P5 — preempt has no implementation until then.)*
- ✅ **P4 — Residency.** `ec1bcfb`. Per-server pool-budget ledger gates spawns (fit = ∀pool
  want ≤ budget−used); eviction solver (evict-then-spill) frees idle non-pinned residents on the
  binding pool, ordered ttl-expired→unprotected→low evictCost→LRU, all-or-nothing → else
  ErrNoCapacity → spill. In-flight (ref-held) and `persistent` models exempt; persistent preloaded
  at boot. Size parsing + pool validation. *Not yet: affinity (prefer-warm over list order),
  `server.maxConcurrent` host cap, CapacityProbe, proactive ttl reaper, dynamic footprint — see §7.*
- ✅ **P5 — Preemption.** Cooperative, streaming-safe cancel of an in-flight slot held by a
  lower-weight, `interruptible` group when a higher group's stage allows `preempt`. The scheduler
  tracks per-slot cancel funcs; `Admit` returns a request context canceled (cause `ErrPreempted`)
  on preemption, which the proxy reverse-proxies under so the cancel aborts the upstream stream and
  frees the slot. The freed slot is handed to the preemptor first (preempt waiters jump fairshare).
  Victim = lowest-weight interruptible slot, strictly below the preemptor (equal/higher exempt),
  each victim targeted once. **Default ordering: preempt before spill** — with no eligible victim,
  the stage's `then`/spill (else queue/reject) applies. `sched.pickVictim`/`pickWaiter`.
- ✅ **P6 — Cost model.** `7bfdbad`/`84f4f70`/`d1091f1`/`e93bf2f`/`1e6ee19`/`c18a698`. The
  parsed-but-inert cost/limits config now behaves. New `internal/cost` package; scheduler gains a
  sliding-window budget ledger + configurable share currency (via `NewWithConfig`, injectable clock).
  - [x] **Local energy → $** — `(completion·genWh + prompt·procWh)/1000 × costPerKwh`. `cost.RequestUSD`.
  - [x] **Paid extraction → $** — `(prompt+completion) × costFactor` for `costFactor`-bearing types.
  - [x] **Swap/load $** — `swap.loadSeconds × loadWatts → kWh × costPerKwh`, charged to the request
        that triggered the cold load (`EnsureReady` reports `loaded`). *(Amortization across the
        coalesced batch deferred — trigger pays full; §7.)*
  - [x] **`limits` enforcement** — per-group + per-(group×type) TCO caps over a **sliding window**
        (`ParseRate` reads `$20/hr`/`600s/min`/`100/min`). Over-budget → spill if the stage allows,
        else back off (reason `over-budget`) with the time until the window frees; preemption N/A.
  - [x] **Share-currency option** — `requests` (default, in-flight count) | `dwell` | `cost`
        (per-group, decaying accumulator, 30s half-life). Mixed-currency queues fall back to
        request-count (coherent, starvation-free).
  - [x] **Meter + persist** — dwell + tokens + $ per request into the activity record (feeds P8);
        streaming + non-streaming usage capture, identity-decode for compressed upstreams.
- **P7 — Quality degradation.** *(beyond MVP)*
  - [ ] `quality` becomes a sort/routing key for degrade fall-through (today it's carried metadata).
  - [ ] Optional request transforms (clamp `max_tokens`/context) when serving a lower variant.
  - [ ] Resolve variant-in-list vs separate fallback map (§7).
- ✅ **P8 (MVP slice) — UI / observability.** `dc9ffd3`/`b7d8dcc`/`b7e1b92`.
  - [x] `recentActivity` GraphQL/REST op + `/activity` polling table (dwell/tokens/$).
  - [x] Residency read op (`Manager.Snapshot`: pool budget/used + resident backends) +
        `/usage` view (per-server pool-utilization bars + resident-model table).
  - [x] `usageRollup` op (per-model requests/tokens/dwell/$ over a window) + a 24h
        summary + per-model rollup table on the Usage page.
- ✅ **P8-beyond — observability polish.** `adf7483`/`45c93d0`.
  - [x] Lanes/groups live view (`Scheduler.Snapshot` → `lanes` op): groups
        (weight/currency/interruptible + live active/waiting) + backend
        health/utilization. $ dashboard = the `usageRollup` 24h view.
  - [x] Live events (`internal/events` broker → `/api/v1/events` SSE), replacing
        poll: proxy publishes activity/changed; UI invalidates caches on push,
        15s fallback. *(SSE not WebSocket — see Status deviations.)*

> **── MVP line ──** Above: P0–P6 + the P8 MVP slice = a usable, observable control
> plane. Below: post-MVP polish, reorderable.

- **Later.** Multi-node peer awareness (remote load introspection across corrallm peers).

---

## 7. Open items / decisions

### Resolved this session
- ✅ **Module path & repo location** — `github.com/iodesystems/corrallm` at
  `iodesystems/services/corrallm`, its own git repo (sibling to redline2/ragtag).
- ✅ **Binary name** — `corrallm` (not the `corral` alias).
- ✅ **Capacity unit** — **per-backend slots** (`maxConcurrent`, default 1), chosen over
  per-server total concurrency. `server.maxConcurrent` layers on as a host ceiling with P4.
  (Capacity-declaration question — declared budget canonical + optional `CapacityProbe` — stands.)
- ✅ **Load coalescing** (P1) — concurrent requests for an unspawned backend wait behind one
  in-flight load (`proc.Manager`, `ready` channel). Queue-behind-load *backoff signaling* still TBD.
- ✅ **Swap-vs-spill default** (P4) — **evict-then-spill**: try eviction to make the preferred
  backend fit; spill only if eviction can't free enough. Configurable later; cost-minimizing
  weighing waits for P6.
- ✅ **Eviction policy** (P4) — evictCost + recency (LRU) + ttl-expiry scoring, constrained to the
  binding pool, all-or-nothing greedy, min-residency hysteresis. Vector bin-packing is greedy
  (small N).
- ✅ **Preempt-vs-spill default ordering** (P5) — **preempt first**: a `preempt` stage reclaims an
  eligible victim before considering spill; only when no victim exists does the stage's `then`/spill
  (else queue/reject) apply. Victim is the lowest-weight `interruptible` slot strictly below the
  preemptor. Per-type `onSaturated` can still pin behavior explicitly via `then`.
- ✅ **`limits` window semantics** (P6) — **sliding window** (trailing per-dimension event log,
  pruned on access), reading `$20/hr`/`600s/min`/`100/min`. **Both** per-group and per-(group×type)
  caps apply (a request charges against both). Over-budget → **spill if the stage allows, else back
  off** (reason `over-budget`, Retry-After = longest binding window); queue/preempt don't apply to a
  budget. Requests charge at admit (incl. the queue/promote path), dwell/cost at release.
- ✅ **Share-currency granularity** (P6) — **per-group** (`requests|dwell|cost`), request-count the
  default. `dwell`/`cost` use a per-group accumulator decayed with a 30s half-life (cost is
  retrospective; dwell measured at release). A backend whose queued groups disagree on currency
  falls back to request-count for that comparison — coherent and starvation-free. (Per-key not done.)

### Still pending (blocking the noted phase)
- **Stickiness/affinity weighting** — how strongly a warm backend overrides *ordered list*
  preference (P4 does ttl/evictCost for *eviction*, but the proxy still walks strict list order
  regardless of warmth); per-group vs per-request latency hint — **P7**.
- **Quality-degrade across served-model boundaries** (variant in same list vs separate fallback
  map) — **P7**. Currently folded into the one backend list via `quality`.

### Optional extensions (improve the product; no planned phase requires them — pull in opportunistically)
- **gRPC surface** — gat gives it cheaply, but no consumer yet; add when one appears.
- **CapacityProbe** (nvidia/drm/amd/metal/none, auto) — declared budget is canonical and
  implemented; the probe only auto-fills undeclared totals, drift-guards, and feeds dashboards.
- **`server.maxConcurrent` host cap** — per-backend slots enforced (P2); the host-wide concurrency
  ceiling parses but isn't enforced yet (layer onto residency).
- **Proactive ttl reaper** — P4 eviction is lazy (on demand); `ttl` only orders victims. A
  background reaper that frees warm-but-expired models for power is not built.
- **Dynamic footprint** — KV scales with slots×context; v1 reserves worst-case `ramUsage`;
  refine with `{base, perSlot}` later.

### Deferred (out of scope until later)
- **NUMA / interconnect** — per-NUMA system pools, PCIe/NVLink cost of multi-GPU splits.
- **Multi-node peer awareness** — remote load introspection across corrallm peers (roadmap "Later").

### Deferred work / known gaps in shipped code
- ✅ ~~P1 first-backend-only~~ — resolved in **P3** (ordered fall-through; rr-within-type).
  ✅ ~~`Stage.Then` follow-up verb~~ — resolved in **P5** (preempt's no-victim fallback honors
  `then: fallThrough|spill|queue`). Still inert: `quality` as a routing/sort key (**P7**).
- ✅ ~~No `limits`/cost metering~~ — resolved in **P6** (`internal/cost`; energy/paid/swap → $;
  per-request dwell/tokens/$ metered + persisted; sliding-window limits; `requests|dwell|cost`
  share currency). Remaining P6 gaps below.
- ✅ ~~No residency accounting~~ — resolved in **P4** (`pools`/`reserve`/`ramUsage`/`sticky`/
  `persistent` gate spawns + eviction). `swap.loadSeconds`/`loadWatts` now priced (**P6**). Still
  inert: affinity, `server.maxConcurrent` host cap.
- **P6 known gaps:** (1) swap $ is charged to the load *trigger* only — not amortized across the
  coalesced batch; a load whose trigger loses the ctx race goes unbilled. (2) Over-budget with a
  `queue` stage degrades to back-off (reason `over-budget` + Retry-After), not an internal
  budget-wait — the client retries when the window frees. (3) Usage capture caps at 1 MiB; a
  non-streaming reply larger than that meters as $0 (streaming keeps a rolling tail). (4) `cost`
  share-currency is retrospective (decayed past releases), so in-flight cost is invisible to
  fairshare until release.
- ✅ ~~Activity log only / no rollups/UI feed~~ — resolved in **P8**: `recentActivity`/`residency`/
  `usageRollup`/`lanes` ops + activity/usage/lanes views; live SSE events drive updates (15s
  fallback poll). Store carries dwell/tokens/$ per request + a per-model rollup query.
- **Test-teardown race**: a held in-flight request can log after `store.Close()` in one test
  (benign warning); revisit if it becomes flaky.

### Next steps
1. **P7 quality degradation** — make `quality` a sort/routing key for degrade fall-through;
   optional request transforms (clamp `max_tokens`/context) when serving a lower variant. The
   variant-in-list vs separate-fallback-map design question (§7 Still pending) is a USER-owned
   call — surface it before building.
