# corrallm — design & roadmap

> Corral + LLM. An OpenAI-compatible reverse proxy + model lifecycle manager +
> priority/fairshare scheduler with cost-aware overflow. Successor in spirit to
> llama-swap (clean-room; reuse *patterns* from redline2, not code).

**How this plan works: §0.** Active work: §6. Known gaps: §7. Deployment: §8.
Capacity-parked: §9. Finished trees live in [`plan/done.md`](done.md); deferred opt-in
next-steps in [`plan/icebox.md`](icebox.md); **decisions that need you** in
[`plan/open-questions.md`](open-questions.md).

## Status — 2026-08-24

**In production on `:8111`, fronted at `https://llm.iodesystems.com`.** corrallm replaced
the llama-swap fork and serves the live workload from two hosts (box1 + an enrolled
64 GB MacBook Pro).

Shipped and running: the engine (P0–P8 — proxy, fairshare scheduler, ordered
fall-through, residency/eviction, preemption, cost model, quality degrade), the
observability + control plane, the **audio/media API surface** (P9 — OpenAI audio
endpoints; backends scoped out to [oidio](https://github.com/IodeSystems/oidio) in P12),
PDF auto-conversion (P13), lanes + single-path models (P14), the bench binary (P15),
the free-tier aggregator (P16), pause (P17), token counting (P20), provider credentials
(P21a–i), the Qwen3.8-27B cutover (P22), per-mode samplers (P23), local-as-a-provider
(P24), the toolchain registry with versioned builds, rollback and pins (P25/P27/P29),
config in SQLite (P26), tickets (P28), and the box1 thermal envelope (P19). Full trees + evidence:
[`plan/done.md`](done.md).

**Two decisions need you** — [`open-questions.md`](open-questions.md): the agent lease
self-reap policy, and whether an honest wait estimate is even wanted. Neither blocks today.

**The dashboard was rebuilt around the questions people arrive with, 2026-09-04/05.** Seven Lenny
runs across two scenarios drove it: ten nav entries named after data became seven named after
questions, Traffic got one time axis, every control that reaches live work says what it costs, and
the home screen answers "is it serving" and nothing else. The verdict moved from *"No. Not on my
own, anyway."* to an unqualified yes for the operator; the caller still says no, and what that
needs is a decision (§6, `open-questions.md` §3). Full tree, composition table and traps:
[`done.md`](done.md) § Lenny.

**Open work is §6**, and it is smaller than it looks: the `ramUsage` placement/size split left
over from P18, P21c budget granularity, the Q8_0-referenced KLD sweep, streaming the trial
transcript + the add-model form, wait-estimate accuracy, `host.Remote` for multi-node, and one
live verification of the per-device VRAM attribution. Parked on hardware: §9.

**Numbering.** Git history is authoritative for phase numbers. A second, dashboard-flavoured
series was written into the old roadmap under numbers that collide with it (P15a/P15b, P17–P21);
those trees are archived in `done.md` § Dashboard & observability under descriptive names.
**Next free number: P30.**

**Deviations from the original design:** (1) UI served from `--web-root` dir, not `go:embed`
(matches redline2); (2) live events use **SSE**, not WebSocket (server→client only, no
dependency, EventSource auto-reconnects — subBroker fan-out preserved). Store is minimal
(activity log + rollup query); no sqlc.

---

## 0. Working this plan

This file is the single source of truth for status. Keep it honest and current —
update it in the **same commit** as the code it describes.

**Status marks.** ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.
A slice is ✅ **only** when its functional unit meets the Definition of done below — and a
✅ slice does not stay here: it moves to `done.md` in the same pass.

**Four files.** `plan.md` = current state, active work, conventions ONLY.
`done.md` = the archive of finished trees, kept for evidence and traps, never a to-do list.
`icebox.md` = deferred, opt-in next-steps. `open-questions.md` = decisions that need the USER,
and **only while they are open** — a resolved question is deleted from it and its answer written
into the plan item that needed it. **Phase numbers come from git history** — next free is P30.

**Re-check a question against the box before you ask it.** On 2026-08-24, six of eight standing
"user decisions" turned out to be already answered by the running system, the migrated config, or
a comment in the code. Verify first; a decision list nobody prunes hides the ones that are real.

**A phase is a functional unit.** Each `Pn` is an independently shippable slice: it
compiles, its behavior is tested, and the engine still runs with it landed. Don't
start `Pn+1` until `Pn` is ✅. A phase too big to land at once → split it into
sub-units (still each a green, tested commit), not a half-done checkbox.

**Definition of done (per functional unit) — all must hold before ✅:**
1. `go build ./...`, `go vet ./...`, `go test ./...` green; `gofmt -l` reports nothing.
2. New behavior has tests: a unit test for logic, an integration/e2e test for any
   request-path change. A bug fix lands with the regression test that catches it.
3. UI changes: `bin/gen` re-run, `tsc`/eslint clean, the SDL snapshot committed.
4. This plan updated **in the same pass**: the finished tree moved to `done.md` with its
   commit hash, a one-line pointer left here only if something still depends on it; resolved
   decisions moved to `done.md` § Resolved decisions; new discoveries filed (rules below);
   the §Status block synced. A stale plan is worse than none.

**Committing.** Conventional commits. Scaffolding and implementation are **separate**
commits (`chore: scaffold X` then `feat: X`). Commit each functional unit on its own —
never batch unrelated phases. The plan-doc update rides with its phase's commit or a
trailing `docs(plan):` commit (as the P0–P5 history shows). **Don't push unless asked.**

**Every active slice in §6 carries:** **next** (the next concrete step) · **risks** (what
could go wrong / what is untested) · **blocking decisions** (a call the USER owns — name it,
surface it, don't guess) · **optional extensions** (nice-to-haves explicitly out of scope).
Record assumptions so they are catchable.

**Filing new work as you discover it — put it in exactly one place:**
- Needed for the *current* slice → add a sub-item to it and do it.
- A follow-on something active needs → a new **§6** slice with its own next/risks.
- Improves the product but nothing active requires it → **`icebox.md`**.
- Out of scope until much later → **`icebox.md`** § Deferred.
- A shortcut / known gap in code already shipped → **§7 Known gaps**.
- A decision only the user can make → **`open-questions.md`**, AND named beside the work in §6.
  When it is answered: write the answer + its evidence into the §6 slice, mark that slice
  **decision resolved**, and DELETE the question from `open-questions.md`.

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

## 6. Active work

Status marks: ◻ todo · ◐ in progress · ✅ done · ⏸ parked · ❓ blocked.
Everything ✅ has moved to [`plan/done.md`](done.md) — this section is only what is
still open. Items parked on hardware rather than on a decision are in §9.

### ◐ carlsmacbookpro is back — 51 GB of Metal that was switched off — 2026-09-05

**Up, self-healing, and self-updating.** It had been dark since July with 51 GB of Metal and
68 GB of system memory idle, and `local-Qwen3.6-35B-A3B-MTP` configured to run there, while box1
sat at 96% on one slot.

**Why it was dark, and it was two things.** (1) The agent was never installed as a service — no
LaunchAgent, nothing in `launchctl`; it was started by hand once and died with the session, and
`corrallm service` is systemd-only so on macOS the tool cannot install itself (a CLI-10 gap).
(2) On first start it enrolled, self-updated, re-exec'd — and the re-exec inherited the now-spent
enrollment token and died with "enrollment token was already used". Almost certainly the original
failure, unwitnessed. The current build already fixes that with a shell-neutral `agent.yml` whose
`enrollToken` is dropped once exchanged; this machine still had the legacy `agent.env`.

**The blocker that actually cost the time, and the wrong turn I took.** Every heartbeat failed
with `no route to host` while `curl` reached the same address from the same shell and returned
200. That is a dead ringer for macOS 26's per-binary Local Network privacy gate, and I built two
workarounds on that theory — a bash-exec launcher, then a fresh-binary-path-per-start scheme —
rationalising each failure into a refinement. **`tccutil reset LocalNetwork` ended it: "Service
name is invalid on this platform."** Local Network is not a TCC service there at all.

The real cause was in the routing table, one command away the whole time:

    default   192.168.1.1   UGScIg    en0
    default   link#26       UCSIg     bridge101   !     ← REJECT route

Two default routes, the second a reject route via a VM/sharing bridge (192.168.252.1; there is a
`vboxwebsrv` LaunchAgent). Go's dialer lands on it and gets EHOSTUNREACH; curl picks differently.
`curl --interface bridge101` returns `000`, en0 and en6 both return 200. Deleting the route needs
root on that machine, which we do not have.

**What is running now** (host config, untracked by this repo, on box1):

| | |
|---|---|
| `corrallm-mac-tunnel.service` | `ssh -N -R 127.0.0.1:18111 → 127.0.0.1:8111`, `Restart=always`. Loopback has no route ambiguity; the agent's `primary` is the tunnel. |
| `corrallm-mac-agent.timer` | every 2 min, runs `~/.local/bin/corrallm-mac-agent.sh`: starts the agent over SSH if nothing is listening on 6503. Idempotent. |

Verified: killed every agent on the Mac, started nothing by hand, and the timer restored it —
`up` on the next check. Self-update verified end to end afterwards: it spotted a new build by
BUILD HASH (the version strings matched), replaced its own binary, re-exec'd and came back
authenticated on `06e7f72`.

**next** — let the scheduler actually use it: nothing has spilled there yet, and the first real
test is a saturated box1 falling through to the 35B on Metal.
**risks** — the tunnel is a single point of failure for the heartbeat, though `Restart=always`
plus the 2-minute timer covers a restart. `bin/agents/` was rebuilt (`make agents`) to publish
`06e7f72`, which changes what every attached machine self-updates to — one machine today.
**how to undo all of it** — delete the reject default route on that Mac (needs root there), point
`primary` back at `http://192.168.1.76:8111`, and remove both units and the script. Nothing in
corrallm needs to change.
**a real gap this exposed** — `corrallm service` supports systemd only. A Mac compute node has no
supported way to install itself, which is why this one was fragile in the first place.

### ◻ What the Lenny runs left open — 2026-09-05

Seven runs over two days and two scenarios, the harness, P30 A–D, and everything they fixed are
archived in [`done.md`](done.md) § Lenny. This is only what is still true and still costs
something.

**The decision, and it gates the three under it** — `open-questions.md` §3: **what does a CALLER
see?** A person handed an API key today gets the operator's dashboard with one credential. The
caller run is the evidence: the door asks for an "Admin token" he cannot get (0 on three of four
questions), the last screen hands him a GPU serial number, and `Change`/`Unassign` follow him
onto the page that is supposedly his own. Three ways out: filter these pages by role, a separate
caller page, or close it and let callers ask the operator. Everything below marked *(caller)*
depends on which.

- ◻ *(caller)* **Nothing says which row is you.** Ten keys listed, none marked as the reader's.
  He identified himself by luck — the one key that appeared in a live request from an address he
  recognised.
- ◻ *(caller)* **The front door speaks only to an admin**: "CORRALLM — ADMIN SIGN IN … `cat
  /home/nthalk/.corrallm/admin.token`". It never uses the word "key", which is the only thing he
  was given.
- ◻ *(caller)* **Which model names go in an editor?** The catalog lists thirteen names — models,
  lanes, proxies, absent ones — without saying which are requestable. He only found out by
  accident, from an example `curl` that used `"model":"chat"`.
- ◻ **`PLACEMENT` is still the label** on three tables and a section heading, and the canon names
  that exact word as one that must be the gloss and never the label (OB-3). Both personas hit it.
- ◻ **Machines does not say whether its numbers are a problem.** `gpu0` at `96%`, a tool marked
  `behind` — neither says whether to act. "I left it alone, but I did so guessing, not knowing."
- ◻ **A backend load records no reason.** The activity log says a backend loaded, never why —
  which is why this morning's diagnosis took a database session instead of a glance. The one
  product gap the slowdown investigation left (`done.md` § Lenny, and the card that now explains
  a slowdown covers the symptom, not the cause).

**next** — the caller decision, then `PLACEMENT`, which is cheap and unblocked.

**Slot cache — built, wired, OFF.** `internal/slotcache` + `--slot-cache-dir`, measured before
it was written (save 643 ms, restore 331 ms, 11x against reprocessing; 36.8 KB per token).
Turning it on needs the flag plus a restart; the backend precondition is already met.
`X-Corrallm-Conversation` lets a caller name its conversation rather than have the head of its
prompt guessed at — **filed with aw4** (`~/local/src/iodesystems/aw4/plan/icebox.md`), because
aw4 is one caller with many concurrent nodes and is therefore the traffic that thrashes a
one-slot backend. Inert until the cache is on, so it can land there whenever.
**risks** — a vocabulary fix is where vocabulary bugs are born; name the object.
**how we will know** — run 8 on both scenarios, same build, reporting composition against
`done.md` § Lenny's table.

### ✅ Slot cache is working — 2026-09-05 21:48, first hour measured

**Vision stays**, so `local-Qwen3.8-27B` keeps its mmproj. That makes `--cache-reuse`
permanently unavailable *for that flag alone* — llama.cpp disables it for multimodal models and
says so at every load — and it is out of the cmd as of revision 32.

**A correction to what this entry said an hour ago.** It claimed the benefit was unproven and
speculated that multimodal might break slot restore too. The first was measured too early against
my own contention; the second was invented — the log line is about the `cache_reuse` flag and
says nothing about the prompt cache, which plainly works here (aw4 sits at 97.7% reuse and
llama.cpp reports `f_sim_best = 1.000`).

**What the first hour actually shows**, `sk-aw4` on this model:

| | requests | mean answer | prompt reuse | tokens re-read |
|---|---|---|---|---|
| the 3 h before, cache off | 1,847 | 3.7 s | 94.8% | 3,159 |
| since 21:48, cache on | 743 | **2.7 s** | **97.7%** | **1,174** |

And the mechanism is visible doing it, on real traffic rather than a probe:

    22:43:22  slot cache backend=local-Qwen3.8-27B caller=sk-aw4 restored=true
    22:43:23  slot get_availabl: selected slot by LCP similarity, f_sim_best = 1.000

Every restore is followed by a perfect prefix match. **One hour, one workload** — not settled,
but the direction is clear and the mechanism is observable.

**Cost so far:** 21 states, 12 GB in the first hour, against a 64 GB cap. At that rate the cap is
reached in ~5 hours and eviction runs continuously thereafter, so the cap decides how many
conversations stay warm rather than how much disk is used. ~1 TB is free; raising it is one flag
and a restart.

**next** — read it again after a full day, and after aw4 sends `X-Corrallm-Conversation`
(filed in aw4's icebox), which replaces the inferred key with a declared one.
**risks** — the inferred key is a guess about aw4's conversation shape; if it is wrong, the cost
is a save that buys nothing, and the swap rate in the log is how that would show.

### ◐ ~~`--cache-reuse 256` is live~~ — RETRACTED 2026-09-05 21:49

**Applied** (config revision 27, one line, restorable): `local-Qwen3.8-27B` spawns with
`--cache-reuse 256` beside its existing `--parallel 1`. More slots is the textbook fix for
prefix thrash and it costs VRAM the box does not have (gpu0 at 96%) — **decided: no more slots
until the GPUs are paid for**, so this is the free half.

**What it can fix:** llama.cpp re-uses cached chunks by KV-shifting them, so when a caller PRUNES
the head of a long conversation — everything shifts, no prefix matches, today the whole prompt is
re-read — the suffix can be reused in place. The 10:00–14:00 window fits that shape: average
prompt size FELL from 62–80k to 40–49k while reuse collapsed, which is what head-pruning looks
like. **What it cannot fix:** two genuinely different conversations interleaved on one slot. They
still trade the cache, and no flag changes that.

**How it was applied, since the obvious ways are wrong here.** `config load` replaces the whole
configuration and needs `--force`; the entry-YAML API refuses this model (`served name
"local-Qwen3.8-27B" collides with a model declared elsewhere`) because it is authored under
`providers.local`, not at the top level. The right door is `upsertModel`, which merges onto the
existing model via `applySpec` — so `sampling`, `convert`, `modalities`, `aliases`, `pool`,
`swap` and `contextPerRequest` survive, and `sticky` is re-sent whole (2m / 5m / medium) because
the form rebuilds that block wholesale. Verified: `config export` differs from the pre-change
export by exactly one line.

Restarted at a moment with nothing in flight, then loaded deliberately so no caller paid the cold
start. Verified in `pgrep`: `--parallel 1 --cache-reuse 256 … -c 188000`. A chat request answers
correctly with speculative decoding still on, and the log is clean of KV-shift errors.

**THE BASELINE TO JUDGE IT AGAINST, frozen here before the change:**

| | value |
|---|---|
| prompt-token reuse, 24 h to 14:30 | **92.0%** (10,515 requests) |
| mean time answering, same window | **5.0 s** |
| the bad stretch, 13:00–14:00 | 57.2% reuse, 10.8 s, 17,148 tokens re-read/request |
| requests finding no near-exact prefix (llama.cpp `f_sim_best < 0.95`) | 09:00 **30%** → 13:00 **68%** → 14:00 **9%** |

**How to check it tomorrow** — the instrument exists now, which it did not this morning:
the reuse figure is on Now (it fires a card when reuse drops a fifth AND time per request rises a
third), and per hour:

    journalctl --user -u corrallm --since today | grep -oE 'f_sim_best = [0-9.]+'

**What would say it worked:** the `f_sim_best < 0.95` share falls during a head-pruning stretch
without reuse collapsing with it. **What would say it did nothing:** the next slow window looks
like this one — reuse down, time up, f_sim low. Either answer is worth having; it is one number
now instead of an argument.

**Early read, 3 hours in (17:35) — NO MEASURABLE EFFECT, and the flattering version of that
number is a trap.** Against the 24 h baseline it looks like a win: 96.0% reuse and 2,649 tokens
re-read per request, against 91.9% and 4,649. That comparison is worthless, because the baseline
window CONTAINS the degraded stretch. Like for like:

| window | reuse | re-read/request | mean |
|---|---|---|---|
| 14:00–14:36 — recovered, still no flag | **98.8%** | 1,192 | 2.6 s |
| 14:36–17:35 — with the flag | 96.0% | 2,649 | 4.1 s |
| yesterday 15:00–18:00 — normal, no flag | 95.4% | 2,235 | 3.8 s |

Against the honest comparison — the same hours yesterday, both periods healthy — it is 95.4% →
96.0% and 2,235 → 2,649 tokens re-read. **Nothing.** Which is what the flag's own description
predicts: it helps a prompt whose head was pruned and whose suffix reappears shifted, and normal
operation is not that. The `f_sim_best` figure I first reached for (15% below .95 since the flag,
against 51% for 09:00–14:00) is the same trap wearing different clothes — that 51% IS the
degradation, not a baseline.

**THE FLAG WAS NEVER ACTIVE. Retracting the whole experiment, 2026-09-05 21:49.** llama.cpp says
so at every load, and I did not read its log until something else sent me there:

    W srv load_model: cache_reuse is not supported by multimodal, it will be disabled

`local-Qwen3.8-27B` carries an mmproj for vision, so `--cache-reuse` is disabled the moment the
backend starts. The "early read" above measured nothing; the honest comparison it corrected was
also measuring nothing. **Both readings are void** — not "inconclusive", void. The flag can come
out of the cmd at the next restart; it costs nothing except the impression that something was
tried.

**What this is worth keeping:** the reason the reading looked plausible is that I compared
against a window and not against the mechanism. A flag that a backend silently disables produces
exactly the numbers of a flag that does nothing, and no amount of care with the WINDOWS would
have found it. The backend's own startup log would have, in one line, before the experiment
began.

### ✅ Per-request priority group, bounded by the key (2026-08-31)

Asked for by aw4, which is one caller with two very different kinds of traffic:
an assistant a person is blocked on, and autonomous work nobody is waiting for.
One key per role would have made it N fairshare claimants against yscr's one —
key generation boosting a client, structurally, by its own design.

- `keys:` accepts either a bare group name (unchanged, every existing config) or
  a policy: `{default: batch, interactive: true}`.
- The credential carries the choice — `sk-aw4:interactive`. In the key rather
  than a header because every OpenAI-compatible client can set an API key and
  few can set a header; a weighting the caller cannot express is one it will not
  use.
- The whole credential is matched as a key FIRST, so a key that literally
  contains `:` keeps resolving to itself. Adding this cannot change what any
  existing credential means. Validation rejects such a key carrying escalations,
  since the suffix form is unreachable for it.
- A refused escalation is SERVED in the key's own group, not rejected — failing
  a request over a weighting is worse than serving it at the weight it is
  entitled to — and says so with `X-Corrallm-Group-Denied`. A silent downgrade
  (revoked permission, renamed group, typo) is the failure that costs a day.
- Cost attributes to the base key. Keyed on the raw credential, one tenant's
  spend would split across a row per group and `RollupByKey` would answer "what
  did this caller cost" with a fraction of it.
- ⚠️ **Permission is not a budget.** The flag says who may ask; nothing bounds
  how much. "A person is waiting" is a judgement the caller makes about itself,
  and the likely failure is a bug — a default left interactive, a rule that
  drifts — which corrallm cannot detect. The guard is `limits` on the escalated
  group. Set one before granting this to anything autonomous.
- `config_key` gained an `allow` column (JSON array) plus the first entry in a
  new `migrations` list — `CREATE TABLE IF NOT EXISTS` is a no-op on the
  production database, so without it the column would exist only on fresh
  installs and every write on box1 would die with "no such column". Tested by
  building the old table and migrating it.

### ⚠ The claims-nothing rule is live, and the recorded caveat understated it

Shipped with the split, as decided: an unmeasured model on a DEVICE pool now claims **nothing**
rather than reserving the whole pool, and the spawn is the fit test. Host RAM keeps the
conservative path, and so does any host that cannot measure per-process memory.

**The decision recorded "a CUDA OOM kills only the allocator". That is true and it is not the
whole risk — the allocator is not always the newcomer.** A resident model that grows AFTER load
can be the one that asks for memory an unmeasured neighbour already took. Qwen's vision path
spikes ~2 GB on a 400-dpi page, and that is exactly the shape of the 2026-08-14 production OOM,
where Qwen3-6 crashed with 1,483 MiB free.

Exposure is one spawn per (card, model): after that, measurement governs. It is bounded, it was
decided deliberately, and it is written down here rather than discovered later.

**next** watch for a spawn-time OOM on box1's first cold load of any newly-added model. If one
bites an INCUMBENT rather than the newcomer, that is the signal to reinstate a floor — reserve
some fraction of the pool for an unmeasured model instead of nothing.
**risks** as above. `gpu1` is the likely first place to see it: 831 MiB of headroom once nomic
and chandra are both resident, and chandra's footprint is input-driven.

### ◐ P21c — budget granularity

Design doc: **`plan/p21-provider-credentials.md`**. P21a–i shipped (archived in `done.md`);
P21c is the remaining slice.

**next** P21c — move budgets to where the provider actually meters. P20's per-model `usd`
budget is the wrong granularity: a `usd` cap on a discovery template becomes N independent
caps (12 models × $200 = **$2,400, not $200**). The live openrouter template carries such a
cap today.
**risks** the preset table's basePaths were probed once, on 2026-08-15; a vendor moving one
shows up as a 404 from a host that plainly exists. The catalogue browser prints the URL it
tried, which is the intended cure.
**assumption** browsing uses a credential's STATIC headers, matching what the discovery loop
does; a credential using `authTokenCommand` reports that it cannot be browsed rather than
sending an unauthenticated request and blaming the endpoint.
**✅ BOTH BLOCKING DECISIONS RESOLVED 2026-08-24.**

1. **Where secrets live — a `0600` file, never served.** Taken as recommended; nothing
   contradicted it and no alternative was ever proposed. The constraint that forces it is
   unchanged: `/api/v1/config/*` serves config as YAML, so a key-management UI that persisted
   secrets into config would turn a working endpoint into a disclosure surface. Keep secrets
   out of the config object entirely — not redacted on read, *absent*, so no future handler can
   leak them by forgetting to redact. P21f/g are unblocked.
2. **The `discover:` question is MOOT — it already deployed and the consequence is absorbed.**
   The live config reads `"directory":{"filter":{...}}`, migrated by P26 into the config store;
   there is no `discover:` left to warn about. The 12 discovered models are already gone and the
   `free` lane is already down to its two declared members (`groq-gpt-oss-120b`,
   `cerebras-gpt-oss-120b`). **Measured cost of that loss: 12 requests in 7 days**, one each to
   groq and cerebras. Not worth a decision.
   Whether to build a real free pool (`extensions.free.virtual`) is an **icebox** item, not a
   blocker — see `icebox.md` § Free pool.

### ◐ P22 — the open tail (KLD baseline, clean head-to-head, probe scoping)

Cutover ✅ 2026-08-14 — Qwen3.8-27B is the daily driver at Q5_K_M `-c 188000`, 3.6 retired.
All VRAM measurements, the bench run (77/90) and the corrections are in `done.md`; **do not
re-derive them.** Three things stayed open:

- **KLD baseline** — owed before ANY sub-Q5 discussion. Blocked on a choice: the proper
  reference is BF16 (46.55 GiB, does not fit the 5090) so it is CPU-slow, or a Q8_0 baseline
  that makes every number relative to Q8_0 rather than truth.
- **A clean head-to-head** — only obtainable now by restoring 3.6 from
  `~/.corrallm/config.yml.bak-pre-cutover-*`. `out/20260814-112505` stays VOID as a
  standalone result, though its 57 completed stages were good enough for the overlap
  comparison. **The swap was never quality-tested** — it was decided on VRAM, capability and
  speed, and quality vs 3.6 measured as a wash (48 vs 47 on 57 overlapping stages) off a run
  where 3.6 was crash-looping.
- **Probe scoping bug (`mcpshell-instructions`)** — it runs on arms that lack mcpshell and
  fails there by construction, costing 4 spurious failures. Fix is in the mcpshell repo
  (`bench/probes/mcpshell-instructions/task.yaml`), **not here**. Until then, subtract 4 from
  any headline failure count.

**✅ DECISION RESOLVED 2026-08-24 — Q8_0 baseline, labelled relative-to-Q8_0.** BF16 is
46.55 GiB against a 32,607 MiB card, so the "proper" reference is CPU-only and would take the
box out for the duration to produce one number. Q8_0 fits and runs on the GPU.

The label is the whole of the discipline here: every KLD figure derived this way measures
**divergence from Q8_0, not from truth**, and Q8_0 carries its own small divergence from BF16.
That is fine for the question actually being asked — "is a sub-Q5 quant meaningfully worse than
what we serve today" is a *relative* question — and it is wrong for any absolute claim. Write
the reference into the result or the number will be read as absolute the first time someone
quotes it.

**next** run the Q8_0-referenced KLD sweep before any sub-Q5 discussion. Nothing else blocks it.
**optional extensions** surface aliases in the Overview model form rather than only in
`advancedFields`; 3.8 past 188k needs one of the priced levers, not more window.

### ◐ Trial spawn + add-model form *(was mis-numbered P16 — git's P16 is the free aggregator)*

Authoring a model is currently "type a cmd string into a YAML box, save it, load it, read
the logs, guess again". Every iteration mutates live config.
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

### ◻ Fix the wait estimate *(was mis-numbered P15b — git's P15 is bench)*

The measurement trees this rests on (retry promises, utilization, service-time distribution)
are archived in `done.md` § Dashboard & observability. **Read the censoring note there before
validating any new estimator against `realWaitMs`.**

**Reframed 2026-08-03 by the first 30 min of retry-promise data — the original premise was
WRONG, and is recorded here so it is not re-derived.**
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

### ◻ `host.Remote` — the last multi-node step

Everything else in the multi-node ordering shipped: per-server VRAM accounting, live config
reload, the `internal/host` Spawner interface, `Server.Agent` binding, `corrallm agent`,
failure semantics, darwin capacity, and enrollment + the Agents/Config UI. Shape (b) —
full remote control, box1 spawns onto machine 2 — was decided 2026-07-28.

**next** `host.Remote`, integration-testable by running a second agent on another port on box1.
Then: switch the daemon's default config to the managed one and retire the hand-written file.
**❓ blocking decision (USER) — ONE, and it is `open-questions.md` #1:** agent lease self-reap
on/off and its TTL. An agent that loses its primary but stays up keeps its backends running
(`internal/agent/server.go:307` names this an open decision in so many words). The primary side
is already settled and shipped — no self-reap, adoption reconciles on heartbeat — but the agent
side is not. Answer it before `host.Remote`, not now.

**✅ Transport trust — RESOLVED 2026-08-24, and it was already answered in the code.** The agent
executes arbitrary shell strings because that is its function: the primary sends `sh -c` strings
and the agent runs them. `internal/agent/wire.go:14` states the position and the mitigation —
*"a remote-code-execution surface by design, and a token is required unless one is explicitly
waived. Treat exposing it exactly as you would treat exposing a shell."* Enforced at
`server.go:110` (`agent token required`).
There is no new trust to grant: corrallm's config already carries `cmd:` strings it execs
locally, so the agent widens the *reach* of that trust to a second box, not its kind. Accepted as
designed. **The standing rule that follows from it:** never expose an agent port anywhere you
would not expose a shell, and never waive the token outside a test.

**◻ Whether to spike `proc_pid_rusage`'s `ri_phys_footprint`** for real per-process measurement
on darwin — a task, not a decision. Today the Mac declares `noProcessMemory: true` and `ramUsage`
becomes authoritative there, with validate-time enforcement so a model omitting it is an error
rather than a box that silently serves one model at a time. That is a working fallback; the spike
would only improve it. Not blocking.

**Design + the full shipped ordering** (shape (a) vs (b), the enrollment/agent/darwin/failure
steps, box2's unified-pool decision and the re-enrol repair path, and the agent-addressing rule
that makes `Server.Agent` a LIST of endpoints) is archived in `done.md` § Multi-node. **One thing
there is a live trap:** `config.IsLocalHost`'s "a private-LAN address is not local" doc comment is
already wrong under this topology — the predicate is still correct, the comment is not.

### ◻ OSS follow-ups (not blockers)

- Auth multi-user accounts/roles + token rotation — today is a single shared admin token.
- Rename the `WattsPerToken` cost fields to `WhPerToken` (the values are already Wh/token;
  the name is the only thing wrong).

---

## 7. Known gaps in shipped code

Resolved decisions and closed gaps are in [`plan/done.md`](done.md); decisions still needing
you are in [`plan/open-questions.md`](open-questions.md). What follows is only what is still
true and still costs something. Optional extensions and out-of-scope items live in
[`plan/icebox.md`](icebox.md).

### Known gaps — shipped and live with these

- ✅ ~~**`/m/<name>/activity` is a route that can never render.**~~ Deleted 2026-09-05. It was a
  full page (the shared activity table scoped to the model, with `?placement=` narrowing) that
  `ModelConsole` never rendered — no `<Outlet/>` — so the URL resolved and silently showed the
  parent's Info tab, and nothing in the UI linked to it. Deleted rather than wired up: the model
  page's own Usage tab answers the same question, and Traffic now answers it for any window. A URL
  that replies with the wrong page is worse than a 404.
- **`carlsmacbookpro`'s agent is unreachable** (`dial tcp 192.168.1.248:6503: connect: connection
  refused`, observed 2026-09-04). The Hosts page shows it, in the daemon's own words. Same class as
  the known macOS Local Network permission trap; unverified which it is this time.

- **P6 known gaps:** (1) swap $ is charged to the load *trigger* only — not amortized across the
  coalesced batch; a load whose trigger loses the ctx race goes unbilled. (2) Over-budget with a
  `queue` stage degrades to back-off (reason `over-budget` + Retry-After), not an internal
  budget-wait — the client retries when the window frees. (3) Usage capture caps at 1 MiB; a
  non-streaming reply larger than that meters as $0 (streaming keeps a rolling tail). (4) `cost`
  share-currency is retrospective (decayed past releases), so in-flight cost is invisible to
  fairshare until release.
- **Test-teardown race**: a held in-flight request can log after `store.Close()` in one test
  (benign warning); revisit if it becomes flaky.
  (3) **`interactiveOrigins` not ported** — llama-swap's browser-origin auto-priority has no corrallm
  equivalent; browser callers land in `default` unless keyed (design choice — priorityGroup is first-class).
  (4) **`queued_ms` is forward-only** — rows predating the column read 0; queue *wait* populates as new
  queued-then-served requests accumulate (rejections + sampled depth are already live).


- ✅ ~~**The qwen 502s need the upstream ~120 s timeout raised.**~~ **No longer observable —
  measured 2026-08-24, 7 days of production traffic.** Three 499s in the window, at 3.6 s, 6.0 s
  and 12.8 s — client cancels, not a 120 s guillotine — and the longest SUCCESSFUL request dwelled
  **4,676 s (78 minutes)**, which no 120 s cap upstream could have survived. Either the front-proxy
  `proxy_read_timeout` was raised or the traffic mix stopped reaching it. P10a's honest reporting
  is what makes this checkable at all: a 499 now means what it says. Reopen only if 499s reappear
  clustered near a round number.

### The activity DB is 5.9 GB, and it is `req_body` — measured 2026-08-24

**Not a leak, and not urgent.** Recording it so nobody re-derives it.

| | |
|---|---|
| database | 5,914 MB, `freelist_count` **0** — no unreclaimed pages, it is all live data |
| `activity` table | 5,635 MB (95% of the file) |
| `req_body` column | **5,330 MB (90% of the file)**; `resp_body` is 242 MB |
| retention | **working** — oldest row is 29.7 days old against a 30 d setting. It has plateaued, not grown |
| disk | 380 GB free of 1.8 T |

**The cap is 256 KiB, not 4 KiB.** `payloadCap` (4 KiB) bounds a captured RESPONSE;
`reqBodyCap` is `256<<10`, raised deliberately so a full agentic request — system prompt + tool
schemas + multi-turn history + tool results — stays VALID JSON and can be replayed in the
console. A 4 KiB truncation left it unparseable and replay degraded to dumping raw text.
Overridable with `CORRALLM_REQBODY_CAP`.

**Where the bytes are:** 23,669 requests in the 64 KiB–1 MiB bucket account for 4,718 MB, and
the largest is 262,172 bytes — exactly the cap. These are agentic turns, where the same
preamble is re-sent every turn and stored in full every time. `dun` on `Qwen3-6-27B-MPT` alone
is 11,391 requests / 2,200 MB. (Traffic splits across `Qwen3-6-27B-MPT` and `local-Qwen3.8-27B`
because activity logs the REQUESTED name and the legacy spelling is an alias — expected, and
already recorded under P22.)

**Measured, not guessed:** per-row gzip on real payloads is **3.8×** — 5,330 MB would become
~1,400 MB. Compressing 200 concatenated rows instead gives 254×, which is not an achievable
column-encoder number but does say where the redundancy lives: **between** rows, not within
them. The same system prompt and tool schemas are stored thousands of times. The proxy already
knows this — P21's cache work measured a 72.3% prompt-cache hit rate and 231.6M tokens served
from cache.

**✅ RESOLVED — payload aging shipped (2026-08-24), 7 days by default.**
`--payload-retention` (`CORRALLM_PAYLOAD_RETENTION`, default 168h, 0 disables) clears
`req_body`/`resp_body` on rows older than the cutoff and KEEPS the row with every metric on it.
Runs in the same 5-minute maintenance tick as the row prune. Full tree in
[`done.md`](done.md) § payload aging.

**Verified against a real copy of the production database**, not a fixture: 57,205 rows cleared,
payloads 5,573 MB → 779 MB, and every aggregate byte-identical — rows 65,617, cost $6.276954,
prompt tokens 1,222,460,967, dwell 522,830,856 ms, cached tokens 1,028,349,212. Second pass
cleared 0 in 72 ms.

**✅ VACUUM run on the live database 2026-08-24** during the same maintenance window as the
deploy, with the daemon stopped so nothing contended for the lock. **5,916 MB → 803 MB**, 6.4 s,
`pragma integrity_check` = ok, and every aggregate identical across the rewrite (rows 65,617,
cost $6.276954, prompt tokens 1,222,460,967, dwell 522,830,856 ms).

`auto_vacuum` stays off, which is the right default: from here the payload prune keeps the file
flat by returning pages to the freelist for reuse, and a full rewrite is only ever needed after
a one-off backlog like this one.

Not taken: compressing the column (~3.8× per row, measured) — the aging removes the same bytes
without a format change or a decompress on read.

### Open decisions the USER owns

**They live in [`plan/open-questions.md`](open-questions.md), and there are two.**

| # | question | gates | urgency |
|---|---|---|---|
| 1 | Agent lease: self-reap on/off, and its TTL | `host.Remote` (§6) | not yet — answer before that step |
| 2 | Is an *honest* wait estimate even wanted? | the wait-estimate formula (§6) | not yet — gather a wider traffic mix first |

**Six others were closed on 2026-08-24 by checking the box instead of re-reading the plan.**
The answers and their evidence are recorded in the §6 slices that needed them, not here:
secrets location (P21c), the `discover:` filter (P21c — moot, already deployed), the KLD
reference (P22), gpu1 residency (VRAM slice — settled by 7 days of usage), transport trust
(multi-node — already answered in `wire.go`), and comfort-fill (P9f — never a decision;
it moved to `icebox.md`). P19's thermal tree closed with them and archived to `done.md`.
One known gap closed the same way: the upstream ~120 s timeout is no longer observable.

**The lesson worth keeping.** Six of eight "decisions" had already answered themselves — the
power cap was applied and persistent, the config had migrated, the trust position was written in
the code, the usage data made a four-way policy call a non-contest. A decision list that is not
re-checked against the running system becomes a list of questions nobody needs answered, and it
hides the two that are real.

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

---

## 9. Capacity-parked (relocated from `~/inflight` 2026-08-04, and again 2026-08-10)

Three items that were sitting on the global shelf because they wait on a machine,
not on a decision. They belong here — the work and the config are both corrallm's.
Resume conditions are kept verbatim so nothing has to be re-derived.

### ⏸ Deploy the hardened Qwen chat template
**waits-on:** systems — the llm host must be quiet.
**check:** `[ "$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits -i 1)" -lt 20 ]`
(exit 0 = ready. Samples ONE instant — observed flipping ready→busy within minutes.)

> **⚠ This check is index-based, and P18 is the reason to distrust that.** `-i 1` names a
> *position*, and the whole finding of P18 was that the nvidia-smi index MOVED onto the new card
> when the 3080 went in — "index is a fine label and a terrible identity". Qwen serves from gpu0.
> Confirm which card `-i 1` is before trusting a ready/busy answer from it, or re-express the
> check against the UUID the pools bind to.

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
**waits-on:** systems — the Mac, for a measurement run. **The chip question is ANSWERED**
(M1 Max, 64 GB, ~400 GB/s — below), so the old "name the chip" condition is retired.
**check:** manual — the box is on the LAN, enrolled, and self-updating its agent.
**next:** measure tok/s AND prefill against box1 before trusting the roofline below; then
confirm the decided unified-pool shape against what the measurement shows. Two of the three
unmeasured things are still unmeasured: whether 35B-A3B fits at a useful context, and whether
speculative decoding pays on a MoE at all. (The wired limit was checked and is fine.)

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

**next (needs the hardware):** 1) ~~name the chip first~~ — **done: M1 Max, 64 GB** ·
2) measure tok/s AND prefill against box1 before trusting any of it; token generation tracks
bandwidth, prefill is compute-bound and further behind · 3) confirm the decided unified-pool
shape against what the measurement actually shows.
