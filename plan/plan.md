# corrallm — design & roadmap

> Corral + LLM. An OpenAI-compatible reverse proxy + model lifecycle manager +
> priority/fairshare scheduler with cost-aware overflow. Successor in spirit to
> llama-swap (clean-room; reuse *patterns* from redline2, not code).

Status: **P0–P8 + P7 shipped and running in production; P9 (audio modality) scoped,
not started; "Later" (multi-node) remains.** Engine: OpenAI proxy + spawn lifecycle + fairshare scheduler + ordered
fall-through + residency/eviction + preemption + cost model + quality-degrade
routing. Observability + control plane: activity log, residency/pool usage,
per-model + per-key + per-lane cost/usage analytics (bars, line + stacked-area
time-series), queue-pressure + sampled queue-depth (starvation watch), backend
logs with parsed `n_ctx`/`n_slots`, and an Overview control plane (model/lane/cmd
defs, capacity, load/unload) — all live over SSE. **corrallm has replaced
llama-swap in production** (see §8 Deployment). Only open roadmap item: multi-node
("Later") — plus the newly-scoped **P9 audio modality** (OpenAI audio surface +
parakeet STT backend), not yet started. How to work this plan is §0; roadmap is §6; decisions in §7; deploy in §8.

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
> - ✅ **P7 quality-degrade** — `26c8d69`: `quality` is a routing key (best-tier-first
>   walk); per-group opt-in (`acceptDegrade`/`qualityFloor`) gates which tiers a group
>   accepts; per-backend `maxTokens` clamp on degrade. Variant-in-list model chosen.
> - ✅ **P8-beyond** (control plane + observability suite; driven by the live cutover):
>   - lanes live view + SSE events (coalesced) — `adf7483`/`45c93d0`/`d97dcec`
>   - Overview control plane: model/lane/cmd defs, capacity, load/unload, logs — `f5ef5da`/`424b280`/`5bca212`
>   - per-key + per-lane usage analytics (bars, line + stacked-area series) — `6bf224d`/`ed80a2d`/`9579fdd`
>   - queue pressure (429s) + per-request wait + sampled queue depth — `15bda80`/`e576065`
>   - backend log capture + parsed `n_ctx`/`n_slots` — `5bca212`
>   - admin-token auth on `/api/*` + login screen — `3e83001`
>   - calibrated cost coefficients (chat/embed) + activity-log retention — `08ec3ad`/`7f12d48`
>   - cutover hardening (health-timeout/`/health` 2xx/liveness route/EWMA Retry-After
>     + maxWait/maxQueueDepth) — `ca1b5b3`/`21698f2`/`7e96bbf`/`14dd1bd`
> - ◐ **P9 audio modality** — **P9a/b/c/d/e/g ✅ done + validated end-to-end**: `/v1/audio/transcriptions`
>   +`/translations` (parakeet STT), `/v1/audio/speech` (Kokoro TTS), `/v1/realtime` ws passthrough
>   (sherpa-onnx realtime STT adapter), and **P9g diarized batch STT** (`diarize` model: sherpa-onnx
>   offline diarization + offline ASR → speaker-labeled transcript). Backends installed under ml-kit
>   `local/` + wired + full-stack tested; P9e idle/max-session reaper ✅. Only remaining: **P9f**
>   (comfort-fill, unconfirmed/parked).
> - ✅ **P11 discovery + console** — `/v1/capabilities` manifest; per-model console (Info/Test/Logs/Usage)
>   with chat/STT/TTS/vision playgrounds; STT playground gates batch/realtime per `model.modes`; batch
>   STT renders speaker-labeled segments when a backend returns them; replay a logged activity in-console.
> - ✅ **P10 request observability** (prod-driven) — **P10a/b/c ✅** honest error status (client/upstream
>   cancel → 499, not a mislabeled backend 502) + `activity.error` reason + configurable
>   `--request-timeout` (latent 130s cap removed); per-request payload + TTFB capture; click-through
>   detail modal (error/timing + request/response payloads).
> - ☐ Later: multi-node peer awareness.
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
  Quality carried but not yet a sort key (list order authoritative); per-quality routing landed in P7.
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
- ✅ **P7 — Quality degradation.** `26c8d69`.
  - [x] `quality` is a sort/routing key: `orderBackends` walks best-quality-tier first
        (type-rr preserved within a tier; uniform quality = pre-P7 ordering, no regression).
  - [x] Per-group opt-in: `acceptDegrade` + `qualityFloor` gate accepted tiers
        (`PriorityGroup.AcceptsQuality`); a non-degrading group sees only the top tier and
        backs off per its stage instead of spilling onto a worse model.
  - [x] Request transform: per-backend `maxTokens` clamp applied to the outgoing body when a
        request degrades onto a capped backend. *(Context-window clamp needs tokenization — §7.)*
  - [x] Resolved: **variant-in-list** (one ordered list, quality-ranked), not a separate map.
- ✅ **P8 (MVP slice) — UI / observability.** `dc9ffd3`/`b7d8dcc`/`b7e1b92`.
  - [x] `recentActivity` GraphQL/REST op + `/activity` polling table (dwell/tokens/$).
  - [x] Residency read op (`Manager.Snapshot`: pool budget/used + resident backends) +
        `/usage` view (per-server pool-utilization bars + resident-model table).
  - [x] `usageRollup` op (per-model requests/tokens/dwell/$ over a window) + a 24h
        summary + per-model rollup table on the Usage page.
