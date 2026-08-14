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
> - ◐ **P9 audio modality** — **P9a/b/c/d/e/g ✅**: `/v1/audio/transcriptions`+`/translations` (STT),
>   `/v1/audio/speech` (TTS), `/v1/realtime` ws passthrough, and diarized batch STT (speaker-labeled).
>   **Backends consolidated (P12):** the five Python adapters (parakeet, kokoro, speaches,
>   sherpa-realtime-adapter, sherpa-diarize) are retired and replaced by **one Go binary,
>   [oidio](https://github.com/IodeSystems/oidio)** — an OpenAI-audio server on sherpa-onnx-go doing
>   STT + diarization + TTS + realtime. corrallm proxies it like any backend; ml-kit serves 4 audio
>   models (`stt`/`stt-diarize`/`tts`/`realtime-stt`) from one persistent oidio process. Only remaining:
>   **P9f** (comfort-fill, parked).
> - ✅ **P12 audio consolidation cleanup** — collapse ✅ (5 adapters → 1 oidio, verified; Python examples
>   deleted). `Model.Modes` **dropped** — batch-vs-realtime is now encoded in the capability (cost type
>   `realtime` → `audio.realtime`); the console dispatches by capability (no modes gate/toggle),
>   `/v1/capabilities` routes each endpoint by capability (no mode-filter), schema regenerated. Verified:
>   realtime-stt=audio.realtime, stt/stt-diarize=audio.stt, all 4 models serve.
> - ✅ **P13 chat PDF auto-conversion** — a text model can't read an attached PDF, so the proxy
>   intercepts `/v1/chat/completions`, finds PDF content parts (OpenAI `file`/`input_file`/data-URL
>   shapes), extracts text via `pdftotext -layout`, and injects it as a text part. On by default
>   (`--convert-pdfs`, `--pdf-max-chars`); text-based PDFs only (scanned → OCR is a follow-up).
>   Verified end-to-end (Qwen answered from a PDF's content); tests cover detection + extraction +
>   truncation + no-op passthrough.
> - ✅ **P11 discovery + console** — `/v1/capabilities` manifest; per-model console (Info/Test/Logs/Usage)
>   with chat/STT/TTS/vision playgrounds; STT playground gates batch/realtime per `model.modes`; batch
>   STT renders speaker-labeled segments when a backend returns them; replay a logged activity in-console.
> - ✅ **P10 request observability** (prod-driven) — **P10a/b/c ✅** honest error status (client/upstream
>   cancel → 499, not a mislabeled backend 502) + `activity.error` reason + configurable
>   `--request-timeout` (latent 130s cap removed); per-request payload + TTFB capture; click-through
>   detail modal (error/timing + request/response payloads).
> - ✅ **P14 lanes + single-path models** — schema v2: one serving path per model,
>   `lanes:` = named fallback lists (per-member sticky override); `config.Backend`
>   removed; proc keyed by model name; catalog lists lanes; groups-op rename.
> - ✅ **P17 pause (models + extensions)** — take something out of service: unload it and never
>   load it again (request, lane fall-through, explicit load, or boot preload) until resumed,
>   with an optional resume datetime. **A pause keys on the PROCESS (ProcKey), not the name** —
>   so an extension's models pause and resume as one unit, matching the blast radius unload
>   always had. Enforced at `Manager.EnsureReady` (the one door every load comes through) plus a
>   `filterByPaused` pre-filter so a lane skips a paused member instead of queueing for an
>   admission slot on it. Refusal is a PERMANENT `CapacityError` → 503, not 429: a pause is a
>   decision, not congestion, so no Retry-After would be honest. Pause overrides `persistent`
>   (a pin is a scheduling preference; letting it veto would make pausing pinned oidio a no-op).
>   Durable in `model_pause` keyed by process, restored before Preload. Timed pauses expire
>   lazily on read, plus a 30s sweeper so a pinned process — which nothing requests by name —
>   actually comes back and is re-warmed. Ops `pauseModel`/`unpauseModel`/`pauseExtension`/
>   `unpauseExtension`; pause + scope on `ModelDef`, live state + pause on `ExtensionDef`;
>   `/v1/models` reports `state: "paused"`. UI: a new **Extensions panel** (the process behind a
>   model group had no surface at all before) with Load/Unload/Pause/Resume, a `datetime-local`
>   pause dialog that states the blast radius before the click, and a confirm on unloading an
>   extension-hosted model — which silently took its siblings down for as long as that button
>   has existed.
>   **Two pre-existing bugs surfaced by it and fixed here:** (1) `waitHealthy` polled for the
>   full `--health-timeout` (600s in prod) even after the process it was waiting for had exited
>   — a crash-on-start held its pools and read as `loading` for ten minutes; it now aborts on
>   the handle's `Done()`. (2) a resume that follows a pause closely races the eviction it just
>   triggered (async SIGTERM + 15s grace), so the respawn can lose a fixed resource — observed
>   live as `Unit oidio.scope was already loaded` from the config's `systemd-run --unit=oidio`;
>   `repin` now retries with backoff past `evictGrace`. Only pause can reach this for a pinned
>   process, since ordinary unload refuses a pin.
>   **Teardown is now tracked** (`Manager.stopping`, set by `evictLocked`, cleared once the group
>   is confirmed gone): eviction only REQUESTS an exit and drops the entry from `procs` at once,
>   so nothing previously remembered a process that was still alive. The two paths differ on
>   purpose — `EnsureReady` WAITS the teardown out (a request has nobody to ask, and failing on a
>   200ms window is worse than waiting), while explicit `LoadModel`/`LoadExtension` REFUSE, along
>   with a load issued while one is already loading or draining: coalescing is right for a
>   request and wrong for an operator action, where reporting "loaded" for a spawn someone else
>   started hides which click did the work.
> - ◐ **P18 multi-GPU capacity** (hardware-driven; UNCOMMITTED as of 2026-08-05) — a second
>   card (RTX 3080) went into box1 and corrallm could not see it. Capacity was single-device by
>   construction: `defaultVRAMPool = "gpu0"`, one `DevicePool` per server, and `sizeFrom` emitting
>   exactly one gpu key. Worse, every probe called `gpu.Probe()` = **nvidia-smi index 0**, and that
>   index MOVED onto the new card — the dashboard began reporting a 10 GiB device under a pool
>   budgeted for 32 GiB, and the slot auto-tuner started sizing against the wrong hardware.
>   **Root cause worth keeping: the two orderings a host offers disagree.** nvidia-smi enumerates by
>   PCI bus id (the BIOS scans the chipset uplink before the CPU slot port, so the card FARTHER from
>   the CPU takes the lower number); CUDA/llama.cpp order FASTEST_FIRST. Index is a fine label and a
>   terrible identity. Shipped: `gpu.ProbeAll`/`Select`/`Matches` binding pools to cards by **UUID or
>   PCI bus id, never position** (bare indices are REJECTED, and ambiguity is an error);
>   `ProcVRAMByDevice` so a footprint charges the card that gave up the bytes; `Server.Devices`
>   (pool → selector) + validation; per-model device resolution through `DevicePoolsNamedBy` for the
>   tune cache, the auto-tuner and measurement publish; residency reports `gpus[]` each tagged with
>   its pool; `introspect` prints every card with pool/budget/UUID and flags cards no pool claims.
>   Prod: box1 declares gpu0=5090 / gpu1=3080 by UUID, nomic + chandra-ocr-2 moved to gpu1,
>   Qwen + deepseek PINNED to gpu0 with `CUDA_VISIBLE_DEVICES=<uuid>` — without that pin llama.cpp
>   enumerates both cards and splits across them, which would have made the whole ledger fiction.
>   **Placement is NOT enforced by pools** (deliberate, operator's call): corrallm accounts, the cmd
>   decides. Verified live: all three local models resident simultaneously on the right cards, and
>   two tune profiles resolving to two DIFFERENT cards in one `introspect` — the lookup that was
>   silently broken.
>   **next:** commit it (14 files); `ui/gen/schema.graphql` regenerated.
>   **open decision (user-owned, agreed but unbuilt):** ramUsage should stop carrying two jobs. Size
>   is measurable (`sampleVRAMPeak` already does it), but the multi-GPU work made ramUsage's KEYS the
>   only statement of WHICH card a model uses. Split it: an explicit `pool:` for placement, measurement
>   for size. Decided with it: an unmeasured model claims **NOTHING** rather than the whole pool, and a
>   failed spawn is the fit signal — the first run is the operator's problem. **Caveat recorded:** that
>   is safe on device pools (a CUDA OOM kills only the allocator) and is exactly the rule that already
>   bit this box on the `system` pool, where oidio reached 119G anon-rss and took corrallm down with it.
>   What saved that was the `MemoryMax` cgroup, not the ledger.
> - ◐ **P19 thermal envelope** (box1, 2026-08-05) — the 3080 hit **88°C at 186W with the fan pegged
>   and `0x20 SW Thermal Slowdown` active** during an OCR sweep, while the 5090 sat at 54°C/0%.
>   Measured its thermal response under sustained load rather than guessing: **dT/dP = 0.239 °C/W,
>   intercept 25.8°C** (the intercept recovering intake-air temperature is what says the fit is real).
>   A healthy 3080 is 0.15–0.18 °C/W, so this card's die-to-air path is ~35–60% worse — consistent with
>   five-year-old paste or a packed fin stack, NOT airflow: an airflow/heat-soak problem rises and falls
>   slowly, and this one fell 88→65°C in 30s. The card's own target is 83°C, which the slope puts at
>   ~239W — hence the throttling at a 250W cap. **200W validated over 7 minutes sustained: 71.7°C
>   steady-state (model predicted 73.6), fan 83% not pegged, throttle `0x0004 SW Power Cap` on 193/207
>   samples and ZERO thermal.** At 200W the card is power-limited rather than thermally limited, which
>   is the correct operating point on this cooling. History is persistent in Prometheus —
>   `nvidia_gpu_exporter` on :9835 labels by **GPU UUID**, the same identity the pools bind to, so
>   Grafana joins to pool names for free.
>   **Pulled from Prometheus afterwards, and it corrected the account:** the exposure was not the
>   brief spike a spot `nvidia-smi` reading suggested. Six load episodes, **15.5 cumulative minutes
>   >=80C**, and the two that mattered were a 2.5 min chandra 400 DPI run at a MEAN of 84.9C (during
>   OCR comparison, half an hour before the bench was blamed for it) and the 10.5 min bench sweep at a
>   MEAN of 86.6C. Lesson for any future thermal claim here: query the exporter, do not spot-read.
>   **Do NOT fit the slope across pooled Prometheus episodes** — mixing power caps and soak states
>   gives 0.194 C/W with a 39.6C intercept, and an intercept that is not plausibly intake air is the
>   tell that the fit is contaminated. It predicts 78.4C at 200W against 70.5C actual, where the
>   single continuous ramp (0.239, 25.8C) predicts 73.6C against 71.7C.
>   **consequence for placement:** gpu1 is the thermally-limited card behind the chipset at x4. Bursty
>   OCR there is fine; a sustained sweep is not — bench on gpu0, which idles while gpu1 cooks.
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

### Served name → model (pinned) or lane (fallback) — schema v2 (P14)
A served name (what clients put in `"model"`) is either a **model** — exactly one
serving path — or a **lane** — a named, ordered fallback list over model names:

```yaml
model:
  cmd?:   string        # spawn it (local; proxy is the port it binds) — XOR standalone proxy
  proxy:  number | "host:port" | { host?, port?, headers? }   # forward target
  type:   string        # cost class: chat | embed | openrouter | … (keys into commandCosts)
  quality: int          # relative quality rank (higher = better)

lanes:
  <name>: { members: [modelName | {model, sticky}] }   # fallback order, best first
```
- `cmd` present → spawn + health-check + proxy to local port; absent → standalone proxy
  model (remote/paid; no residency knobs). `headers` → auth for remote ($) endpoints.
- Same weights via two paths = two named models, composed in a lane.
- **Fall-through** (overflow *and* degrade) = requesting the LANE name; a model name pins.
- **Round-robin within the same `type`** (cost-equivalent); **quality tiers ordered best-first**.
- A lane member's `sticky` overrides the model's own when the lane's request loaded it.

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
    *SUPERSEDED (this session): the coarse `modality: text|audio` string is replaced by
    per-model INPUT `modalities` — a nested map keyed text|image|audio, each with optional
    client metadata (image `maxResolution`/`formats`, text `maxTokens`). Config: `Model.Modalities
    map[string]ModalitySpec` + `EffectiveModalities(audioDefault)` fallback (audio cost class →
    {audio}, else {text}); Validate rejects unknown keys. `/v1/models` emits a JSON object;
    GraphQL `ModelDef.Modalities []ModalityView` (list — GraphQL has no map). UI: `modality`
    query field dropped, `modalities{…}` selected + rendered as chips on the model page. Drove
    it: llama.cpp auto-loads the mmproj sibling from a `-hf` vision repo (no `--mmproj`), so
    `image` is CONFIG-DECLARED, not backend-detected. ~~**bonsai vision verified end-to-end**
    (/props modalities.vision=true + red-circle test → "Circle, Red")~~ — **RETRACTED
    (2026-07-18): that check was run against a WARM bonsai.** Cold (first request after a load)
    it silently drops the image and answers as if none were attached; warm it is correct, and a
    fresh second image proves the warm pass is genuine perception, not prompt-cache reuse.
    `/props` still says `vision: true` and the mmproj still loads, so no warm probe can catch
    this. Root cause not yet found (corrallm readiness gate vs. llama.cpp mmproj init), and the
    scope on Qwen/gemma is UNKNOWN — both were only ever probed warm. See **P15** (cold-path
    capability probing exists because of this bug). **All 3 chat models
    ship mmproj** (HF-API repo check: Qwen, gemma, bonsai each have mmproj-*.gguf) → declared
    text+image in ml-kit/corrallm.yaml; the whole `chat` lane [Qwen, gemma] is uniformly vision,
    so a degrade keeps image. Qwen/gemma repo-verified but runtime /props NOT re-checked (box too
    busy to load 29 GB Qwen). gemma-4-12b is "omni" — audio-in left undeclared (separate path,
    unverified). Lane modalities = PRIMARY member's (matches capability derivation). Backend build
    + full `go test` + tsc clean; `make gen` lint gate already red on main (pre-existing `any` debt
    in model.tsx, unrelated). Uncommitted.*
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

- ✅ **P14 — Lanes + single-path models (schema v2).** A model is exactly ONE serving path
  (`cmd` spawned local, or standalone `proxy` remote/surface — the same weights served two ways
  = two named models); fallback across models is a first-class **`lanes:`** section: named,
  ordered member lists (`members: [name | {model, sticky}]`, per-member sticky override for
  "loaded on the lane's behalf → unload sooner"). Requesting a lane name allows substitution
  across members (quality-tiered walk, type-rr, acceptDegrade/qualityFloor/preferResident all
  unchanged); requesting a model name pins exactly that model. Replaces the P3/P7
  "ordered backend list" — `config.Backend` is gone (`Model` flattened; `Slots`/`ProxyTarget`
  moved onto it), `ResolveServed` → `[]Candidate`, proc.Manager keyed by plain model name
  (no more `served#idx`), reservations target the first candidate, `/v1/models` lists lanes
  (`kind: lane`, `members`) alongside models, overview gained `LaneDef`s, and the old hidden
  degrade row (a proxy row pointing at another model's port — lifecycle-blind: it forwarded
  to a dead port unless the target happened to be warm) is impossible by construction.
  Priority-group API/UI surfaces renamed lanes→groups (op + dashboard) to free the term;
  persisted `lane_samples` schema untouched. Motivating case: ml-kit's `chat` lane
  (Qwen 27B → gemma-4-12b) replacing a misleading `Qwen3-6-27B-MPT`-serves-gemma proxy row.

- ✅ **P15 — Bench: capability verification, performance profiling & user probes.** Fold the
  crucible eval harness (`github.com/iodesystems/crucible`) INTO this repo as a **second binary**
  (`cmd/corrallm-bench`, plus its MCP helper) so corrallm owns measurement as well as serving.
  One engine, three probe tiers. Depends on nothing in P0–P14; sequence it after P11d.

  **DONE — landed as `cmd/llm-bench` + `cmd/llm-bench-mcp`** (not `cmd/corrallm-bench`; the
  binary took the name it is invoked by in `serve --bench-bin`). Verified 2026-07-29:

  - **Engine moved whole.** `internal/bench/{check,journal,judge,report,run,task}` mirrors
    crucible's package-for-package, and has since grown past it: `run/residency.go` (the
    cold-path control that was this item's entire justification), `run/audio.go`,
    `check/starlark.go`, `task/markdown.go`.
  - **All 14 crucible tasks are present** in `probes/`, plus 7 that crucible could never have
    run: `capability-{vision,stt,tts,diarize}` (need residency control) and
    `edit-safety-{import,pop,rename}`. Diffed task-by-task: 10 byte-identical, and the 4 that
    differ are `crucible-mcp`→`llm-bench-mcp` renames in comments, one gofmt space, and the
    README. No semantic drift.
  - **The MCP helper carried its jailing**, including the 2026-07-16 fix that checks EVERY argv
    element rather than argv[0] (`cmd/llm-bench-mcp/main.go:358`).
  - **Crucible is deleted**, not merely superseded: its last real commit and its last gateway
    call were both 2026-07-18 (9,247 calls lifetime), and corrallm's bench keys have served
    every run since. The repo had no remote, so its plan — the only record of the tool-format
    axis results, the poly-lsp net-negative measurement, and the run-to-run variance data this
    item cites — is kept verbatim at `plan/archive/crucible-plan.md`. Everything else went.

  **Why fold rather than federate.** The decisive argument is lifecycle access, not tidiness.
  A capability claim can only be falsified by probing a model **cold**, and corrallm is the only
  component that can force that — it owns residency, eviction and the `loadModel`/`unloadModel`
  ops (`internal/api/handlers.go:1109,1123`). Crucible today talks to corrallm over HTTP as a
  black box: it cannot evict, so it can never test the cold path, so the entire class of
  cold-load bugs is invisible to it. In-repo, the bench binary drives residency deliberately.
  Keep the transport **HTTP over the real `/v1` surface** even in one repo — an in-process
  shortcut would stop testing the thing users actually hit.

  **Motivating bug (2026-07-18).** `ternary-bonsai-27b` declared `modalities.image` and
  `corrallm.yaml` claimed it "verified end-to-end (red-circle test)". It silently drops the image
  on the FIRST request after a cold load — the model's own reasoning says "no actual image
  attached" — and works once warm. `/props` reports `vision: true` and the mmproj loads, so every
  warm probe passes. Nothing in either codebase could contradict the declaration: corrallm never
  calls `/props` or otherwise checks a declared modality against the live backend (declaration is
  pure operator-trust, `config.go:96-98`), and crucible has no modality concept at all. The claim
  was verified once, by a human, against a warm model. **Cold-path probing is therefore a P15
  requirement, not a nice-to-have** — a warm-only capability check would have passed this bug.

  > **⚠ Correction (2026-07-20): the cold-drop bug did not reproduce, and the probe was at fault.**
  > With sampling pinned (`--temp 0` + seed) and the workspace tools removed from capability probes,
  > `ternary-bonsai-27b` answers "Red circle" **cold AND warm, 3/3 runs each** — and so does
  > `gemma-4-12b` (12/12 stage rows). The cold pass evaluates ~900 prompt tokens of image and gives
  > the same answer as warm, so the image is not being dropped.
  >
  > What actually happened: the probe ran through the agent loop with the default system prompt
  > ("...working in a sandboxed workspace. You have MCP tools: read_file, write_file, list_dir...").
  > The model read "this image" as a file to go find, called `list_dir`, got `.git/`, and answered
  > *"I don't see any image files in the workspace"* — while the identical prompt sent straight to
  > the backend answered *"Red circles"*. `maxToolCallsPerStage: 0` did not save it: that caps calls
  > made, but the tools were still advertised. Sampling then hid the pattern — bonsai and gemma run
  > at `--temp 0.7` and the harness sent no temperature, so warm sometimes got lucky and the
  > cold/warm split read as a residency bug.
  >
  > The 2026-07-18 human observation is left above as recorded; it may have been the same harness
  > effect or genuinely fixed since. **Cold-path probing stays a requirement** — nothing here shows
  > it is unnecessary, only that this particular finding was an artifact. What it does show is that
  > a capability probe must test the BACKEND, not the agent loop wrapped around it.

  **Tiers (one runner, three probe kinds):**
  - **T1 capability** — does the model do what it CLAIMS? Cross-check declared `modalities`
    against the live backend: `/props`, plus real payloads per declared modality (image in →
    describes it, audio in → transcribes, declared `formats`, tool-calling, structured output).
    Cheap, deterministic, pass/fail. **Runs cold AND warm**; a cold/warm disagreement is itself
    the finding. This supersedes the ad-hoc `ml-kit/bin/capcheck` sketch — don't build both.
  - **T2 performance** — gen/prompt tok/s at several context depths, cold load seconds, VRAM,
    spec-decode acceptance. Most inputs already exist: `tune` measures VRAM
    (`internal/tune/tune.go`), and llama.cpp's `timings` are parsed into
    `PromptPerSec`/`PredictedPerSec` per request (`proxy.go:1343-1364`, `store.go:119-120`) —
    but they are **never aggregated**: `RollupByModel` (`store.go:209`) sums tokens/dwell/cost
    only, so there is no per-model "typical tok/s" anywhere in the product. T2 adds deliberate
    timed probes + the missing aggregation.
  - **T3 quality** — crucible's existing tasks/checks/judge/toolsets, moved wholesale. Its
    `task.yaml` + check DSL IS the **user-defined probe format**; reuse it for T1/T2 built-ins
    (a new task `class`) rather than inventing a second assertion language. That was the whole
    risk in growing a separate bench, and folding is what removes it.

  **Run N times, report variance — non-negotiable.** Four back-to-back baseline runs of the same
  two models (2026-07-18) gave 15/19, 18/20, 16/20, 18/20 for one model against 15/19, 18/20,
  18/20, 18/20 for the other. Three runs tied exactly; the one apparent 2-stage gap was
  infrastructure (aborted stages), not capability. **A single run's per-task diff between similar
  models is noise** and reading it as signal is the default failure mode of this kind of tool.
  Throughput was the stable discriminator (1.43× aggregate across runs, consistent every run)
  while wall-clock was not (one run had the "faster" model slower, via swap churn). So: repeat
  probes, surface spread not just a point estimate, and prefer throughput over wall-clock.

  **Persistence + UI.** Keep crucible's `out/<ts>/{runs.jsonl,summary.csv,report.md}` artifacts
  for a full run, but ALSO persist a per-`(gpuName, model)` summary the way `tune.Cache` does
  (`tune.go:132-134`) so the dashboard shows current capability/perf without re-running. UI: a
  **model catalog / comparison view** — the gap today is that `ui/src/routes/index.tsx` groups by
  capability and `model.tsx` shows one model's own numbers, but nothing is cross-model. The T1
  capability matrix wants a declared-vs-verified column, so a false claim is visible as a red cell
  rather than a comment in a YAML file.

  ✅ **per-capability scoring + per-probe detail** (2026-07-19) — the run-wide pass rate was not a
  comparable number and was being read as one. A probe a model cannot serve is skipped, not failed,
  so an STT model ran 4 audio probes, passed them, and showed ~100% while a chat model ran 20 mixed
  probes and showed 90% — the table ranked the speech model above the chat model at chatting.
  `PublishResults` also flattened every row into one aggregate before it reached the DB, so "which
  probe, and how did it do" had no answer server-side at all.
  - `report.Row.Capability` records the surface the PROBE required (`Requires.EffectiveCapability`),
    stamped in `run.go` alongside RunMode.
  - Skipped probes are captured as `run.Skip` in a slice kept OUT of `rows` — letting them into the
    row set would put zeros into summary.csv/report.md and restate a config fact as a capability gap.
  - `PublishProbeResults` → `POST /api/v1/measurements/probes` folds stage rows to one record per
    (model, probe, runMode); cold/warm stay split because the disagreement is the finding.
  - `bench_probe_results` table; `GET /api/v1/bench/probes` groups by capability server-side and
    scores each capability only on its own probes. Skips count toward neither numerator nor
    denominator but ARE returned, so the console says "not applicable" rather than leaving a hole.
  - `model.tsx` gains a "Last run by capability" accordion (per-probe rows, cold/warm, pass/fail with
    the failing check in a tooltip, skips with their reason); History keeps the aggregate with a note
    that it tracks a model against itself, not against other models.
  - The aggregate `bench_results` path is unchanged and still published — an older llm-bench that
    knows nothing about probe detail keeps working.
  - **Gotcha worth keeping:** Huma derives "required" from the absence of `omitempty`, so reusing the
    read struct as the publish struct made a skip record (which legitimately carries no measurement
    fields) fail validation with 422. Publish and read shapes are separate types for that reason —
    caught only by driving the real endpoint, not by the unit tests.
  - `GET /api/v1/bench/capabilities` (`BenchCapabilityMatrix`) ranks models WITHIN each capability
    off each model's own latest run — latest-per-model, not latest-overall, since models are benched
    at different times and one run id would drop everything not in it. A model whose every probe on
    a surface was skipped is **omitted** from that ranking rather than listed at 0%, which would
    assert a failure that never ran. Ties break by name so map iteration can't reorder a ranking.
  - `bench.tsx` renders one score chart + scatter + table per capability; the flat cross-model table
    is retained but retitled "Run totals" and explicitly labeled not-a-ranking, since its score
    column mixes capabilities. VRAM is joined in from `benchResults` rather than duplicated onto the
    matrix endpoint.
  - **untested** — no real llm-bench run has published through either new endpoint yet; both were
    verified with synthetic payloads against a live server. The end-to-end check reproduced the
    original bug's shape: stt 100% on audio.stt, absent from chat; qwen 90% and gemma 70% on chat.
  - **unverified** — neither UI surface has been visually rendered (the console requires pasting an
    admin token). Both typecheck and lint clean and their data sources are verified.

  ✅ **A/B arms + probe drill-in** (2026-07-19, follow-on) — two gaps in the above.
  - **Arms were being averaged.** `PublishProbeResults` keyed on (model, probe, runMode), so two
    toolsets or two tool formats of the same probe UPSERTed over each other — destroying the exact
    comparison an A/B exists to make. An **arm is (toolset, toolFormat, runMode)** and all three are
    now part of the key, in the publisher and in `bench_probe_results`' UNIQUE constraint.
  - **Baseline arm, not pooled average.** A probe's headline score comes from one designated arm
    (rank: warm > any > cold, then `baseline` toolset, then `json` format, then a lexicographic
    tiebreak so the choice is deterministic); other arms render as ± deltas. Pooling would move a
    model's score whenever an arm was added or dropped, which reads as a quality change that never
    happened. `BenchCapabilityMatrix` folds to the baseline BEFORE accumulating, or a model running
    a 3-arm A/B contributes 3× the stages of one running a single arm.
  - Arms reaching different verdicts set `disagreement` — the finding a pooled score hides.
  - **Drill-in.** `bench_probe_stages` + `bench_probe_checks` persist per-stage metrics (turns, tool
    calls, bait calls, broken intermediates, compactions, tok/s) and every check's kind/desc/pass/
    detail. `GET /bench/probe/detail` serves them. Transcripts and journals stay as FILES and are
    served by `GET /bench/probe/{transcript,journal}` from `bench_runs.out_dir`, which llm-bench now
    reports (corrallm cannot infer it: `--out` is relative to llm-bench's cwd, and it previously
    learned the path only by scraping `wrote out/<ts>` from the child's stdout). Host is recorded so
    a run benched elsewhere says so instead of returning an empty transcript that reads as "the
    model said nothing".
  - Artifact filenames are built server-side via `judge.ComboName`, never taken from the caller,
    with a containment check as backstop — otherwise these endpoints are a file-read primitive for
    anyone who reaches the API. Covered by a traversal test.
  - `Open` drops a pre-arms `bench_probe_results` so the schema recreates it: the fix is to a UNIQUE
    constraint and SQLite cannot alter one in place. **Safe only because the table has never carried
    a real run** — if it ever ships with real history this must become a copy-into-new-table
    migration. See `dropStaleProbeTables`.
  - Verified end-to-end against a live server: `json` baseline 50% vs `toon` **+50%** at 25% fewer
    input tokens, failing check `cmd_ok: exit 2: auth.go:14: undefined: Register`, bait tool call
    surfaced in the journal.
  - **still unverified** — no real llm-bench run has published through this path, and the UI remains
    visually unrendered.
  ✅ **cross-model arm comparison** (2026-07-19, follow-on) — `GET /bench/arms`
  (`BenchArmMatrix`) + an "A/B arms across models" section on `bench.tsx`. The per-model view
  answers "did toon help THIS model"; this answers "does toon help at all", and the second cannot
  be read off the first.
  - Comparisons are **paired per probe**: an arm is credited only on probes where its baseline also
    ran. Unpaired, an arm that happened to run against the strong models looks like an improvement
    it never made. A probe whose baseline never ran is skipped entirely rather than counted as a win
    for whatever did run.
  - Mean **and** median delta are both reported, plus W/L/T and the paired probe/model counts — one
    pathological probe must not carry a verdict the rest of the evidence does not support, and a
    verdict resting on 3 probes must not read like one resting on 60. The mean averages over
    PROBES, not models, since the pairing is per probe and that is where the evidence is.
  - Token delta uses evaluated prompt + completion, so a cached prefix re-sent every turn is not
    charged to an arm twice.
  - Verified live: toon **+5.0% mean / +5.0% median, 3W/1L/2T over 6 paired probes, −1500 tokens** —
    but the per-model table shows that is qwen +13% and claude −3%, i.e. the headline hides a split
    decision. This is exactly why `byModel` is rendered under every arm rather than the aggregate
    alone.

  ✅ **first real run + the four fixes it forced** (2026-07-20) — full matrix, 4 models, exclusive,
  19 min: `83 probe results, 98 stages, 288 checks (13 skipped)`, all readable through the API.
  Capability scoring, the drill-in, and the arm split all worked on real data. Four defects surfaced:
  1. **Artifacts collided per arm.** `persistRun` named files by (model, toolset, task) only, so a
     `run: both` probe's warm pass overwrote the cold one and the drill-in served the PASSING arm's
     transcript — the one nobody needs. Fixed by `judge.ComboVariant` (mode + repeat index in the
     name); the judge reads via `ComboCandidates` (warm before cold), and the API falls back to the
     bare name so historical artifacts still resolve.
  2. **Nothing pinned sampling.** `agentkit`'s `llm.ChatOpts` had no temperature or seed at all, so
     llama.cpp's own `--temp 0.7` governed and capability probes were a coin flip. Added
     `Temperature`/`Seed` (pointers, so unset stays distinct from a deliberate 0) to agentkit and
     pinned them for capability-class probes only — quality probes still measure the model as served.
  3. **One sample became a verdict.** `publishMeasurements` published a capability verdict per pass.
     Now observations accumulate across repeats and `publishCapabilityVerdict` publishes once:
     `verified=true` if ANY repeat saw it work, `verified=false` only when EVERY repeat agrees, and
     a mixed result is labelled `FLAKY: observed in k of n runs`. Deliberately asymmetric — a
     success cannot be a false positive the way a miss can be a false negative.
  4. **Capability probes were measuring the harness** — see the correction under the motivating bug
     above. They now run tool-free with `capabilitySystemPrompt`, which mentions no workspace and no
     tools.
  - **next** — the `find-render-entrypoints` failure the drill-in explained is still real: gemma
    burned 10 turns and 9 tool calls exploring, never wrote `findings.txt`, and one grep went out
    malformed (`"grep","-r","","selectorGrammarHelp","."`). Worth a look as a genuine finding rather
    than a harness artifact.

  **Migration risks / decisions to make before starting:**
  - crucible pulls in `agentkit` (`agent`, `llm`, `mcpmgr`); corrallm currently does not. New dep
    on the serving repo even though only the bench binary uses it — acceptable, but name it.
  - `crucible-mcp` is a separate spawned binary (workspace tools for T3). Two new binaries, not one.
  - Module path rewrite `github.com/iodesystems/crucible/...` → `.../corrallm/...`; crucible's own
    `plan/plan.md`, `tasks/`, and `out/` run history need a home (archive the history, port the plan
    into this file).
  - The bench must NOT run inside the serving process — it contends for the GPU it is measuring.
    Separate binary is the point; if it ever gains an API trigger, it still shells out.
  - **Carry crucible's hard-won scoring lessons, don't re-derive them:** a turn/tool-call cap must
    not veto passing checks (it hid a model that had written the correct answer), but a
    *pathological* breach (identical-call loop, tool-call budget) must; and never infer an abort's
    cause — report the underlying error, or a 9.6 s failure gets blamed on a 10-minute timeout.

- ☐ **P16 — Free-tier aggregator (quota-aware remote routing).** Pool many providers'
  independent free daily/minute quotas into one serving budget: register free OpenAI-compatible
  endpoints (Groq, Cerebras, OpenRouter, …) as proxy backends, track each provider's remaining
  quota (from response rate-limit headers where present, else a local counter), and swap across
  them before exhaustion. Extends P14 lanes from react-on-error failover to avoid-before-exhaust
  selection; local models stay the floor (remote free is never the sole path). **Full design +
  provider facts + phased build order in [plan/p16-free-aggregator.md](p16-free-aggregator.md).**

- ✅ **P17 — live in-flight visibility.** The activity log only holds FINISHED requests, so a long
  completion, a cold load, or a request queued behind a saturated backend was invisible in both the
  Overview and Activity views until it ended. The proxy now keeps an in-flight registry
  (`internal/proxy/inflight.go`: queued → loading → streaming, registered before admission, cleared
  on every exit path incl. spills), exposed as `activeRequests` and rendered by a shared
  `ui/src/ActiveRequests.tsx` at the top of BOTH views; elapsed ticks client-side off `startedAt`
  so it never looks frozen between refetches. Overview also leads with a **Loaded** section —
  resident models (ready/loading/evicting) pulled out of their capability sections, so what holds
  capacity right now is the first thing on the page.

- ✅ **P18 — dark ops theme + the panel vocabulary.** The UI ran on a bare `createTheme()`: every
  surface `#fff` on `#fff`, so a dozen cards and section headings read as one undifferentiated
  sheet ("white on white with white sauce"). Now `ui/src/theme.ts` defines three layers
  (canvas `#0f1115` → surface `#171a21` → raised `#1c2029`) separated by **borders, not shadows**
  (MUI's dark elevation overlay is explicitly killed), and `ui/src/Panel.tsx` defines what a panel
  IS: bordered surface + tinted header bar carrying a small uppercase tracked label, with `Row`
  (hairline-separated items — no card-in-a-card) and `Stat` (label-over-value, replacing one-row
  tables). Every route was migrated. Chart palette (`SERIES`) is validated by the dataviz six
  checks against the panel surface — lightness band, chroma floor, adjacent CVD ΔE 8.8, normal
  vision, contrast all PASS; **re-run the validator before reordering or substituting a hue.**
  Table heads deliberately keep the body surface (a tinted head under a tinted panel header reads
  as one confused block). Known gap: charts still have no hover/tooltip layer.

- ✅ **P19 — memory attribution on the Overview (RAM/VRAM, and who holds it).** A stacked bar per
  pool/device showing occupancy with per-model attribution, colored by model over the FULL sorted
  model list (never the per-bar subset — a load/unload must not repaint the survivors).
  **Two truths, deliberately not merged:** *measured* (nvidia-smi VRAM + `/proc/meminfo` host RAM,
  new `internal/sysmem` with the same fail-safe contract as `internal/gpu` — a failed probe reports
  `available=false`, never a zeroed bar) and *accounted* (the scheduler's pool ledger, which is what
  admission and eviction actually decide on). The gap between them IS the signal — a backend that
  outgrew its declared `ramUsage`, or a stray process squatting on the GPU, appears as
  "other processes"; averaging them into one bar would erase it. Non-model segments wear a neutral
  grey, never a categorical hue. Exposed on `residency` as `gpu`/`host`. The old static
  "System capacity" panel is gone — its live half moved here, its one unique fact
  (`server.maxConcurrent`) became "Host limits".

- ✅ **P15a — record the promise (who we told to come back, and when).** A 429 is an appointment,
  not just a rejection: the caller is handed a `Retry-After` and, if they honor it, returns at a
  time we chose. That number was computed from live scheduler state, written to the wire, and
  forgotten — the activity log could say "we refused this key at 14:03" but never "…and told them
  4s". Now `writeBackpressure` RETURNS the promise it made (so the logged value cannot drift from
  the header — it is the same number, not a second rounding of the estimate) and every 429 site
  stamps it onto the activity row as `retry_after_ms`. New `retryPromises` op + "Come back later"
  panel on Activity lists who is due back and correlates each promise with the caller's next
  request: **waiting** (due in the future, not back) · **honored** (returned at or after) ·
  **early** (jumped the gun — ignoring Retry-After) · **gone** (never came back). Caller identity
  for the correlation is the key, falling back to source IP for unkeyed callers.
  - **next:** watch a week of real traffic — the promised-vs-actual column is the evidence P15b needs.
  - **risks:** the correlation subquery is per-promise (indexed on `activity(key, ts)`, bounded by
    limit + retention, but it is a correlated scan for the unkeyed arm). Two hosts sharing one key
    collapse into one caller — conservative (can report a return sooner than the promised caller
    made it, never later).
  - **assumption:** the caller's next request of ANY kind counts as "coming back". A caller that
    returns for a different model still reads as honored.

- ✅ **P15a.2 — utilization (per-model pressure).** Row per served model: in use / capacity, queue
  depth, promises outstanding, not-honored, early, and **both halves of the wait** — `est` (the
  scheduler's own live projection, via `BackendLoad.EstWait`, which calls `backpressure()` rather
  than restating its arithmetic — the moment a read view computes the estimate its own way, the
  number an operator sees stops being the number callers are given) beside `real` (measured mean
  `queued_ms` of requests that queued and were then admitted). Per-model, not box-wide: capacity
  is not fungible, and a saturated 27B summed with an idle embedder into "1/6 in use" describes
  neither. Row set = models ASKED FOR in the window ∪ anything busy right now (a model under load
  with no COMPLETED request yet has no activity row, and would vanish exactly when it matters).
  Measured wait excludes instant admissions (else a quiet hour drags the mean to zero) and
  rejections (whose `queued_ms` is time spent before being turned AWAY — a different quantity).

- ✅ **P15a.3 — service-time distribution (measurement only; no behavior change).** The scheduler
  carries ONE dwell EWMA per backend, so every caller of a model is predicted by the same scalar.
  Measured what that scalar averages over, on 24h of real traffic on `Qwen3-6-27B-MPT`:

  | key | n | mean | CV | max | (1+CV²)/2 |
  |---|---|---|---|---|---|
  | `dun` | 1634 | 3.2s | 1.31 | 40.7s | 1.36× |
  | `probe` | 671 | 13.7s | 1.35 | 110.3s | 1.41× |
  | `life-raglit` | 470 | 8.2s | 3.77 | 398.4s | **7.59×** |

  Means differ **4.3× across callers on one model**. `store.ServiceStats` computes mean/stddev over
  SERVICE time (`dwell − queued − load`; feeding queue delay back into a wait estimate would be
  circular). `utilization` gained CV, ρ, and a Pollaczek–Khinchine `pkWaitMs` —
  `ρ/(1−ρ)·E[S]·(1+CV²)/2` — as a third opinion beside est and real, derived from the distribution
  rather than from either mechanism. New `serviceProfiles` op + panel breaks it down per caller,
  with statistics **blended toward the model prior** (pseudo-count 30, raw and blended both shown so
  the adjustment is auditable) — `life-raglit`'s CV of 3.77 is driven largely by one 398s request,
  and a handful of samples must not set policy.
  - **Dead config found and surfaced:** `maxQueueDepth: 8` against `maxWait: 15s` at
    `maxConcurrent: 1` and ~8s mean service allows **1–2** reachable waiters. The depth bound can
    never bind — which is exactly why 18/18 observed rejections were `queue-timeout` and none was
    `rejected`. The row now flags `depthUnreachable` rather than leaving the two settings to
    contradict each other silently.
  - **risks:** ρ over a window assumes steady state, which a bursty hour is not — `pkWaitMs` is an
    order-of-magnitude read, not a promise. Per-key stats are recomputed from the log per request;
    fine at this row count, not free forever.
  - **⚠ `realWaitMs` is CENSORED, and this is structural, not a bug to fix in the query.** It
    averages requests that queued *and were then admitted* — but anyone who waited past `maxWait`
    became a `queue-timeout` and is excluded. The longest waits are precisely the ones removed from
    the sample, so the measured mean is biased LOW and can never exceed `maxWait` (15s) no matter
    how bad the queue gets. First live reading: est 2.0s · real 4.6s · theory 3m49s. The truth sits
    above `real`; how far above is not observable while `maxWait` truncates the distribution.
    **`maxWait` does not only bound the wait, it bounds what can be MEASURED about the wait** —
    which is its own argument for deriving the setting rather than fixing it, and a trap for anyone
    who later "validates" a new estimator against this column.

- ◻ **P15b — fix the estimate.** *(reframed 2026-08-03 by the first 30 min of P15a data —
  the original premise was WRONG and is recorded here so it is not re-derived)*
  - **What I predicted:** N callers rejected in the same second all get the SAME number, all return
    in the same instant — a self-inflicted thundering herd — fixed by a promise ledger of phantom
    waiters plus staggering.
  - **What 18 real promises showed:** no clustering (told-values ranged 2s–14s, since `ewmaDwell`
    tracks prompt size on a single-slot 27B); **no bursts at all** — every one was `queue-timeout`,
    not a single `rejected`, so `maxQueueDepth` is never reached on this box and the constant-promise
    path the herd argument rested on is not exercised; **zero `early`**, one `gone`. And the estimate
    was **short every single time** — 1.7×–11.6×, median ~4×. The caller (`dun`) retries, is refused
    again, and backs off more patiently than we asked.
  - **So the defect is accuracy, not collision.** `rounds × ewmaDwell` badly under-models a queue
    whose dwell varies by prompt size, and the caller's own backoff absorbs the error. Phantom-waiter
    counting addresses a problem this deployment does not have.
  - **next:** watch the utilization panel's est-vs-real gap across a wider traffic mix before
    changing the formula — one caller against one single-slot model is not a sample.
  - **blocking decision (USER owns):** whether an honest estimate is even wanted. A truthful 25s may
    drive callers off (`gone`) where an optimistic 4s keeps them retrying into a slot that does open.
    That is a product call about what a 429 is FOR, not an arithmetic fix.
  - **risks:** dwell EWMA is prompt-size-blind, so any recalibration on this box's traffic may not
    generalize; `keepSoonest` takes the min across a lane walk, so a lane's promise is not
    attributable to one backend.

- ✅ **P20 — keys-page charts (who spent it, and on what).** Two stacked areas over a shared time
  axis: requests/cost/**dwell** by caller, and the same by model — neither derives from the other
  ("dun is 60% of cost" and "the 27B is 90% of cost" are both true and answer different questions).
  New `usageSeriesByModel` op (+ `store.RollupSeriesByModel`, optional key filter) is the "on what"
  axis to `usageSeries`'s "by whom"; the key-detail page reuses it scoped to one caller. One metric
  selector drives both charts so they are always read on the same measure — **separate charts, never
  a dual axis**. `seriesAxis` is shared by both series ops so the two charts cannot land on subtly
  different axes.
  - **Chart primitives extracted** from `usage.tsx` into `ui/src/Charts.tsx` (one implementation;
    a second copy is how two charts of the same data start disagreeing about gaps and stacking).
    Added a **crosshair + hover tooltip** (timestamp, per-band values, stack total) — bands below the
    top one are read as thicknesses, which the eye cannot measure, so exact values must be reachable
    some other way. That tooltip is also what lets the legend collapse without identity becoming
    color-only. Legend is **collapsed by default** (as requested) and absent entirely for one series.
  - **Unbounded series are folded, not cycled:** top 6 by window total keep the validated `SERIES`
    hues; the rest sum into a neutral grey "other" band — grey because "other" is an aggregate, not
    an entity. Palette re-validated unchanged against the panel surface (all six checks PASS).
  - **Window control (6h/24h/7d), not an axis trick.** A single 6,524-request embedding burst
    flattened the other 23 hours to a hairline. Log/clipping/broken scales all misrepresent a
    stacked area, which is additive by construction — the bands would stop summing to the total the
    reader sees. Narrowing the window leaves every mark honestly scaled to its own period (6h peak
    127 shows real structure where 24h showed a flat line).
  - **known gap:** no x-axis tick labels — time is carried by the hover tooltip only, matching the
    existing sparkline-style panels. Fine for "when did this spike" via hover; not for reading a
    time off the page.

- ✅ **P21 — cache visibility (is the prompt cache actually working?).** `cached_tokens` was captured
  per request but never summed anywhere, so the one number that says whether prompt caching earns its
  keep did not exist. Added `cachedTokens` + `cacheHitRate` to `usageRollup` and `usageByKey`
  (per model AND per caller — hit rate is a property of how a caller PROMPTS: a stable system prefix
  reuses cache, a shuffled one never does), plus `cachedSecondsSaved`, an estimate of the
  prompt-processing time avoided at the observed tokens/sec. Live at 7d: **72.3% box-wide,
  Qwen3-6-27B-MPT at 79.1%, 231.6M tokens served from cache ≈ 52h of prompt processing avoided.**
  - **The correctness question here is `cacheReports`, not the rate.** A zero in `cached_tokens` has
    two incompatible meanings — a backend with no prompt cache (every embedding model, every remote
    provider) versus one that genuinely missed. Reporting both as "0%" invents a caching problem for
    half the catalog. `cacheReports` counts requests that reported ANY hit; at zero the UI shows
    **`n/r`**, never a measured-looking 0%. Pinned by `internal/api/cache_test.go`.
  - **Still ambiguous, deliberately not hidden:** `cacheReports == 0` cannot separate "does not
    report" from "never hits". The tooltip says so. Separating them would need the capture to record
    whether the backend emitted the field at all, not just its value.
  - The grand-total rate is recomputed from summed tokens, NOT averaged across model rows — a model
    with 12 requests must not weigh the same as one with 14,000.
  - **known gap:** `chat` sits at ~19–22% against Qwen's 79–88%. Unexplained; it is a lane, so the
    member it lands on may be resetting cache between calls. Worth a look, not yet looked at.

- **Later.** Multi-node peer awareness (remote load introspection across corrallm peers).

---

## 7. Open items / decisions

### Resolved this session
- ✅ **Lane vocabulary** (P14) — "lane" now means the config's named fallback list over models
  (the user's term); the priority group keeps its proper name and its API/UI surfaces were
  renamed lanes→groups. Persisted analytics naming (`lane_samples`, `LaneDepthSeries`) and
  sched/reservation internals keep the old word — schema stability over vocabulary purity.
- ✅ **Model = one serving path** (P14) — `cmd` XOR standalone `proxy`; residency knobs
  (sticky/persistent/ramUsage/swap/server) are rejected on proxy models at validation. The
  same capability via a paid remote = its own named model composed into a lane, which keeps
  cost accounting per path honest and makes degrade-vs-spill pure member order.
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
- ✅ **Slot reservations** (interactive headroom) — a keyed caller can lease K slots on a model's
  primary backend (`model#0`) for **its own lane**, so saturating batch backs off and interactive
  work finds an already-free slot. Proactive (holds capacity free), distinct from reactive preempt.
  Gates BOTH direct-admit AND promote via `effCapLocked = capacity − Σ(slots reserved by OTHER
  lanes)` — a freed batch slot won't refill a reserved one; preempt waiters bypass (they swap, not
  fill). Lease ≤ **5m** (`--reservation-max-ttl`), renewed by heartbeat re-POST, auto-expired by a
  2s reaper (`StartReaper`). API: `POST /v1/reservations {model,slots?,ttl?}` create/renew,
  `DELETE ?model=` release, `GET` list. Keyed by (backend, lane), no id. Any keyed caller may
  reserve for their lane. `internal/sched/reservation.go` + `internal/proxy/reservation.go`.
  **Dashboard**: a "Reservations" panel on the Lanes page (model/lane/slots/live-countdown),
  fed by a `reservations` gat op (`corrallm.reservations` in GraphQL). Verified live end-to-end:
  reserved nomic → interactive got its slot in 0.02s while batch queued → release drained batch.

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
- ✅ ~~VRAM accounting assumed one server~~ — resolved 2026-07-28, before a second host exists,
  because both failures are silent. (1) `vramBudget` summed every resident process and every
  pinned model with no server filter, so two loaded servers each under-counted their own budget by
  the other's footprint and tuned themselves down to fewer slots. (2) The measured footprint was
  written to a hardcoded `"gpu0"` (`const vramPool`), so a unified-memory server declaring only
  `system:` charged it to a pool with no budget — every measured model there goes PERMANENTLY
  unschedulable, surfacing as a 503 that reads like a backend fault on a model that worked right
  up until it was first measured. Now `Server.DevicePool` (default `gpu0`, validated to name a
  pool the server actually declares) and every `vramBudget` term scoped to the model's own server.
  Regression tests: `TestEffectiveUsage_ChargesMeasuredToServerDevicePool`,
  `TestVRAMBudget_IgnoresOtherServersPinnedModels`.
- ✅ ~~Remote models reported as loaded; extension siblings disagreed about one process~~ —
  resolved 2026-07-28. Two bugs with one root: residency was keyed by served NAME and had no
  concept of "not ours". A pure-proxy backend latches `StateReady` on first request (manager.go:446
  — correct, there is nothing to health-check) and is never an eviction victim (`server == ""`), so
  groq/cerebras sat in the dashboard's "Loaded" panel forever having loaded nothing; meanwhile
  oidio's four models are ONE process, and whichever sibling spawned it read `ready` while the
  other three read `absent` off that same live process. Now: `Model.LocalProcess()`/`Model.Remote()`
  (proxytarget.go) are the predicates, `Process.remote` + `ResidentModel.ProcKey` carry them out of
  the manager, `/v1/models` reports `remote: true` + `state: "proxy"` (kind stays `model` — clients
  filter kind to separate models from lanes), and every consumer resolves state through ProcKey.
  `ModelDef.Spawnable` was `m.Cmd != ""`, which called every extension-hosted model a proxy.
- ✅ ~~Transient capacity misses reported as 503~~ — resolved: `ErrNoCapacity` now returns a
  `*proc.CapacityError` splitting **permanent** (won't fit even fully evicted → stays 503, a real
  operator fault) from **transient** (a resident is inside its `activeUse`/`minResidency` window →
  429 + `Retry-After` = when that blocker becomes a legal victim). The proxy walk keeps the
  *soonest* backpressure across candidates (`keepSoonest`) instead of the last one, so a lane
  answers "when could ANYTHING here serve", including a saturated-but-live member's dwell EWMA.
  Applied to both the inference and realtime paths. **Why it mattered:** agentkit-style clients
  retry 429 against their whole budget but cap 5xx at ~5 attempts (1+2+4+8s = 15s), which is
  shorter than these models' 30s cold load — so every mid-swap load was unretryable. Found via
  crucible, where pinned model names give a 1-element candidate list: any capacity miss was an
  instant, unretryable 503 that silently scored the task zero.
  *Deliberately NOT added: a queued-load intent marker.* It would re-create the load/evict thrash
  `activeUse` exists to damp (see the 107-spill note at `manager.go`), and the computed
  `Retry-After` already lands the client's retry at the moment eviction becomes legal. Revisit only
  with measurements showing spills persist.
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

### ◻ P21 — provider credentials (multi-key providers, scoped budgets, key ACLs)

Design doc: **`plan/p21-provider-credentials.md`**. Not started.

Completes the P16 insight the implementation shortcut — *"free quota is enforced
per ACCOUNT ... across multiple accounts of the same provider"* — by making
`credentials` a list under a provider instead of one `proxy.headers`. Unblocks
four things at once: pooling several keys of one provider, budgets keyed where
the provider actually meters (per API key, not per served model), an ACL of
which corrallm keys may use which credential, and a UI with an object to render.

Deliberately NOT an OpenRouter extension: discovery, roster, quota, cooldown and
cost normalisation are already generic and already work. The missing primitive
is credentials, and Anthropic/Groq need it at the second key.

**next** P21a (schema + implicit-default back-compat) — unblocked, config only.
**blocking decision (USER)** where secrets live (§7). corrallm persists no secret
today and `/api/v1/config/*` serves config as YAML, so a key-management UI turns
that endpoint into a disclosure surface. Recommend a `0600` secrets file, never
served. P21f/g wait on this.
**note** P20's per-model `usd` budget is the wrong granularity — a `usd` cap on a
discovery template becomes N independent caps (12 models × $200 = $2,400, not
$200). P21c fixes it. The live openrouter template carries such a cap today.

### ◐ P22 — Qwen3.8-27B cutover (model aliases) — 2026-08-14

Qwen3.8 shipped 2026-08-03; open weights are **Max (2.4T MoE)** and **27B** only,
so 27B is both the intended target and the only one that fits the 5090.

**✅ Shipped: `aliases` on a model** (`d4f69ee`). Resolution is now lane → model →
**alias** → discovered → glob. An alias yields the CANONICAL name, unlike a glob
which keeps the requested id — a glob's members are distinct models sharing a
spelling and each earns its own metrics row, but an alias is a second name for
ONE process holding ONE reservation, and carrying the requested name would split
residency across two ids for memory allocated once. Validate rejects an alias
that is empty, globbed, self-referential, or that collides with a model, lane, or
another alias. The model-collision rule is load-bearing: resolution returns on an
exact `Models` hit, so a colliding alias could never fire, and a silently dead
alias mid-cutover is worse than a load error.

**◐ In flight: the model itself.** `Qwen3.8-27B` is in the live config, opt-in by
name — no lane, no alias — so 3.6 and 3.8 can be compared without one of them
silently serving `chat`. It runs on 5802 against the same 220k window as 3.6, so
a comparison measures the model and not the context.

**✅ MEASURED 2026-08-14 (live, via corrallm after `bin/deploy --no-ui`).** It
loads, serves correct output, and vision survives (mmproj-BF16 auto-pulled).

    Q5_K_M + Q5_K_M draft @220k   30322 MiB  ->  1473 MiB free
    Qwen3-6-27B-MPT       @220k   28952 MiB  ->  3655 MiB free   (config.yml:381)

**That gap is a REGRESSION, not spare room.** config.yml:383 spends 3.6's 3655 MiB
explicitly: "what a 400 DPI page's image-encoder activations have to fit inside …
treat the headroom as spoken for, not spare." 3.8 as first configured more than
halves it, on a model carrying `convert: pdf: vision`.

**The hybrid saved memory; the draft spent 3× it.** Backing out the 2130 MiB
draft, 3.8's own footprint is ~28192 MiB against 3.6's 28952 — ~760 MiB less,
consistent with DeltaNet layers holding recurrent state rather than KV. The
prediction that the arch changes the KV picture held, but not enough to matter.

**The third-party MTP draft works:** `draft acceptance = 0.889` (48/54), mean
accepted len 3.67. Materially de-risks the a4lg dependency.

**→ Moving to `UD-Q4_K_XL`** (16.69 vs 18.47 GiB): −1823 MiB predicts ~28499 MiB
and ~4108 MiB free — MORE headroom than 3.6 has now, keeping the draft. It is
also unsloth's own balanced pick and Dynamic V3.0 against 3.6's *plain* Q5_K_M,
so the quality delta is narrower than 5-bit→4-bit implies. Pulling; re-measure
on arrival.

**Sampling is a DEFAULT, not a policy** (settled 2026-08-14). llama.cpp reads
`chat_template_kwargs.enable_thinking` per request and overrides `--reasoning`
with it (`server-common.cpp:1079`), and corrallm forwards bodies untouched — so
both the sampler and the thinking toggle are per-request. CLI carries the card's
non-thinking numbers; a thinking caller must send the thinking sampler itself,
because one set of defaults cannot serve both modes and the mismatch degrades
output silently rather than erroring.

**next** re-measure on UD-Q4_K_XL, then bench head-to-head.

**throughput is NOT yet comparable.** A single ad-hoc prompt gave 3.8 91.2 tok/s
against the 136 tok/s recorded for 3.6 in `ml-kit/docs/model-evals.md` — but that
figure came from the llm-bench harness over 2 runs at 180k ctx, while this one was
one prompt at 220k taken while a 17GB download saturated I/O. The two numbers do
not belong in the same table. The real comparison is the harness that rejected
the 35B-A3B: `llm-bench run --models Qwen3-6-27B-MPT,Qwen3.8-27B`. Note its judge
runs on the `chat` LANE, which holds only 3.6 — a judged head-to-head puts judge
and candidate on the same card. The 35B rejection turned on DETERMINISTIC probes,
not judge-scored ones, so a first pass can skip `--judge` and avoid the thrash.

**risks**
- **VRAM is the live one.** 31GB is copied from the 3.6 entry plus an unmeasured
  MTP head, on a card where 3.6 already sits within ~3.6GB of the ceiling. 3.8 is
  a hybrid (gated DeltaNet + gated attention, GGUF arch `qwen35`) and DeltaNet
  layers hold recurrent state rather than KV, so the 220k footprint may not
  resemble 3.6's in either direction.
- **Only one 27B can be resident** (31GB reserved of 32.6GB). Any A/B is serial
  swaps, ~30s declared / ~66s cold — never split traffic.
- **The MTP draft is third-party.** unsloth published no MTP GGUF for 3.8, so
  `--spec-draft-hf` points at `a4lg/Qwen3.8-27B-MTP-ONLY-GGUF`, unverified beyond
  loading. Any throughput number is provisional until ml-kit's spec-sweep runs it;
  dropping the three spec flags leaves a correct, slower model.
- Arch support was CHECKED, not assumed: `qwen35` is in the pinned tree's arch
  table and the built `llama-server` is 10380 (`0b1bad14f`, 2026-08-11). No
  llama.cpp rebuild needed.

**blocking decision (USER)** the cutover itself — three config edits, no code:
rename `Qwen3-6-27B-MPT` → `Qwen3.6-27B-MTP` (also fixing the MPT/MTP
transposition it has carried since it was written), add `aliases:
[Qwen3-6-27B-MPT]` to the 3.8 entry, and repoint the `chat` and `free` lane
members. Rollback is moving the alias line back. The rename is deferred on
purpose: it costs the measured profile (filed under the placement name) and
forces a re-measure, so it is worth paying once, after the decision.

**optional extensions** (not in scope) — surface aliases in the Overview model
form rather than only in `advancedFields`; raise 3.8 past 220k toward its native
262144 once the footprint is known.

### ✅ VRAM measurement: per-device attribution + peak decay (2026-08-12, UNCOMMITTED)

Two fixes to `internal/tune` + `internal/proc`, both found by measuring rather than reading:

1. **Multi-GPU footprints were charged to one card.** `measure()` took the whole process
   group's footprint (`h.MemoryMiB()`, every card summed) and filed it under a single device
   name — and for a two-card model `deviceNameFor` falls through `localDeviceFor` (which
   deliberately refuses to name one card) all the way to `gpu.Probe()`. Live effect: DeepSeek-V4
   split 29.9/6.3 GiB across the 5090 and the 3080 was recorded as **37,094 MiB on the 3080**, a
   figure ~4× that card. Fixed by measuring per device via the already-existing
   `gpu.GroupVRAMOn(pgid, uuid)` — whose doc comment describes this exact bug — behind a new
   `host.Handle.MemoryOnMiB(uuid)`. Falls back to the old whole-host path when a host cannot
   attribute per device (remote agent, macOS). `kvMiB` is deliberately not passed through: the
   KV total is per-process and splitting it would need the per-card layer distribution that
   `-ts` decides and nothing observes. Regression test verified to FAIL without the fix.
   The corrupt live entry was purged; it repopulates per-device on deepseek's next load.
2. **`PeakMiB` is monotonic forever, which is wrong for input-driven footprints.** Correct when
   footprint is a function of slot count; chandra-ocr-2's is a function of its *input*
   (`--image-max-tokens 16384`) — 6196 base / 8240 peak on gpu1, 10706 on the M1 Max. One
   400-dpi page reserved its peak permanently. Added `RecentPeaks` (bounded ring, `RecentWindow`
   = 8) + `EffectivePeakMiB()`: all-time peak until the window fills, then max-of-window.
   Chose max-of-window over the p95 originally proposed — at 8 samples p95 *is* the max, and
   making a percentile meaningful needs ~40 retained observations to buy the privilege of
   under-reserving on 5% of spawns, where under-reserving VRAM is an OOM, not a slowdown.
   Applied at both reservation sites; `PeakMiB` is retained for diagnostics.

**Not verified live.** The regression tests cover both, and the rebuilt binary is deployed, but
the end-to-end check (load deepseek, confirm two per-device profiles) was abandoned — a live
`life-raglit` OCR batch was hammering chandra every few seconds and admission kept refusing on
gpu1. Do it in a quiet window; it is one `models/load` and a read of `vram-profile.json`.

**Related, still open:** gpu1 (10GB 3080, 9,877 MiB usable after driver reserve) now has four
claimants — nomic 816 (persistent), chandra 8,240 (sticky, input-variable), deepseek 6,676,
and any Qwen drafter. They do not fit; a resident deepseek evicts chandra. Policy call is the
user's.

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
  3. **P15: bench (capability + performance + user probes)** — fold the crucible harness in as a
     second binary; corrallm owns measurement, not just serving. Unblocks the two things nothing
     currently covers: verifying a declared modality against the live backend, and aggregating
     per-model throughput. **Blocking decision for the USER before starting:** whether crucible's
     repo is archived on merge or kept dual-homed for a transition (affects whether its `out/` run
     history and `plan/plan.md` move or get referenced). Also carries an open bug — the bonsai
     cold-load vision drop (§6 P9d retraction) is unrooted, and its scope on Qwen/gemma is untested.
  4. **Later: multi-node peer awareness** — remote load introspection across corrallm peers.
     Concrete driver (2026-07-28): attaching a 64 GB Apple-silicon box as a second compute host.
     **Two shapes, and the cheap one is not a stepping stone to the other:**
     (a) *proxy peer* — machine 2 runs its own `corrallm serve`, attached to box1 as an ordinary
     `extensions:` proxy target. **Zero new code, works today.** Buys serving; buys no unified
     residency, eviction, fairshare or dashboard. Use it to measure whether the second box earns
     its place before building anything.
     (b) *managed host* — box1 spawns onto machine 2. Needs `proc.Manager`'s single local
     `exec.Command("sh","-c",…)` (manager.go:398) behind a Spawner interface, `gpu`/`sysmem`
     probing per-server instead of per-host, and `config.Server` gaining a real address (today it
     is a pure local pool budget: pools/reserve/maxConcurrent, no host field).
     **DECIDED 2026-07-28 — shape (b), full remote control.** Also decided: agent-local config,
     UI-editable, **synced up into the primary's declared config** (an injected model can never be
     a lane member — `config.go:1018` validates lane membership against `c.Models` at load time —
     so agent-contributed models must land in declared config, which is what makes live reload a
     prerequisite rather than a nicety); one-time enrollment token minted in an Agents tab; the
     daemon serves cross-compiled binaries for `curl <daemon>/install.sh | bash` (verified:
     `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64` builds clean, ~37 MB, sqlite is pure-Go modernc).
     One binary, `corrallm agent` subcommand — satisfies the no-duplication concern while keeping
     the auth domains separate (`auth.Middleware` gates `/api/*` with the admin token; an agent
     needs its own credential).
     Ordering (each step shippable with no second machine present): ✅ **step 0** per-server VRAM
     accounting (done) → ✅ **live config reload** (done: `include:` merging, SIGHUP,
     atomic config swap in all four holders; fractional `quality` landed alongside it because
     the Mac's 4-bit tier sits between two integers) → ✅ **`internal/host` Spawner interface**
     (done: Spec/Handle/Host, Local only, platform primitives moved; Process holds a Handle
     instead of an *exec.Cmd) → ✅ **`Server.Agent` binding**
     (done: endpoint LIST per the VPN topology, `TargetFor` so an agent model's loopback port
     never resolves to the primary's, `host.Unavailable` so it refuses to spawn locally) → ✅ **`corrallm agent`**
     (done: hello/capacity/spawn/list/status/signal + sequenced logs with `from=`; token
     required by default since it executes shell commands; heartbeat+lease deferred to the
     failure-semantics step) → ◻
     `host.Remote` (integration-testable by running a second agent on another port on box1) → ✅ **failure semantics**
     (done: agent heartbeats OUTWARD, 3-interval miss window, down refuses spawns before
     reserving pools, config survives the outage, token revocation is how membership ends;
     NO self-reap — per-server ledgers make a stranded reservation harmless to other hosts.
     reconnect ADOPTION reconciles on every heartbeat: matching keys adopted, orphans reaped
     past a 60s grace, vanished backends free their pools) → ✅ **darwin capacity**
     (done: `gpu.Apple` + `sysmem_darwin`; ProcVRAM errors rather than reporting zero, and a
     host that cannot measure per-process memory now REQUIRES ramUsage at validate time —
     otherwise it silently serves one model at a time) → ✅ **enrollment + Agents/Config UI**
     (done: one-time tokens, self-registration sized from the agent's own probe, GUI model
     CRUD). Remaining: switch the daemon's default config to the managed one and retire the
     hand-written file.
     **Agent addressing (USER, 2026-07-28): an agent has SEVERAL addresses, not one.**
     A NAT/LAN address on the same network as llm.iodesystems.com, an external host, and a
     VPN address when both the Mac and the daemon are on the VPN — all valid for the same
     box at the same time, and which one works depends on where the daemon is sitting.
     So `Server.Agent` takes a LIST of candidate endpoints (tried in order / by
     reachability), not a single `url`, and the backend data-plane host is resolved the same
     way. Two consequences: reachability becomes per-endpoint state, not per-agent; and
     `config.IsLocalHost`'s "a private-LAN address is not local" doc comment is already
     wrong under this topology — a 192.168 address may well be a box we manage. The
     predicate itself stays correct (it short-circuits on LocalProcess), but the comment has
     to be reworded to say "we run no process for it", or the next reader breaks it.
     ✅ **box2's pool shape — DECIDED (USER, 2026-08-04): ONE unified pool.** `pools: {system:
     <hw.memsize>}`, `devicePool: system`, and the wired limit expressed as `reserve` rather than
     as a second pool, so budget = total − reserve lands at (or just under) what the GPU may
     actually wire. This was not merely undecided — enrollment was shipping the *wrong* shape:
     `poolsFrom` declared `system` from hw.memsize AND `gpu0` from the wired limit, while
     `devicePoolFrom` picked `gpu0` whenever a GPU reported anything, so a 64 GiB Mac enrolled as
     119 GB across two ledgers `fitsLocked` checks independently. Undetectable by construction, on
     the one class of host that also cannot measure per-process memory. Fixed by making the three
     answers one function (`api.sizeFrom`) fed by a new `Capacity.Unified` the agent reports —
     GOOS cannot answer it, since an Intel Mac with a discrete card is darwin with two real pools.
     ✅ **Repair path: RE-ENROL.** Enrollment used to preserve `pools`/`reserve`/`devicePool`, which
     pinned a server to whatever its FIRST enrollment computed — so the one operation that exists to
     re-measure a machine was a no-op precisely when it was needed. `mergeEnrollment` now re-derives
     all three from the fresh probe and keeps only what the agent cannot know (`notes`,
     `maxConcurrent`), appending a note when a resize actually moved the numbers. Safe because
     re-enrolling needs a one-time token the operator just minted — it cannot happen by accident.
     **Still USER-owned, surface before the step that needs it:** agent lease self-reap on/off and
     its TTL (decides whether the ledger may ever be released after a host is lost — the trade is
     "a network blip kills an in-progress cold load" vs "a partition strands 48 GB"); transport
     trust (the agent executes arbitrary shell strings — it is an RCE surface by design);
     whether to spike `proc_pid_rusage`'s `ri_phys_footprint` for real per-process measurement on
     darwin before accepting "unmeasurable → ramUsage becomes authoritative".
  5. ◐ **P16: trial spawn + add-model form** — authoring a model is currently "type a cmd string into
     a YAML box, save it, load it, read the logs, guess again". Every iteration mutates live config.
     ✅ **API slice done** — `POST /api/v1/config/models/trial` (`internal/proc/trial.go`,
     `internal/api/trial.go`): spawns an UNCOMMITTED cmd on a chosen server and tears it down,
     writing nothing to config whatever happens. Stages reuse what existed: `TargetFor` (resolve,
     incl. the agent-endpoint rewrite) → admit → `hostFor(server).Start` (local or remote, one
     interface) → line-buffered log events → `waitHealthy` → `GET /v1/models` → `probeUI` →
     `handle.MemoryMiB()` → term/grace/kill. Reuses `specToModel`, so a trial and the save that
     follows interpret `proxy: 5800` identically. A failed command returns **200 with the
     transcript**, not a 4xx — the failure IS the answer, and an error status would discard the
     logs saying why. 6 tests cover the admission invariants.
     **next:** stream the transcript (today it returns as one document when the run ends — fine for
     curl, poor for a 90 s cold load being watched); then the add-model form itself, with
     "Trial" beside "Save" and the result prefilling `ramUsage`/`upstream`/`maxConcurrent`.
     **assumption made:** a trial is registered `persistent: true` so unrelated traffic cannot kill
     a run halfway. Safe only because it reserves nothing but FREE memory (never evicts), so it
     displaces nothing by holding it; `trialTTL` (10 min) is what bounds the harm. Revisit if
     trials start being left open.
     **DECIDED (USER, 2026-08-04): a trial reserves only if it FITS — never evicts.** On a full box
     it refuses with "unload something first". A trial is an experiment; it must not be able to
     take down a warm production backend.
     **risks:** ✅ (1) registered in `m.procs` under `trial:<id>` so `ReconcileAgent` adopts rather
     than reaping it past the 60s grace (`reconcile.go:71`); ✅ (2) `trialTTL` + a deferred teardown
     on every exit path, so a closed tab cannot strand the reservation; ✅ (3) one trial per server,
     enforced in `admitTrial`; ◻ (4) it widens the existing RCE surface from config-authoring to a
     dashboard button — same admin-token gate, unchanged, but stated rather than discovered.
     **blocking decisions (USER):** none outstanding.
     **optional extensions (out of scope):** re-probe/resize an already-enrolled server from a live
     `GET /agent/v1/capacity` (would have auto-repaired the osx double-count, and would track a
     `iogpu.wired_limit_mb` the operator changes later); trial an A/B pair of cmd strings and diff
     their banners.
  6. ✅ **P17: heartbeat-driven self-update actually fires** — `maybeSelfUpdate` always ran from the
     heartbeat, but could not DECIDE: it compared version strings, and `make agents` without a tag
     stamps "dev" on both sides, so `ack.Version == "dev" && own == "dev"` skipped every update.
     That is the normal state while iterating on agent code — the exact case the feature exists for.
     Now both sides exchange a **build id** (short sha256 of the binary; `agent.HashFile` is the one
     definition, `agentdist.BuildID` caches by size+mtime since every agent asks every beat). Bytes
     differ → update; bytes match → don't, which is also what terminates the loop. Version strings
     survive only as the fallback when either side lacks an id, keeping their old dev/dev rule.
     Post-download the installed FILE is hashed (not the `X-Corrallm-Version` header) before the
     rename, so the restart guard compares like with like. 5 tests.
  - OSS follow-ups (not blockers): auth multi-user accounts/roles + token rotation (today is a single
    shared admin token); rename the `WattsPerToken` cost fields to `WhPerToken`.
- Optional polish in §7 Optional extensions (affinity weighting, context-window clamp on degrade,
  gRPC, CapacityProbe, `server.maxConcurrent` host cap; instantaneous queue depth is now covered by
  the sampler; the proactive ttl reaper shipped with slot reservations).

---

## 8. Deployment (production cutover)

corrallm **replaced the llama-swap fork on `:8111`** for the live workload.

> **CORRECTION (2026-08-14).** The live config is **`~/.corrallm/config.yml`**, not
> `ml-kit/corrallm.yaml`. ml-kit was refactored to "the llama.cpp builder, nothing else"
> (`b72e7f0`) and no longer holds the deployment. The running command is authority:
> `corrallm serve --config ~/.corrallm/config.yml --web-root <repo>/ui/dist ...`.
> Everything below still describes the deployment correctly except that path.

The deployment lives outside this code repo:
- **the production config** — translated from `ml-kit/llama-swap.yaml`:
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

### ✅ P20 — token counting already works end to end (verified 2026-08-10)

**This item was WRONG when written and is kept as a correction.** It claimed
corrallm had no token-count surface, on the strength of a route grep truncated at
25 alphabetical lines — the listing stopped at `/api/v1/config/...` and never
reached the entry that mattered. There are 67 routes.

`/upstream/<model>/tokenize` resolves the model, ensures the backend is resident
and forwards the path. Verified live:

    POST /upstream/Qwen3-6-27B-MPT/tokenize  {"content":"hello world"}
    -> 200 {"tokens":[14556,1814]}

Ungated by design (`/upstream/m/` is in auth's ungated list beside
`/v1/chat/completions`), so a caller needs no admin token. Note the shape is
`/upstream/<model>/`, NOT `/upstream/m/<model>/` — the latter 404s "unknown
model" even for a registered id, which is what sent the first probe wrong.

agentkit already consumes exactly this: `llm/tokenize.go` lists
`root + "/upstream/" + model + "/tokenize"` among its candidates, beside bare
llama.cpp's `/tokenize` and vLLM's `/v1/tokenize`.

**Anthropic count_tokens: BUILT 2026-08-10, `dc01410`.** The llama.cpp half was
already there; the Claude half was not, and the reason was subtle. The `claude`
extension is a passthrough to `api.anthropic.com` that already injects
`anthropic-version` and Claude Code's OAuth bearer — but its models are GLOB
TEMPLATES (`claude-haiku-*`), and `/upstream` resolves with an exact
`Models[served]` lookup, so `/upstream/claude-haiku-4-5/...` answers "unknown
model" for an id the chat path serves fine.

`POST /v1/messages/count_tokens` resolves through `ResolveServed` (glob-aware)
and forwards the path unchanged. Mounted OUTSIDE `handleInference`: counting runs
no inference and holds no GPU, and a caller sizing a prompt before deciding
whether to send it must not queue behind the backlog it is measuring against.

**VERIFIED LIVE 2026-08-10** against real Anthropic, after `bin/deploy --no-ui`:

    messages only                 200  {"input_tokens":9}
    + system                      200  20
    + system + tools              200  563
    claude-opus-4-8 (other glob)  200  9
    local backend                 404  names /upstream/<model>/tokenize
    unknown model                 404

**The live run found a bug the unit test could not, and the FIXTURE was why.**
The first cut gated on "has a proxy target". Every locally-spawned backend has
one — a loopback port is how corrallm reaches the llama.cpp process it started —
so the route forwarded to a local server with no such path and returned **502**
from a dead port. The fixture had given the local model an empty `Model{}`, which
made the wrong predicate look valid. Now gated on `Model.Remote()` (no local
process AND non-loopback), the fixture carries a loopback proxy the way real
config does, and the fake upstream binds a NON-loopback address — a test server
on 127.0.0.1 is indistinguishable from the backend corrallm spawned, which is
the distinction under test. Fixed at `502bfe3`; reverting the predicate now
fails two tests, one with the same 502.

**Measured, and it constrains the dun design (2026-08-10):**

    no tools   9        fixed tool-use overhead   497 tokens
    1 tool     551      per small tool             45 tokens
    2 tools    596      9 + 497 + 45 + 45 = 596 — exact

Enabling tools at all costs ~497 tokens of Anthropic's own tool-use preamble.
**So per-server tool costs CANNOT be counted independently and summed** — the
fixed overhead would be charged once per MCP server, overstating a four-server
session by ~1.5k and breaking the property that the parts add up to the total.
The Anthropic path has to measure incrementally (N servers minus N-1), or carry
the overhead as its own row. dun's `SystemBreakdown` already asserts parts-sum-
to-total, so this would have surfaced as a failing test rather than a wrong
number — but only after the work was built.

**CONSUMED, end to end 2026-08-10.** agentkit gained `CountPrompt(system, tools)`
(`284ec36`) — Anthropic's shape where the endpoint speaks it, the concatenation
otherwise — and dun's `/context` now reports MARGINAL per-part costs
(`75c165f`), because the preamble makes independent counting wrong. Live through
this route on `claude-haiku-4-5`:

    total 638  prompt 11  shared 497
    built-in tools 44 · mcp: chrome 43 · mcp: raglit 43   (sums exactly)

## 8. Capacity-parked (relocated from `~/inflight` 2026-08-04, and again 2026-08-10)

Three items that were sitting on the global shelf because they wait on a machine,
not on a decision. They belong here — the work and the config are both corrallm's.
Resume conditions are kept verbatim so nothing has to be re-derived.

### ⏸ Deploy the hardened Qwen chat template
**waits-on:** systems — the llm host must be quiet.
**check:** `[ "$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits -i 1)" -lt 20 ]`
(exit 0 = ready. Samples ONE instant — observed flipping ready→busy within minutes.)

**Check replaced 2026-08-10, because the old one failed CLOSED.** It polled
`http://127.0.0.1:5800/slots`. The port is right — corrallm spawns llama-server
on fixed slots 5800/5801 — but when nothing is bound there, `curl -sf` exits
non-zero on a refused connection and the pipeline reports NOT READY. The idlest
state, no model loaded at all, read as blocked. Observed that day: 5801 held an
embeddings model, 5800 was unbound. corrallm's own `/api/v1/active` on 8111 is
not a substitute — it answers 401, admin token required, and a token does not
belong in a versioned plan.

The served template renders untrusted tool-result content raw, so bytes from any
file an agent reads can inject a chat turn. **Not deployed:** no
`chat-template-file` in `corrallm.yaml` or `local/corrallm.yaml` as of 2026-08-04.

**Evidence (verified, do not re-derive):**
- `<|im_start|>` in content is **1 token**, not 6 — a real frame break, not a lookalike.
- Through **minja** (llama.cpp's own engine, `build/bin/test-chat-template`): stock
  yields turns `[user, assistant, user, SYSTEM, user, assistant]`; hardened yields
  `[user, assistant, user, assistant]`.
- The stock template is byte-identical to the live server's `/props` output — it is
  what actually runs.
- Hardened is a 4-line functional derivative; every structural literal preserved.

**canonical source:** `~/local/src/iodesystems/tool-call-lightly/templates/`, with an
offline suite (`./render-test`) that proves the fix through minja with no model. The
ml-kit copies are a stale duplicate — prefer the project.

**next:** 1) `corrallm features Qwen3-6-27B-MPT` for a baseline · 2) add
`--chat-template-file .../qwen3-27b-heredoc.jinja` to the Qwen entry in
`corrallm.yaml`, restart · 3) `corrallm features` again — **if round-trip fidelity
moved, the swap broke the autoparser: revert** · 4) `probe.py render`.

**risks:** llama.cpp's autoparser derives its parser FROM the template, so a template
change can silently break tool-call parsing. Step 3 is the guard, not a formality.

**UPDATE 2026-08-04 — llama.cpp rebuilt from master (b10273 / `4308a4f03`), and this
changes the risk.** `f5919bf45 chat : add qwen3 specialized parser (#26252)` (2026-08-02)
adds `common_chat_params_init_qwen3_coder`, selected by SNIFFING the template source for
all three of `<tool_call>`, `<function=`, `<parameter=`.

- **No Qwen template file changed upstream** — `models/templates/*Qwen*` is untouched
  across the whole 260-commit range. The change is parse-side, not render-side.
- **All four of our templates match all three literals** — `qwen3-stock`,
  `qwen3-hardened`, `qwen3-27b-stock`, `qwen3-27b-heredoc`. So stock and hardened now
  select the SAME specialized parser. **The risk above shrinks**: swapping templates no
  longer moves you onto a differently-derived parser. Step 3 stays the guard, but the
  failure mode is narrower than when this was written.
- **Every fidelity baseline is now stale as a comparison point.** The
  `corrallm features Qwen3-6-27B-MPT` numbers predate this parser. Re-baseline in
  step 1 rather than comparing against anything recorded earlier.
- **Open question:** the parser is labelled "Qwen3-Coder XML tool calls". Qwen3-6-27B-MPT
  is not a Coder model — it is selected purely by literal sniffing, so this may be a
  MISDETECTION. Its grammar is deliberately lenient in ways that matter (accepts required
  arguments in any order; "Qwen3-Coder models may occasionally omit the `<tool_call>`
  token"). Worth confirming the detection is intended for this model before deploying.

### ◻ Muse Glimmer 30B vs Qwen3-6-27B-MPT — the probe suite (relocated 2026-08-10)

**waits-on:** systems — the 5090's one slot. Running it evicts interactive chat.
**check:** `[ "$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits -i 1)" -lt 20 ]`

**NOTHING IS UNDECIDED, and the shelf said otherwise for days.** It carried
`waits-on: you (a decision)` with the prose condition "you say to run it", so
`./check` could not evaluate it and a reader went looking for a decision that had
already been made. The model is committed, serving and measured; the command is
written; the spec body is beside this file. What it costs is the 5090 for the
duration of repeated 20-31 GB swaps, which is the box's single interactive-chat
slot — a green light on disrupting shared hardware, not a design call.

It is here rather than on the global shelf for the reason §8 exists: the work,
the config, the bench binary and the spec are all corrallm's. Waiting on a
machine does not make an item cross-repo.

`muse-glimmer-30b` is COMMITTED to the running corrallm at native 131k ctx and
verified serving. Only the probe suite is left; it was deferred because it needs
repeated 20–31 GB swaps (Glimmer 23.3 GB and Qwen 31 GB cannot co-reside).

**To resume:** `llm-bench --models muse-glimmer-30b,Qwen3-6-27B-MPT`. Callers
MUST pass `chat_template_kwargs {"reasoning_effort":"low"}` or scoring breaks
(see traps). Model is in NO lane on purpose — it takes no live traffic until
someone adds it. Spec body kept at `plan/muse-glimmer-trial.json`.

**Served, end to end (2026-08-10):** through corrallm on key `aw3`, a correct
one-sentence answer at **91.8 tok/s with DFlash accepting 147/297 drafts
(49.5%)**. Single prompt, one rep — a working-wiring data point, NOT a benchmark.
Meta's 233.4 tok/s is for the *17gb* build; this is *dynamic*, so the two are
not comparable and neither is confirmed.

**Evidence (measured 2026-08-10 — do not re-derive):**
- `POST /api/v1/config/models/trial` returned `ok:true` twice, WITH mmproj +
  dflash resident: **`memoryMiB: 22866` at `-c 65536`**, **`23336` at `-c 0`
  (= 131072, the trained max)**. Doubling context cost **+470 MiB**, leaving
  ~8.2 GB spare on the 5090 — deep-context efficiency confirmed by measurement,
  and the mechanism is sliding-window 2048 on 3 of every 4 layers plus GQA 16:1,
  so only 13 of 52 layers carry full-context KV. Run it at `-c 0`; a 26 GB
  ramUsage guess was 3.6 GB high, declare **24GB**.
- `modalities: [video, vision]`, `supportsTools: true`, `slots: 1`.
- ml-kit llama.cpp rebuilt to `dd1ea5243` (`b10355`, archs 86;120). The old pin
  `f9e832c10` predated the model by 4 days. Arch landed upstream in `62bf73d25`
  (PR #26841, merged 2026-08-10) — `src/models/muse-glimmer.cpp`.
- `--spec-type draft-dflash` and `LLM_ARCH_DFLASH` are fully implemented; the
  drafter attaches via `-md <snapshot>/dflash-kquant.gguf` + `-ngld 99`. There is
  no `--hf-file-draft`, and `-hfd repo:kquant` is AMBIGUOUS (three files match).
- Files (HF hub cache, snapshot `93769bc7…`): dynamic 19.65 GB, mmproj 1.4 GB,
  dflash 1.63 GB. `--mmproj-auto` does NOT fire when `--hf-file` is explicit —
  mmproj must be pulled and passed by hand.
- Meta's own claim, unverified here: 74.9 → 233.4 tok/s (3.1x) with DFlash on a
  5090, using the *17gb* build (we hold *dynamic*).

**risks / traps:**
- **SIGFPE in `--fit`.** The new auto-sizing pass divides by zero when a GPU is
  nearly full, and it enumerates CUDA devices EVEN AT `-ngl 0`. Crashed twice
  before `--fit off`. This threatens every corrallm spawn onto a busy card with
  this binary, not just Glimmer.
- **Reasoning defaults to high** and llama.cpp's `--reasoning off` does NOT lower
  it — strength lives in the harmony template ("Reasoning strength: high").
  Control per-request with `chat_template_kwargs: {"reasoning_effort": "low"}`.
  Without it, short-budget requests return EMPTY `content` with everything in
  `reasoning_content` — which would silently score as failure across the probes.
- Probes are agentic/coding; Glimmer is agentic-multimodal and Qwen3-6-27B-MPT is
  the box's chat model. `capability-vision` has no Qwen-side comparison.
- corrallm refuses to evict for a trial ("a probe never evicts a running model"),
  so the suite must unload deliberately between candidates.

### ◐ Second compute host: 64 GB MacBook Pro — ENROLLED 2026-08-04
**waits-on:** yours — name the chip. Everything else is unblocked.
**check:** manual.

**The box is on the LAN and enrolled.** From `~/.corrallm/config.yml`:

    carlsmacbookpro:
      pools:   { system: 68GB }      # budget = 68 - 18 reserve = 50GB usable
      reserve: { system: 18GB }
      devicePool: system
      noProcessMemory: true
      agent.endpoints: [192.168.1.249, 192.168.252.1, 192.168.1.58]:6503

Three things this confirms in the field rather than in theory:
- **The unified-pool decision was right and the repair path WORKS.** The notes
  record the second enrollment resizing it: `pools map[gpu0:51GB system:68GB] →
  map[system:68GB], devicePool "gpu0" → "system"`. That is exactly the
  two-ledger bug §7 #4 called "undetectable by construction", corrected by
  re-enrolment on first contact with real hardware.
- **`noProcessMemory: true`** — the predicted macOS limitation. `ramUsage` is
  authoritative on this host, so **a model entry omitting it is a validate-time
  error**. Check that before adding the A3B model.
- The endpoint LIST (LAN / VPN / alt) is populated, as designed.

**Chip named (USER, 2026-08-04): M1 Max, 64 GB — ~400 GB/s.** The LOW end of
the range this entry was written to resolve, and the reason it insisted on
naming the chip first. Against box1's RTX 5090 at ~1.79 TB/s that is **~22% of
the bandwidth**, with more usable memory (50GB vs ~31GB). Capacity is the axis
box2 wins on; bandwidth and compute are the axes it loses on.

Roofline at Q5_K_M (~0.6875 bytes/param), decode:

| model | bytes read/token | M1 Max ceiling | 5090 ceiling |
|---|---:|---:|---:|
| dense 27B | 18.6 GB | **~22 tok/s** | ~96 tok/s |
| 35B-A3B (~3B active) | 2.1 GB | ~194 tok/s | ~868 tok/s |

Ceilings, so real numbers land well below — but the ~9× gap is why dense 27B
feels slow here and why A3B is not merely preferable on this host, it is close
to mandatory.

**The M1-generation twist, and it matters more than the bandwidth.** Token
generation tracks bandwidth; **prefill is compute-bound**, and M1 is compute-weak
relative to its own memory bandwidth versus M3/M4 — the Max has a 32-core GPU.
At 180k context, prefill is the likely wall on this host — and speculative
decoding does **nothing** for prefill. Optimizing decode here may be optimizing
the half that is not the problem.

**Consequence worth deciding explicitly: 180k may be a BOX1 constraint, not a
fleet constraint.** corrallm can serve a different context per model/server.
box2 running A3B at a smaller context (fast decode, tolerable prefill) may be
worth far more than box2 struggling to match box1's window. Do not port the
180k floor to box2 by default.

**And MoE does not fix prefill.** A3B's ~9× decode advantage comes from
activating ~3B of 35B per token. Prefill batches many tokens at once, so across
a batch effectively all experts are hit and prefill FLOPs approach the FULL
35B — meaning **35B-A3B may prefill SLOWER than dense 27B while decoding far
faster.** For long-prompt agent sessions that trade could go either way, and it
is measurable: `bench/spec-sweep.sh` records `prompt_per_second` alongside
tok/s for exactly this reason. Measure prefill before committing.

**Model sizing — DECIDED (USER, 2026-08-04): Qwen3.6-35B-A3B, not the dense
27B.** Decode on unified memory is bandwidth-bound: dense 27B reads all 27B
params per token, the A3B reads ~3B active. Capacity is the cheap axis here
(50GB usable), bandwidth is the scarce one. Even on a compute-rich RTX 6000 the
published gap is 160 → 240 tok/s; on Apple silicon it should widen.
`unsloth/Qwen3.6-35B-A3B-MTP-GGUF` is a direct sibling of the 27B entry, so the
model config is nearly a copy.

**Before committing to it, three unmeasured things:**

1. ✅ **The wired limit is NOT a problem — checked 2026-08-04, and the design
   worked.** `sysctl iogpu.wired_limit_mb` returns `0`, so macOS is on default
   policy and `gpu/apple.go:wiredLimitMiB` falls through to
   `defaultWiredLimitBytes`: `memBytes >= 36GiB → memBytes * 3/4`. On 64 GiB
   that is **48 GiB = 51.5 GB**, against a budget of 68 − 18 = **50.0 GB**.
   Fits, with 1.5 GB of headroom. This is exactly what §7 #4 intended by
   "the wired limit expressed as `reserve` … so budget = total − reserve lands
   at (or just under) what the GPU may actually wire" — it landed just under,
   on real hardware, without anyone tuning it.
   **Residual, worth one read not a fix:** `defaultWiredLimitBytes` is by its
   own comment an *approximation* of macOS's policy. The authoritative number is
   Metal's `recommendedMaxWorkingSetSize`, which llama.cpp prints at Metal init
   (`ggml-metal-device.m:946`: `recommendedMaxWorkingSetSize = %8.2f MB`). If the
   real value is below 51.5 GB the 1.5 GB margin evaporates, so read that line
   once from any llama.cpp start on the Mac and confirm.
2. **Does it fit at all?** 35B-A3B Q5_K_M ≈ 24GB of weights leaves ~26GB for KV
   plus compute buffers. A 180k KV cache is plausibly a double-digit number of
   GB even quantized, so this is tight rather than comfortable — and it
   interacts with (1). Compute it from the model's real layer/KV-head counts,
   or measure it, before declaring the context.
3. **Whether speculative decoding pays on a MoE at all** — reported ~1.35×
   there vs ~1.75× dense, with enormous spread. `ml-kit bench/spec-sweep.sh
   PROFILE=box2`, run ON the Mac (corrallm owns spawning there through the
   token-gated agent). Read `prompt_per_second` first; if prefill dominates,
   the lever is prompt caching / KV reuse, not draft methods.

Everything that does NOT need the hardware is §7 next-steps #4 above, which now
carries the decided design (one unified pool, re-enrol repair path, endpoint lists).
This is only what cannot be done until the box is on the LAN.

**Evidence (measured, do not re-derive):**
- corrallm cross-compiles clean to darwin/arm64 with `CGO_ENABLED=0` (~37 MB; sqlite
  is `modernc.org/sqlite`, pure Go). No release pipeline needed — the daemon can serve
  the agent binary itself.
- Box1 baseline to compare against: RTX 5090, 32 GB, Qwen3-6-27B-MPT resident
  ~15.9 GB / 1 slot, 220k ctx/request, ~1.79 TB/s.

**next (needs the hardware):** 1) **name the chip first** — M4 Max ~546 GB/s vs M4 Pro
~273 swings the answer 2×, and every later number depends on it · 2) measure tok/s AND
prefill against box1 before trusting any of it; token generation tracks bandwidth,
prefill is compute-bound and further behind · 3) confirm the decided unified-pool shape
against what the measurement actually shows.
