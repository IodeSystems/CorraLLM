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
**Status:** genuinely open. This is the only real fork left on the list.

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

## 2. ❓ `nvidia-power-init.sh` binds power limits by INDEX, which P18 exists to forbid

**Owner:** you (needs root). **Status:** live risk, currently harmless.
**Found:** 2026-08-24, while verifying the P19 thermal question.

### The problem

The 3080's 200 W cap is persistent and correct — `nvidia-smi.service` is enabled and active, and
sets it at boot. But it names the cards by **position**:

```bash
# /usr/local/sbin/nvidia-power-init.sh
nvidia-smi -pm 1
nvidia-smi -pl 450 -i 1   # intended: 5090
nvidia-smi -pl 200 -i 0   # intended: 3080
```

Today that is right, verified live: index 0 = 3080 (200 W of 340 W default), index 1 = 5090
(450 W of 600 W). **But this is the exact identity trap P18 was built to eliminate.** nvidia-smi
enumerates by PCI bus id, and P18's whole finding was that the index MOVED onto the new card when
the 3080 went in — "index is a fine label and a terrible identity." corrallm's pools were fixed to
bind by UUID; this script was not.

### Why it matters

If the indices ever swap — another card, a slot change, a BIOS update reordering the scan — the
script silently applies **200 W to the 5090**. That is the box's entire interactive capacity
running at a third of its power budget, with no error anywhere: the unit succeeds, `nvidia-smi`
reports a valid limit, and corrallm's own pools stay correct because they bind by UUID. The
symptom would be "chat got slow" with nothing to point at.

The reverse half fails loudly (`-pl 450` on a 340 W card is rejected), so the loss is one-sided
and silent in the direction that costs the most.

### The fix

`nvidia-smi -i` accepts a UUID. Two lines, same shape:

```bash
nvidia-smi -pl 450 -i GPU-ee90af07-0882-d325-182e-87137ec6d47b   # 5090
nvidia-smi -pl 200 -i GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544   # 3080
```

These are the same UUIDs box1's pools already bind to, so the script and the ledger would finally
agree on what a card is.

**Needs you** because the file is root-owned in `/usr/local/sbin/` and it runs at boot — I am not
editing a boot script unasked. Say the word and I will write it; or run it yourself with
`! sudo …`.

---

## 3. ❓ Is an *honest* wait estimate even wanted?

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

*Nothing else is open.* The six decisions this file started with were resolved on
2026-08-24 against the running system; each answer and its evidence now sits in the plan item
that needed it. See the `docs(plan)` commit for that pass.