- ✅ **P8-beyond — observability + control plane.** Grew well past "polish": driven
  by the live llama-swap → corrallm cutover (§8), it's now the operator surface.
  - [x] **Lanes live view** — `Scheduler.Snapshot` → `lanes` op: groups
        (weight/currency/interruptible + live active/waiting) + backend health/util. `adf7483`
  - [x] **Live SSE events** — `internal/events` broker → `/api/v1/events`; proxy
        publishes activity/changed, UI invalidates on push (300ms coalesced throttle,
        15s fallback). *(SSE not WebSocket — see Status deviations.)* `45c93d0`/`d97dcec`
  - [x] **Overview control plane** — `overview` op: model + spawn-`cmd` defs (auth
        headers redacted, cmd behind a modal), lane defs, declared capacity; per-model
        `loadModel`/`unloadModel` mutations (`Manager.LoadModel`/`UnloadModel`; rails:
        spawnable-only, never pinned or in-flight) + Open-UI links. `f5ef5da`/`424b280`
  - [x] **Per-key usage** (`usageByKey`) + **time-series** (`usageSeries`): cost/
        requests/energy/time per caller key — bars + dependency-free SVG line charts. `6bf224d`/`ed80a2d`
  - [x] **Per-lane analytics** (`usageSeriesByGroup`, resolves key→group): stacked-area
        throughput + 429-rejections + avg queue-wait — priority-starvation watch. `9579fdd`/`15bda80`
  - [x] **Queue-depth sampler** — 5s background snapshot → `lane_samples` → `queueDepth`
        op: instantaneous per-lane waiting/active over time (pre-resolution pressure;
        48h retention, pruned). `e576065`
  - [x] **Backend logs + introspection** — per-process stdout/stderr ring (`logBuffer`)
        tee'd from the spawn, parsed `n_ctx`/`n_slots`, `modelLogs` op + live logs dialog. `5bca212`
  - [x] **Cutover hardening** (llama-swap parity, §8) — configurable `--health-timeout`
        `ca1b5b3`; readiness waits for `/health` 2xx so a cold load doesn't 503 `21698f2`;
        plain `/health`+`/healthz` liveness route `7e96bbf`; EWMA Retry-After + `maxWait`/
        `maxQueueDepth` queue bounds (the fork's good-citizen 429 contract) `14dd1bd`.
  - [x] **Auth** (`internal/auth`) — admin token in `<home>/admin.token` (auto-generated)
        gates all `/api/*` (ops + load/unload) via Bearer or cookie; `/v1`, `/upstream`,
        `/health` stay open. Dashboard login screen points to `home/admin.token`. `3e83001`
  - [x] **Retention / compaction** — `--activity-retention` (default 30d) prunes the activity
        log in the 5-min maintenance tick (it grew unbounded; only `lane_samples`/48h was pruned).
        SQLite reuses freed pages → file plateaus, no VACUUM. `7f12d48`

> **── MVP line ──** Above: P0–P6 + the P8 MVP slice = a usable, observable control
> plane. Below: post-MVP polish, reorderable.

- ☐ **P9 — Audio modality (OpenAI audio surface + parakeet STT backend).** Extend the
  request edge beyond JSON/text to OpenAI's audio API, with **parakeet**
  (`achetronic/parakeet` — Whisper-compatible ASR, NVIDIA Parakeet-TDT 0.6B ONNX, **STT-only**,
  a spawnable Go binary that binds a port → ordinary `cmd` backend) as the first concrete STT
  backend. **Audio backends are ordinary backends** — they spawn, health-check, draw pool
  budget, take fairshare slots, fall through, preempt, and meter exactly like text backends
  (P1–P7 reused unchanged). What's new is only the **request shape** (multipart-in,
  binary/SSE-out) and the **cost basis** (audio replies carry no token `usage`). Modality is
  **inferred from backend `type`** (an audio cost class), not a new config field. Split into
  shippable sub-units:
  - ✅ **P9a — Multipart request edge + STT routing.** Done (not yet committed). `resolveRequest`
    forks on Content-Type: JSON → existing `modelFromBody`/`streamFromBody`; `multipart/*` →
    `modelFromMultipart` reads the `model`+`stream` form fields from the buffered body (skipping
    the file part — `NextPart` streams past it) and the whole body replays to upstream intact.
    `/v1/audio/transcriptions` + `/v1/audio/translations` mounted through the same scheduler →
    residency → ordered-walk → reverse-proxy pipeline; audio routes get a 64 MiB body cap (vs
    32 MiB; parakeet caps audio at 25 MiB). SSE transcription deltas ride the existing streaming
    passthrough (`statusCapture`). No `audio` cost-class needed yet — an unpriced `type` already
    meters $0 via `cost.RequestUSD` (real coeffs land in P9c). Tests: `TestAudioTranscriptionMultipart`
    (e2e multipart extract + replay + activity log), `TestModelFromMultipart` (field/stream/empty-boundary),
    `TestAudioTranscriptionStreaming` (SSE `transcript.text.delta` passthrough: in-order, flushed not
    buffered, per-token `logprob` confidence preserved — the first streaming test for any route).
    `go build`/`vet`/`test -race` green, gofmt clean.
    *Known gap (→ P9c): metering is token-based, so audio meters $0 until the byte-basis cost path
    lands. The 130s request timeout is unchanged — fine for ≤25 MiB; revisit if long-audio jobs appear.*
  - ✅ **P9b — TTS endpoint (`/v1/audio/speech`).** Done (not yet committed). **Backend decided:
    Kokoro** (`remsky/Kokoro-FastAPI` v0.5.0, Apache-2.0, CPU). `/v1/audio/speech` mounted through
    the same pipeline; JSON-in (model resolves via the existing JSON path), **binary-audio-out**
    streamed through untouched. Metering forks on a `tts` route flag: TTS is **costed by OUTPUT
    bytes** (`statusCapture.written` — the synthesized audio; its JSON input is tiny), vs STT's
    input bytes — the binary reply is never parsed as JSON `usage`. A `tts` type declaring audio
    coeffs auto-flags `modality: audio` (reuses P9d). Test: `TestAudioSpeechTTS` (binary
    passthrough byte-for-byte incl. a `0x00`, output-byte metering, 0 tokens). `go test -race`
    green. Installed Kokoro under ml-kit `local/` (uv venv + torch CPU + 327MB weights + 67
    voices); smoke-tested healthy on :8880, and full audio loop proven (text→Kokoro→mp3→parakeet
    STT round-trips) + full stack (curl→corrallm→cold-spawned kokoro, metered `audio_bytes`=mp3 size).
  - ✅ **P9c — Audio cost model (file-bytes basis).** Done (not yet committed). Audio replies carry
    no token `usage`, so `cost.AudioRequestUSD(typ, bytes)` costs by **byte size**: a local type
    bills `audioWhPerMiB` (→ kWh × `costPerKwh`), a paid type bills `audioUSDPerMiB` directly; an
    unpriced type stays $0. New `commandCosts` audio coeffs (`audioWhPerMiB`/`audioUSDPerMiB`).
    `handleInference` forks metering on the `audio` route flag — STT bills `len(body)` (uploaded
    audio + small multipart overhead) instead of token usage. New `activity.audio_bytes` column
    (schema + forward-only migration, like `queued_ms`); `Activity.AudioBytes` persisted +
    threaded through `p.log`. Tests: `cost.TestAudioRequestUSD` (local/paid/unpriced/zero) +
    `proxy.TestAudioTranscriptionMetering` (0 tokens, `audio_bytes` = body len, byte-based $).
    `go build`/`vet`/`test -race` green, gofmt clean.
    *Scope notes: TTS char/output-byte costing wires up when P9b lands (the byte path already
    covers TTS output bytes); true-duration costing deferred (would parse `verbose_json`/SRT or add
    ffprobe — §7 Optional extensions). Rollup/usage SUMs of `audio_bytes` are P9d's UI concern.*
  - ✅ **P9d — Catalog + observability.** Done (not yet committed). **Modality decided this
    session: inferred from cost class** (a backend `type` declaring audio coeffs is audio; a model
    is audio iff any backend uses one — `cost.IsAudioType`). `/v1/models` (`handleModels`) and the
    `overview` op (`ModelDef.Modality`) now carry `text|audio`; `recentActivity` exposes
    `audioBytes`. UI: Overview model card shows an `audio` badge; the Activity table adds an Audio
    (bytes) column and renders prompt/completion as `—` for audio rows (tokens N/A). `bin/gen`
    re-run → SDL snapshot (`ui/gen/schema.graphql`) updated with `audioBytes: Long!` + `modality:
    String!`; codegen/eslint/tsc/`vite build` clean. Tests: `cost.TestIsAudioType`-via-metering,
    `api.TestOverviewAudioModality` + `TestOverview` (text), `TestRecentActivity` (audioBytes
    carried). `go test -race ./...` green, gofmt clean.
    *Deferred to opportunistic polish: per-`audio_bytes` SUMs in the rollup/usage ops
    (`usageRollup`/`usageByKey`/`usageSeries`) — activity rows + catalog cover the P9d goal.*
  - ✅ **P9e — Realtime WebSocket passthrough (live/conversational transcription).** **Done +
    validated end-to-end.** New `handleRealtime` (`/v1/realtime`): model from `?model=` query,
    ordered-backend admission holding **one slot for the session**, then `proxyWebSocket` — a manual
    hijack + bidirectional `io.Copy` (the request body is NOT buffered). Preemption teardown is
    explicit: a `<-ctx.Done()` goroutine closes both conns when the slot is reclaimed (`ErrPreempted`
    → session logged 499) — the flagged "does cancel fire on a hijacked conn" risk is **verified by
    test**. Metered by client→backend bytes (audio in) → `AudioRequestUSD`; one activity row on close
    (dwell = session). **Metering correctness fix:** wait for *both* copy directions before reading
    the byte count (reading after one side closed raced the counter → undercount/0). Tests:
    `TestRealtimeWebSocketPassthrough` (raw-conn 101 upgrade + bidirectional echo, no ws client dep) +
    `TestRealtimePreemptAbortsSession`. `go test -race` green.
    **Backend: Speaches** (speaches-ai/speaches v0.8.3, CPU faster-whisper int8) installed under
    ml-kit `local/`, wired into the config (served name = model id; `LOOPBACK_HOST_URL` required, model
    pulled once via `POST /v1/models/…`). **Full stack validated:** ws client → corrallm → Speaches →
    "And so my fellow Americans." + metered `audio_bytes`=501712. *(Speaches realtime VAD over-segments
    a blasted synthetic stream + a transient "item already exists" — app-layer, cleaner at real-time
    mic pace.)* **Idle/max-session reaper ✅** — a background ticker watches live byte counts
    (`countingWriter`, both directions) and closes a session silent past `--realtime-idle-timeout`
    (default **5m**, `CORRALLM_REALTIME_IDLE_TIMEOUT`) or longer than `--realtime-max-session`
    (default off); a reaped session frees its slot and logs **408** with `idle timeout`/`max session`.
    `SetRealtimeTimeouts`; test `TestRealtimeIdleReaper`. **P9e fully done.**
    <!-- original scope retained below -->
    A **separate request edge** from P9a's file model: live mic transcription (OpenAI Realtime,
    `wss://…/v1/realtime?model=…`) streams audio *in* continuously, so it **must not buffer the
    request body** the way `handleInference` does (`proxy.go:97`). New `handleRealtime` that
    **upgrades** the connection and lets the reverse proxy raw-copy bytes both ways (Go 1.26's
    `httputil.ReverseProxy` already handles `Connection: Upgrade` — `newReverseProxy` works once
    we skip the body read). **corrallm stays a transparent ws byte-pipe** with a clear division of
    responsibility (confirmed by the user): **device/mic capture is the CLIENT's job** (corrallm
    never manages live audio devices), and **VAD / overlap-window / commit-stitch (LocalAgreement)
    is the BACKEND's job** (Speaches — the chosen backend — / sherpa-onnx / etc.) — corrallm does
    **neither**; it doesn't decode audio or tokenize (§7). It only upgrades, routes, schedules,
    meters, and tears down. **Decided (§7):** standardize the wire on the **OpenAI Realtime
    transcription schema** and ship **Speaches** as the native-passthrough default (CPU, MIT) — true
    byte-pipe; custom-protocol backends get a thin adapter instead. (Installed batch Parakeet-TDT
    can't stream, so realtime is a *different* backend.) What's new vs P9a:
    - **Model resolution from the query string** (`?model=`) — a third source after JSON body
      field (chat) and multipart form field (P9a). Generalize `resolveRequest` accordingly.
    - **Long-lived slot lifecycle** — a session holds one fairshare slot for its whole duration;
      **`dwell` share currency** (P6) is the honest cost, not request-count. The 130s request
      timeout must NOT apply — replace with an **idle/max-session timeout + reaper**. Preemption
      reuses P5: `Admit`'s `reqCtx` cancel (cause `ErrPreempted`) tears down the upgraded conn and
      frees the slot — streaming-safe cancel already proven for SSE; verify it fires on a hijacked
      conn. Metering: no token usage in ws frames → meter **dwell + $** (session seconds/bytes,
      P9c byte-basis); persist as one activity row at close.
    - Mount `/v1/realtime` as the **scheduled** realtime path (distinct from the untracked
      `/upstream/*` bypass, which is unscheduled).
    - *Requires a realtime/ws ASR backend — parakeet is file-only, so like P9b's TTS this ships the
      passthrough and the concrete backend is a separate decision (§7).*
    - *DoD: 101 Switching-Protocols upgrade + bidirectional byte passthrough test (raw-conn echo
      upstream, no ws client dep), slot held-for-session then released on close, preempt-aborts-session
      test. `go build`/`vet`/`test -race` green.*
  - ☐ **P9f — Conversational grace / comfort-fill on contention** (depends on P9e; optionally P9b
    for TTS-generated fillers). When a **speech-OUT** realtime session can't be admitted immediately
    or is preempted, mask the delay instead of stalling/cutting — keyed to corrallm's already-computed
    expected delay (Retry-After EWMA + cold-load time): micro (<~300 ms) → nothing; short (~0.3–2 s) →
    injected disfluency ("um", "one moment"); long (>~2 s) → spoken "hold on…" + hold music, session
    **parked** (not killed) and resumed on free. **Explicit, scoped exception to "transparent
    passthrough"** — corrallm *synthesizes/inserts* audio, justified because it's the only layer that
    knows the delay. Only applies to conversational (speech-out) sessions, not transcription-only.
    Start with **pre-recorded canned clips** (deterministic, no TTS dependency); TTS-generated fillers
    later. *Not confirmed by the user yet — parked pending the transparency-tradeoff call.*

  - ✅ **P9g — Diarized batch STT (speaker-labeled transcript).** Done + validated. The offline
    half of the realtime/batch split: realtime-stt streams partials but has **no speakers** (stable
    IDs need the whole utterance); `diarize` is batch-only and returns them. **Service**
    (`examples/sherpa-diarize/diarize.py`, deployed to ml-kit `local/src/sherpa-diarize/`): aiohttp,
    OpenAI-shaped `POST /v1/audio/transcriptions` — ffmpeg-decode any container → 16k mono f32 →
    sherpa-onnx **OfflineSpeakerDiarization** (pyannote-segmentation-3-0 + wespeaker_en CAM++ +
    FastClustering) + **offline zipformer** (gigaspeech int8) ASR → align tokens to speaker segments by
    timestamp → `{text, segments:[{speaker,start,end,text}], num_speakers, duration}`. Plain OpenAI
    clients read `.text`; the console's BatchStt renders the speaker-labeled segments (per-speaker color
    chips + timestamps). Wired in ml-kit `corrallm.yaml` as model `diarize` (type `stt`, `modes:[batch]`,
    proxy :5805, sticky 300s). **corrallm code unchanged** beyond the UI — it's just another stt backend.
    *Validation:* full proxy path = cold spawn → diarize → metered (status 200, `audio_bytes`, byte-basis
    cost). Diarization QUALITY: pyannote **segmentation** is accurate (clean turn boundaries on
    silence-gapped audio); **clustering** separates **real** voices well (thr=0.6 ⇒ correct count on a
    4-speaker reference) but **not synthetic TTS** (voxceleb embeddings don't separate kokoro timbres —
    documented in the README; validate with real recordings or pass `NUM_SPEAKERS`). Default
    `CLUSTER_THRESHOLD=0.6` (real-audio accurate), env-tunable. *Side fix (P10b metering):
    `statusCapture.WriteHeader` now skips interim **1xx** — large uploads sending `Expect: 100-continue`
    were logging status **100** instead of the final 200.*

  **P9 reuse note:** scheduler/residency/preemption/fairshare/limits need **no changes** — an
  audio backend is a `cmd`+`proxy` entry with a `type`, slots, and pool `ramUsage` like any
  other. The blast radius is `internal/proxy` (routing + multipart fork + binary metering),
  `internal/cost` (byte-basis path), `internal/store` (one metered column), and the catalog/UI.

