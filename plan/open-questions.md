# Open questions

Decisions and problems that are **genuinely undecided** and need the user. Nothing else
lives here.

**The contract.** A question stays here only while it is open. When it is resolved, the
answer and its evidence go into the plan item that needed it (`plan.md` §6, or `done.md`
if the tree is finished), that item is marked **decision resolved**, and the question is
**deleted from this file**. A resolved question kept here is the same failure as a stale
plan: it sends the next reader looking for a fork that no longer exists.

**Before adding one, check it is real.** Most of the eight "user-owned decisions" this file
started with had already answered themselves in the running system — the cap was applied, the
config had migrated, the code carried the decision in a comment. Verify against the box, not
against the prose.

Last triaged: **2026-08-24** (against live box1 + the production database).

---

## 1. ❓ Agent lease: self-reap on/off, and its TTL

**Owner:** you. **Gates:** `host.Remote` (`plan.md` §6) — the last multi-node step.
**Status:** genuinely open, and the only one of the two that gates unbuilt code.

### What is undecided

An agent that **loses its primary but stays up** keeps its spawned backends running. Should it
reap them itself after a timeout (a lease), or hold them until a primary comes back?

This is *not* the primary-side question, which is already decided and shipped: the primary does
**not** self-reap a down agent's ledger, because per-server ledgers make a stranded reservation
harmless to other hosts, and reconnect ADOPTION reconciles on every heartbeat (matching keys
adopted, orphans reaped past a 60s grace). That is settled. The agent side is not:

> `internal/agent/server.go:307` — "It does NOT cover the agent being killed abruptly or losing
> its primary while staying up — that needs a lease, which is a deliberate open decision."

### What each choice costs

| | **Lease ON** (agent self-reaps) | **Lease OFF** (current behaviour) |
|---|---|---|
| network blip | **kills an in-progress cold load.** box1's Qwen cold-loads in ~66 s; the Mac's would be slower. A blip longer than the TTL destroys work that was nearly done. | survives — the backend keeps running, adoption reconciles it on reconnect |
| real partition | memory is released, host is reusable | **strands the host's whole budget** (50 GB on the Mac) until someone intervenes |
| failure mode | silent and automatic | visible: the host looks full and nothing explains why |

### What the answer probably hinges on

Whether the Mac is on a link you trust. The endpoint list is `[192.168.1.249, 192.168.252.1,
192.168.1.58]:6503` — a LAN address, a VPN address, and an alt. A laptop that sleeps, moves
between networks, or drops off Wi-Fi produces *exactly* the blip that a lease punishes. That
argues **lease OFF**, or a TTL well above a cold load (≥5 min) rather than a tight one.

**Not blocking today** — `host.Remote` is unbuilt and the Mac is reachable. Answer it before
that step, not now.

---

## 2. ❓ Is an *honest* wait estimate even wanted?

**Owner:** you. **Gates:** "Fix the wait estimate" (`plan.md` §6) — the formula change, not the
measurement. **Status:** open. **Missed in the 2026-08-24 triage** — the §6 slice named it, this
file did not.

### What is undecided

corrallm's `Retry-After` is short **every single time** — measured 1.7×–11.6× short, median ~4×.
Fixing the arithmetic is easy. Whether to fix it is not:

- A truthful **25 s** may drive callers away (`gone`) — they give up and the slot that opens goes
  unused.
- An optimistic **4 s** keeps them retrying into a slot that does, in fact, open.

That is a product call about what a 429 is FOR — an apology, or an appointment — not an
arithmetic fix.

### What the data says, and what it cannot

Measured 7 days: **407 rejections, mean 14.2 s, max 15.0 s.** That maximum is not a coincidence —
it is `maxWait: 15s` truncating. Which is the trap:

> **`realWaitMs` is CENSORED, structurally.** It averages requests that queued *and were then
> admitted*. Anyone who waited past `maxWait` became a `queue-timeout` and left the sample. The
> longest waits are exactly the ones removed, so the measured mean is biased LOW and **can never
> exceed 15 s no matter how bad the queue gets**. First live reading: est 2.0 s · real 4.6 s ·
> theory 3 m 49 s.

So the honest number is not currently knowable from this box's own telemetry, and any new
estimator "validated" against `realWaitMs` will be validated against a ceiling.

Behaviour so far argues the optimistic side is not hurting: **zero `early`, one `gone`** — the
main caller (`dun`) retries, is refused, and backs off more patiently than we asked.

**Not blocking today** — §6's next step is to watch the est-vs-real gap across a wider traffic
mix first. One caller against one single-slot model is not a sample.

---

*Nothing else is open.* The six decisions this file started with were resolved on 2026-08-24
against the running system, and the boot-script problem found during that pass was fixed the
same day. Each answer and its evidence sits in the plan item that needed it.

---

## 3. ❓ P30 — what does a CALLER see?

**Owner:** you. **Gates:** nothing today; it is the scope boundary of P30.
**Status:** open, deliberately out of P30's scope until you say otherwise.

A person handed an API key currently gets the operator's dashboard, `Unload` and `Restore`
included. Either they get a filtered view of the same pages, or a separate surface. Bigger than a
reorganization, and the Lenny scenario for it is already written up in `icebox.md`.

---

## 4. ❓ Is the free lane overflow capacity, or decoration?

**Owner:** you. **Gates:** nothing built — this is a routing-config decision, and the
reason to write it down is that the current state *reads* as insurance and is not.

**The claim under test:** remote providers absorb overflow when box1 saturates.

### What is actually true (measured 2026-09-06, live box1 + production database)

Four gates stand between that intent and the mechanism, and each is a deliberate setting.

1. **Nothing asks for a lane.** aw4 requests `local-Qwen3.8-27B` *by name* — 12,294 of the
   12,300 requests in the last 24 h. `ResolveServed` returns exactly one candidate for a
   model name (`internal/config/config.go:1324`), so no ladder is ever consulted. Requests
   that did name a lane exist only from testing: `chat` 148, `free` 35, all-time.
2. **`chat` is not a ladder.** Its resolved membership is one rung, `local-Qwen3.8-27B`.
   A caller that *did* ask for the lane would get the same box1 model. `free` resolves to
   13 rungs (2 declared + 11 from the `free` pool).
3. **`acceptDegrade=false` on all three groups** (`batch`, `default`, `interactive`). A
   multi-rung ladder still would not be walked down to a lesser model — the request queues
   or is refused instead.
4. **The free rungs are thin and partly unconfigured.** 239 requests served by a remote
   provider *ever*, last on 2026-09-01. 118 of them failed: 105 were 429 from the providers'
   own free tiers (groq 83, `openrouter-z-ai-glm-5.2` 22), `cerebras-gpt-oss-120b` 7/7
   `no backend available`, `openrouter-liquid-lfm-2.5-2.6b` `no permitted credential`.

**And the condition it would insure against is not currently occurring.** Turn-aways per
day over 14 days run 137, 11, 206, 16, 12, 19, 0, 0, 109, 1, 12, 0, 4, **0** — the last three
days are 12, 4, 0 against ~25k requests, and queueing caps at 15 s. Today: 4,434 requests,
nobody turned away.

This is the same shape as the Mac (`done.md`, "The Mac, measured"): capacity that exists
and sits in no lane. The difference is that here it is four gates deep.

### The fork

- **Wire it** — give `chat` real rungs, set `acceptDegrade` on `batch` (aw4's group), fix the
  two credential gaps. Cost: an aw4 answer can silently become a free-tier 120b answer, and
  the 429 evidence says that rung is itself rate-limited, so the fallback can fail too.
- **Leave it, and stop calling it overflow** — `free` keeps serving deliberate `model: "free"`
  requests, box1's ladder stays one rung by design, and the strategy note says "spare
  capacity for work that opts in", not "insurance". Costs nothing; matches what is running.
- **Neither yet** — revisit when a day's turn-aways go back above ~5%, which is where
  2026-08-26 sat (206/3,559).

**No evidence favours wiring it today.** The recommendation is the second option, because
the only thing currently wrong is the belief, not the routing.