- ◐ **P10 — Request observability & honest errors.** Driven by a production incident: qwen requests
  logging **502s**. Diagnosed (DB + proxy log): a long request (big prompt / image data on the
  27B/220k-ctx model) outruns a **~120 s timeout *upstream of corrallm*** (the `llm.iodesystems.com`
  front proxy and/or the client), which drops the connection — `http: proxy error: context canceled`,
  dwell ≈120 s, 0 tokens. llama-server itself is healthy. corrallm was **mislabeling** the
  client/upstream cancel as a backend 502, and its own fixed **130 s** request cap was a latent
  second guillotine.
  - ✅ **P10a — Honest status + error reason + configurable timeout.** Done (not yet committed). The
    reverse proxy now has an `ErrorHandler` that captures the failure and maps it: connection
    canceled (client/front-proxy gave up) → **499**, corrallm's own deadline → **504**, genuine
    backend dial/transport error stays **502**; preemption stays 499. The reason string is captured
    into a new `activity.error` column (schema + forward-only migration), exposed on `recentActivity`,
    and shown as a **tooltip on the status chip** in the Activity table. The hard 130 s cap is gone —
    new `--request-timeout` (`CORRALLM_REQUEST_TIMEOUT`, default **0 = no corrallm deadline**, defer
    to client + backend; `SetRequestTimeout`). Tests: `TestClientCancelLogged499` (the exact 502→499
    repro) + `TestRequestTimeout504`. `go test -race ./...` green; UI tsc/build clean.
    *Does NOT fix the failures themselves — the real ~120 s cap is upstream (raise the front-proxy
    `proxy_read_timeout` / client timeout). Streaming (`stream:true`) also masks it: chunks reset the
    read timeout. corrallm's job here is honest reporting + not being a second cap.*
  - ✅ **P10b — Per-request payload + timing capture.** Done (not yet committed). New activity columns
    `req_body`/`resp_body`/`ttfb_ms` (schema + forward-only migrations). `p.log` refactored to take a
    `store.Activity` (was 12+ positional args). Request payload captured once on every exit path;
    **STT multipart uploads + TTS binary output are summarized to `<content-type, N bytes>`, never
    stored raw**; text capped at 4 KiB. TTFB = first-response-byte time (`statusCapture.firstWrite`).
    `--capture-payloads` / `SetCapturePayloads` toggle (default on; payloads are user data, admin-gated,
    pruned with `--activity-retention` → "discard on compaction"). `id`+`ttfbMs` exposed on the lean
    `recentActivity` list; payloads only via `ActivityByID`. Tests: `TestPayloadCapture` (capture +
    disable) + `TestPayloadCaptureBinaryAudio` (summarized, no raw bytes) + store round-trip.
  - ✅ **P10c — Activity detail modal (UI).** Done (not yet committed). New `activityDetail(id)` op
    (`/api/v1/activity/detail`) returns the full row + payloads on demand (list stays lean). UI: rows
    are clickable → MUI `Dialog` showing served/backend/path, **error + timing (dwell/ttfb/queued/$)**,
    and **request + response payloads** (monospace, scrollable). SDL regenerated; tsc/eslint/`vite
    build` clean.

- ◐ **P11 — Capabilities/discovery + model detail.** A self-describing surface so an LLM/client can
  build a compatible client, and so UI-less models (parakeet/kokoro/speaches have no web UI) are
  still inspectable from the dashboard.
  - ✅ **P11a — `/v1/capabilities` manifest.** Done. Public (unauthenticated, like `/v1/models`),
    synthesized from config, **never exposes API keys**. Returns: the OpenAI endpoint surface with a
    runnable example each (curl, + the realtime ws session flow), models grouped by **capability**
    (inferred from cost class — `capabilityForType`: chat/embeddings/audio.stt/audio.tts/rerank), and
    the fairshare **lanes** (name/weight/currency/interruptible — policy only). Test
    `TestCapabilitiesManifest` (grouping, endpoint coverage, lanes, **key-leak assertion**).
  - ✅ **P11b — Disabled "Open UI" for UI-less models.** Done. The proc manager probes the backend
    root once when ready (spawned backends only; async, never gates readiness) and caches `hasUI`
    (yes/no/unknown). Exposed on the residency `ResidentModel`/`ResidentModelView`; the Overview
    Open-UI button is disabled with a "no web UI" tooltip when `hasUi === "no"`. Test `TestProbeUI`.
  - ✅ **P11c — Model console.** Done. New `/model?name=` route (`ui/routes/model.tsx`) reached from a
    "Console" button on each Overview model card. Tabs: **Info** (modality/capability/state chips,
    backends table + spawn cmd, the `/v1/capabilities` examples for this model, Open-native-UI or a
    "no web UI" chip), **Test** (the P11d playgrounds by capability), **Logs** (`modelLogs`), **Usage**
    (`usageRollup` 24h). Makes UI-less models fully inspectable. tsc/eslint/build clean.
    *(Deploy note: queries the new `hasUi` field, so the prod dashboard needs a `bin/run` rebuild — a
    new-field UI change isn't binary-compatible with the old gateway.)*
  - ☐ **P11d — In-dashboard test playgrounds** (user's vision). Since not all backends ship a native
    UI, let the dashboard *drive* each model by capability, using browser Web APIs:
    - **chat** — a chat playground; **MUST use `flex-direction: column-reverse`** for the message
      list (user preference — auto-pins newest, no scroll management). Streams via `stream:true`.
    - **audio (STT↔TTS loop)** — mic capture (MediaRecorder / Web Audio) → `/v1/audio/transcriptions`
      (or `/v1/realtime` ws) → optionally pipe the text → `/v1/audio/speech` → speaker playback. A
      full voice loop in the browser.
    - **image/vision** — image upload → chat with image content parts (for multimodal models).
    Decided: **consolidated model console** (tabs Info+Examples · Logs · Usage · Test); build all
    three playgrounds **chat → voice → image**; playground `/v1` calls default to the **default lane**
    with a lane picker.
    - ✅ **chat** — streaming chat playground in the Test tab; **flex column-reverse** message list
      (newest pins to bottom), SSE delta parsing, optional lane-key field. Built + typechecked (not
      yet live-smoke-tested against a chat backend).
    - ✅ **voice (STT↔TTS loop)** — Test tab for audio models. STT: mic capture
      (`getUserMedia`+`MediaRecorder`) → `/v1/audio/transcriptions` → transcript, then **"speak it
      back"** via a chosen TTS model → `/v1/audio/speech` → `Audio()` playback (a full browser voice
      loop). TTS: text → speak. tsc/eslint/build clean.
    - ✅ **image/vision** — image attach (🖼) in the chat playground: file → base64 data URL → the
      user turn is sent as OpenAI multimodal content-parts (`text` + `image_url`) for vision models;
      thumbnails render inline. tsc/eslint/build clean.
    - ✅ **STT/TTS clarified + batch/realtime + dispatch fix** — `config.Capability` keeps STT vs TTS
      DISTINCT (never lumped "audio"); fed to `/v1/models` (new `capability`), the `overview` op, and
      UI badges (`capLabel`: stt/tts/embed). The console dispatches the playground from the model's own
      `capability` (not the async `/v1/capabilities`), fixing a race that briefly showed a chat box for
      parakeet (backend verified fine: webm→200). STT playground gains a **Batch (record→upload) /
      Realtime (live ws PCM16@24k)** toggle + a secure-context (https) mic guard; the upload part is
      named by the real MediaRecorder mime so the backend demuxes across browsers.
  - ✅ **P11e — Replay an activity into the console.** Done. The activity detail modal (P10c) gains a
    **"Replay in console"** (chat paths) / "Open in console" button → navigates to
    `/model?name=<served>&replay=<id>`. The console opens the Test tab and the chat playground fetches
    `activityDetail(id)`, parses the captured `reqBody.messages` (incl. multimodal content-parts via
    `extractText`), loads prior turns as history, and drops the last user turn in the input to re-run/
    tweak. Audio rows only stored a size summary, so they just open the console. tsc/eslint/build clean.

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
- ✅ **Quality-degrade model** (P7) — **variant-in-list** (one ordered backend list, quality-ranked),
  not a separate fallback map. Degrade is **per-group opt-in**: `acceptDegrade` + `qualityFloor`
  decide which quality tiers a group accepts; a non-degrading group sees only the model's top tier
  and backs off per its stage rather than spilling onto a worse model. Degrade transform = per-backend
  `maxTokens` clamp on the outgoing request (context-window clamp deferred — needs tokenization).

### Resolved this session (P9 scoping)
- ✅ **Audio cost basis** — **file bytes** for v1 (deterministic, no extra dependency): STT $ by
  uploaded-audio bytes, TTS $ by `input` chars / output bytes. True-duration costing (parse
  `verbose_json`/SRT or add ffprobe) deferred to Optional extensions.
- ✅ **TTS scope** — **STT now, TTS endpoint stub**: land transcriptions/translations + parakeet
  fully (P9a/c/d); mount `/v1/audio/speech` wired to a configured remote/future TTS backend, optional
  and untested until one is chosen (P9b). No TTS engine selection blocks the phase.
- ✅ **Modality source** (P9d) — **inferred from cost class**: a backend `type` that declares audio
  coeffs (`audioWhPerMiB`/`audioUSDPerMiB`) is an audio type; a model is `audio` iff any backend uses
  one (`cost.IsAudioType`). Zero new config field. Known limitation: an audio model left **unpriced**
  won't be flagged — pricing it (which production should) flags it. Revisit with an explicit optional
  `modality` override only if an unpriced-audio case appears.

### Still pending (P9 — surface before starting the sub-unit, don't guess)
- ✅ **Multipart buffering strategy** (P9a) — **bounded in-memory buffer** (matches the JSON path,
  which already buffers the whole body at `proxy.go:85`); bound = 64 MiB × concurrent audio slots,
  fine on the 5090 box. Revisit (temp-file spool / stream-tee) only if audio concurrency grows.
- ✅ **Concrete TTS backend** (P9b) — **Kokoro** (`remsky/Kokoro-FastAPI`, Apache-2.0, CPU,
  native `/v1/audio/speech`, ~35–100× realtime on CPU, <2 GB). Picked over VibeVoice (CUDA-only on a
  full GPU, no turnkey OpenAI server, watermark/disclaimer, MS deprioritized it). **Chatterbox** (MIT,
  cloning, 4–8 GB) is the parked "quality" option for when GPU headroom exists.
- ✅ **Realtime ASR contract + backend** (P9e) — **standardize `/v1/realtime` on the OpenAI Realtime
  *transcription* schema** (de-facto standard; every OpenAI SDK speaks it). **Backend RESOLVED →
  sherpa-onnx via a native adapter** (`examples/sherpa-realtime-adapter`). Speaches was the first pick
  but its realtime *transcription* mode is **broken** (fires response-generation per utterance and 500s;
  ignores `create_response:false` — it's a speech-to-speech server). Parakeet-TDT is **batch-only**.
  The adapter speaks the OpenAI Realtime schema **natively** (corrallm passes through unchanged) and runs
  **sherpa-onnx streaming zipformer** inside — TRUE streaming (live `delta`s + silence endpointing, CPU
  int8). Validated full-stack: client → corrallm → adapter → live partials + finals, metered. Diarization
  NOT included (sherpa diarization is offline-only). *(Original Speaches plan retained below for history.)*
  Default backend:
  **Speaches** (ex faster-whisper-server, MIT, CPU, native `/v1/realtime?intent=transcription`) →
  true byte-passthrough, corrallm's transparent design holds. Custom-protocol backends (sherpa-onnx,
  WhisperLive) would need a thin adapter (base64-JSON↔binary-PCM transcode, auth, interim→`delta`/
  stable→`completed`, synth-VAD). **The installed batch Parakeet-TDT does NOT stream** (full-attention
  FastConformer) — realtime can't reuse it; Speaches (Whisper) or sherpa-onnx (both CPU) are the fits.
- ✅ **Realtime slot model** (P9e) — **one fairshare slot per live session, held for its duration,
  `dwell` currency, preemptible, and parkable in the background** (on preempt: park + resume when a
  slot frees, don't hard-kill). Idle/max-session timeout replaces the 130s request cap.

### Optional extensions (improve the product; no planned phase requires them — pull in opportunistically)
- **Stickiness/affinity weighting** — how strongly a warm backend overrides *ordered list*
  preference (P4 does ttl/evictCost for *eviction*, but the proxy walks strict quality/list order
  regardless of warmth); per-group vs per-request latency hint. Not built.
- **Context-window clamp on degrade** — P7 clamps `max_tokens`; clamping the prompt to a smaller
  backend's context window needs tokenization, so it's deferred (declared `maxTokens` only for now).
- **gRPC surface** — gat gives it cheaply, but no consumer yet; add when one appears.
- **CapacityProbe** (nvidia/drm/amd/metal/none, auto) — declared budget is canonical and
  implemented; the probe only auto-fills undeclared totals, drift-guards, and feeds dashboards.
- **`server.maxConcurrent` host cap** — per-backend slots enforced (P2); the host-wide concurrency
  ceiling parses but isn't enforced yet (layer onto residency).
- **Proactive ttl reaper** — P4 eviction is lazy (on demand); `ttl` only orders victims. A
  background reaper that frees warm-but-expired models for power is not built.
- **Dynamic footprint** — KV scales with slots×context; v1 reserves worst-case `ramUsage`;
  refine with `{base, perSlot}` later.
- **Audio true-duration costing** (post-P9) — P9c costs audio by bytes; cost by actual seconds
  needs duration: parse parakeet `verbose_json`/SRT, or add a local `ffprobe` dependency. Refine
  the byte-basis once P9 is live and the byte→$ error matters.

### Deferred (out of scope until later)
- **NUMA / interconnect** — per-NUMA system pools, PCIe/NVLink cost of multi-GPU splits.
- **Multi-node peer awareness** — remote load introspection across corrallm peers (roadmap "Later").

### Deferred work / known gaps in shipped code
- ✅ ~~P1 first-backend-only~~ — resolved in **P3** (ordered fall-through; rr-within-type).
  ✅ ~~`Stage.Then` follow-up verb~~ — resolved in **P5** (preempt's no-victim fallback honors
  `then: fallThrough|spill|queue`). ✅ ~~`quality` inert~~ — resolved in **P7** (routing key +
  per-group degrade opt-in + `maxTokens` clamp).
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
- **P8-beyond known gaps / OSS pre-reqs:**
  (1) ✅ ~~`/api` unauthenticated~~ — resolved (`3e83001`): admin token (`<home>/admin.token`) gates
  `/api/*` incl. load/unload, via Bearer or cookie; `/v1`/`/upstream`/`/health` stay open.
  *(Single shared admin token — no per-user accounts/roles/rotation yet; fine for one operator.)*
  (2) ✅ ~~Cost coefficients are placeholders~~ — calibrated (ml-kit config): split into `chat`
  (Qwen: ~400W ÷ 83 gen, ÷2300 prompt tok/s → gen 0.0013 / proc 0.00005 Wh/tok) and `embed`
  (nomic: single pass → proc 0.000002, gen 0). Verified live: chat ≈ $0.0000068, embed ≈ $0.00000007.
  Re-measure if hardware/models change. *(Field name still says "WattsPerToken" but is Wh/token —
  cosmetic rename deferred.)*
  (3) **`interactiveOrigins` not ported** — llama-swap's browser-origin auto-priority has no corrallm
  equivalent; browser callers land in `default` unless keyed (design choice — priorityGroup is first-class).
  (4) **`queued_ms` is forward-only** — rows predating the column read 0; queue *wait* populates as new
  queued-then-served requests accumulate (rejections + sampled depth are already live).

### Next steps
- The full P0–P8 + P7 roadmap is shipped and live (§8). `/api` auth landed (`3e83001`) and cost
  coefficients are calibrated (per-backend `chat`/`embed` Wh/token, verified live). Open work:
  1. **P9: audio modality** (in progress) — **P9a/c/d (STT) + P9b (TTS) ✅ done**: parakeet STT +
     Kokoro TTS, both routed/metered/flagged, installed under ml-kit `local/`, validated full-stack.
     Remaining: **P9e** (realtime ws passthrough — backend decided: Speaches on the OpenAI Realtime
     schema; all decisions resolved, ready to build) and **P9f** (comfort-fill on contention —
     unconfirmed, parked pending the transparency-tradeoff call).
  2. **P10: request observability** ✅ **done** (P10a honest errors + timeout; P10b payload+TTFB capture;
     P10c detail modal). NOTE: the actual qwen failures still need the **upstream** ~120 s timeout raised
     (front proxy / client) — outside corrallm; and a production rebuild/restart (`ml-kit/bin/run`) to
     pick up P10.
  3. **Later: multi-node peer awareness** — remote load introspection across corrallm peers.
  - OSS follow-ups (not blockers): auth multi-user accounts/roles + token rotation (today is a single
    shared admin token); rename the `WattsPerToken` cost fields to `WhPerToken`.
- Optional polish in §7 Optional extensions (affinity weighting, context-window clamp on degrade,
  gRPC, CapacityProbe, `server.maxConcurrent` host cap, proactive ttl reaper, instantaneous queue
  depth is now covered by the sampler).

---

## 8. Deployment (production cutover)

corrallm **replaced the llama-swap fork on `:8111`** for the live workload. The deployment lives in
the **ml-kit** ops repo (sibling), not this code repo:
- **`ml-kit/corrallm.yaml`** — the production config, translated from `ml-kit/llama-swap.yaml`:
  two models (`nomic-embed-text` persistent/preloaded; `Qwen3-6-27B-MPT` sticky), absolute
  llama-server paths, fixed ports (5800/5801), fairshare groups (`aw3`→interactive=10,
  `ragtag`→batch=1, default=5), `scheduler.maxWait 60s`/`maxQueueDepth 8`. Pool budget reflects the
  real RTX 5090 (~32GB): Qwen `gpu0 29.5GB` + nomic `gpu0 1.5GB` (nomic offloads to GPU despite no
  `-ngl`). `commandCosts` are calibrated per type — `chat` (Qwen) vs `embed` (nomic), measured on the
  5090 (§7 gap 2); `/api` is gated by the `home/admin.token` admin token.
- **`ml-kit/bin/run`** — adapted from the llama-swap launcher: builds corrallm fresh from this repo
  (`go build` → repo `bin/corrallm`, gitignored), frees `:8111`, runs `serve` with
  `--health-timeout 600s` (matches llama-swap; Qwen's 220k-ctx cold load is ~66s). Supports
  `--detach` (setsid + `tmp/corrallm.pid`/`tmp/corrallm.log`; stop via `kill -- -$(cat tmp/corrallm.pid)`).
- The dashboard is fronted at **`https://llm.iodesystems.com`** (reverse proxy); SSE verified flowing
  through it (no buffering).
- **Build/run model:** corrallm is build-once-run (no hot-reload — `air` would thrash spawned model
  backends). UI changes are served from `ui/dist` per-request (browser reload picks them up unless a
  new GraphQL op needs the new binary); backend changes need a `bin/run` rebuild+restart.
- **Restart drill:** stop (`kill -- -<pid>`), wait for `:8111`/5800/5801 to free (~10s graceful reap),
  then `bin/run --detach`. Blip: in-flight requests drop, Qwen cold-reloads (~66s).
- **DB / retention:** SQLite at `ml-kit/local/corrallm.db`. Activity is pruned to 30d
  (`--activity-retention`); `lane_samples` to 48h. After the cost calibration, historical `cost_usd`
  was recomputed in place from stored tokens × the new `chat`/`embed` coefficients (one-time backfill,
  stop → backup → `UPDATE` → restart) so the 24h dashboard wasn't stuck on pre-calibration totals.
