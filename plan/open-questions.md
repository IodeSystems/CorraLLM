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

## 4. ❓ Should aw4 ride the `chat` lane?

**Owner:** you. **Gates:** nothing — **unblocked as of 2026-09-06**. The attribution fix this
question waited on is deployed and verified on live traffic: a request for `chat` now records
`served=local-Qwen3.8-27B, requested=chat`. Moving aw4 no longer costs per-model visibility.

`chat` being a one-rung ladder is a **design, not a gap** — it is the indirection you
retarget when you experiment or upgrade, and it means "the best we can do". That makes the
open question the other way round from how it first looked:

> **aw4 asks for `local-Qwen3.8-27B` by name — 12,294 of 12,300 requests in 24 h.** A model
> name resolves to exactly one candidate (`internal/config/config.go:1324`), so aw4 does not
> ride the indirection. Retarget `chat` tomorrow and your dominant caller keeps hitting
> whatever you retargeted it *from*.

`yscr` already does it the intended way: 4,070 requests over 17 days (2026-08-13 → 08-30)
addressed to `chat`. So the pattern works and is in use; aw4 is the one outside it.

### What makes this a decision rather than a chore

Moving aw4 onto `chat` means an upgrade reaches it **silently and immediately**, which is the
point and also the risk: a model swap changes aw4's answers with no aw4-side change and no
announcement. Pinning is the opt-out from exactly that. Which you want is a judgement about
how much you trust an upgrade to be an upgrade, and it is yours.

### Supporting fact, since it came up

The `free` lane is not overflow for `chat` traffic and nothing suggests it should be:
`acceptDegrade=false` on all three groups, so no ladder is walked down to a lesser model
regardless. And the free rungs are thin — 239 requests served by a remote provider ever,
last 2026-09-01, of which 118 failed: 105 were 429 from the providers' own free tiers (groq
83, `openrouter-z-ai-glm-5.2` 22).

**Decision resolved 2026-09-06: cerebras removed.** It had been answering 402 Payment
Required since at least 2026-09-01 — an unpaid account, not a corrallm fault — so the free
lane's second declared quota was a rung nobody could stand on. Provider definition and lane
membership deleted (config revision 33; revision 32 rolls it back, and `presets.go` still
carries Cerebras so re-adding it is a form, not a rewrite). The reason is recorded in the
groq model's notes, next to the sentence it invalidated.

Every other rung was probed at the same moment and is alive: `groq-gpt-oss-120b` and nine of
the ten OpenRouter pool models answered 200, `openrouter-poolside-laguna-xs-2.1` answered 429
(a free-tier rate limit, which is the tier working as sold). `openrouter-liquid-lfm-2.5-2.6b`,
whose one recorded failure was `no permitted credential` on 2026-08-15, serves fine — that
record was stale, not a gap.

Overflow is also not a live pressure: turn-aways over the last three days are 12, 4, **0**
against ~25k requests, and queueing caps at 15 s.

### What unblocked it

`served` used to be the request's own model field, never reassigned to the candidate that
won, so every lane-addressed row named the lane. Prometheus had the right name all along
(`proxy.go:685`), which made one request two disagreeing accounts. Now `served` is the model
that answered and `requested` is what the caller wrote; a request nobody served repeats the
requested name rather than crediting a backend that refused. History is not backfilled — the
answering model was never recorded, so reconstructing it would be inference dressed as
measurement. Expect a discontinuity in per-model charts at the cutover.

It also caught something nobody was looking for: `life-raglit` addresses models by **alias**
(`nomic-embed-text` → `local-nomic-embed-text`), which was being mis-recorded the same way
lanes were. Aliases and lanes are now both visible as indirection.

**Optional extension, deliberately out of scope:** `requested` is not on the API or the
dashboard yet, so "who uses lanes" is an SQL question for now. Worth a panel only if the answer
to this question is "move aw4".

---

## 5. ❓ The remap out: the calls corrallm may not guess

**Owner:** you. **Gates:** the three contracts of the remap out that are still
unbuilt (`plan.md` §6, design in [`p35-corral-layer.md`](p35-corral-layer.md) §8–§9).
**Status:** ◐ **three open, down from five.** `stream=true` was answered and shipped
2026-09-23; the canonical audio schema was ratified and shipped the same day — both
deleted from the list below per the contract at the top of this file. What is left
(2, 5, 6) was never waiting on the schema and is not unblocked by it.

**Answered — the governing principle.** "We aim to take the mish mash of non standard audio
tools and corrall them into a common, portable API." So **no backend's shape is canonical;
corrallm publishes one and maps every backend onto it.**

**Answered — the mission.** "Yes, project mission statement is openai compatible." So the
shape corrallm publishes is **OpenAI's, wherever OpenAI published one**. That closed the
last gating sub-choice, and the canonical transcription body shipped the same day —
[`p35-corral-layer.md`](p35-corral-layer.md) §9.0c, §12, as-built with its measurements in
§12.11. Deleted from the list below. **The larger half of that instruction — "ensure that is
the case", which is not scoped to audio — is tracked as P35 §13 and is work, not a
question.**

Three entries went with it, and their answers are in §12 rather than here. **(3) whose
diarization shape is canonical** — neither; both map onto OpenAI's, and the premise that
OpenAI published none expired. **(4) is a per-request speaker UUID a bug or a fact** — a
FACT, and it is now exposed as one: it is gone from the default body (where it invited a
client to key a database on a value that changes every call) and lives in
`backend.speakers[].uuid` beside its canonical label, under `include[]=backend`.
Stabilising it across calls stays refused — that is speaker recognition (§9.4), a feature,
not a remap, and §9.0's last paragraph is what says so.

**Answered and SHIPPED — (1) `stream=true` on a path that cannot stream: synthesise.**
Deleted from this list per the contract at the top of this file. The answer, the code and
the measurements live in `plan.md` §6 and [`p35-corral-layer.md`](p35-corral-layer.md)
§9.1. **`/v1/audio/speech` was deliberately NOT included and is still open** — TTS streams
audio chunks, not SSE events, and that answer must not travel by momentum.

**Answered — the robustness principle.** "I prefer the Berkeley method: accept generously,
produce and document strictly." Asymmetric on purpose: tolerance on the way IN, exactly one
documented shape on the way OUT. It settles four of the schema's sub-choices outright
(`words[].end` omitted rather than computed · the `backend` sidecar opt-in via
`include[]` · one `segments[].id` type everywhere, string · `diarized_json` to a
non-diarizing model stays 400) and **reopened the `data: [DONE]` divergence as in-scope**
— corrallm's output must terminate identically whichever backend answered, so oidio's own
stream gets normalised too. **That one is now SHIPPED** (§12.10.2): the real stream was
measured first and had TWO divergences, not one — the missing terminator, and a
`transcript.text.done` with no usage block. The terminator is appended; the usage block is
not, because a real stream measured no duration and computing one is what this same
principle already forbade for `words[].end`. It does NOT reach capability: accepting a
request for data no backend produced is not generosity. Reasoning and the boundary:
[`p35-corral-layer.md`](p35-corral-layer.md) §9.0b.

**Why the rest are decisions and not chores.** Two of the three still open below (5 and 6)
are renames. **2 requires corrallm to produce something the backend never returned** — a
translation from a transcription. That is not compatibility, it is invention, and inventing
without being asked is how a corral layer starts lying about what it is fronting. 7 is new,
and it is a schema question rather than a behaviour one.

Each is verified against the wire or the source; do not re-derive them.

2. **`/v1/audio/translations` does not translate.** oidio maps it straight onto
   `handleTranscriptions`, so it transcribes in the source language. The shape claim is true and
   the behaviour claim is false — **re-verified 2026-09-23 through the canonical remap: the
   route now returns the canonical body field for field and still reports
   `"task":"transcribe"`.** Route it to something that translates, refuse it as
   unimplemented, or document the lie. ~~Or have corrallm translate the transcript with a
   chat model.~~ **The chat-model option is struck 2026-09-23 — the owner caught it, and
   it should never have been on the list.** NeMo-Speech.cpp translates natively
   (`src/nmt/translator.h` → Riva Translate 4B Instruct v2; `src/speech/speech_translator.h`
   for streaming; composed ASR→NMT→TTS in its README), and it fits the 3080: ASR +
   diarization ≈ 1.5 GB, + NMT at Q4 ≈ 3.9 GB, against 4,970 MiB free today on a 10 GB
   card. **Estimates from parameter counts, not measurements — nothing here has run
   NeMo.** The question stays open because oidio serves the route today and does not
   translate; what changed is that its answer is now conditional on NeMo landing rather
   than on a build-vs-buy call corrallm gets to make.
5. **`response_format: url` on `/v1/images/generations`.** OpenAI defaults to `url` and honours
   the field; corrallm only ever emits `b64_json`. Serving a URL means corrallm hosts the bytes —
   a store, a lifetime, a retention policy. Worth it, or is `b64_json`-only the documented limit?
6. **`/v1/realtime`'s advertised audio format.** corrallm hardcodes "PCM16 mono @ 24 kHz". That
   is oidio's default (`engine/realtime.go:56`) but is per-model configurable, and its WebRTC
   transport decodes Opus at 48 kHz regardless. Report what the model is actually configured
   with (requires asking oidio), or keep the constant and say it is a default?
7. ❓ **NEW 2026-09-23 — should a model be able to DECLARE that it diarizes?** Nothing in
   corrallm's model record says so today: `capability` is derived from the cost `type`
   string (`config.TypeIO`), and both oidio's diarizer and NeMo are `type: stt`. The
   consequence is live and measured: `diarized_json` to a non-diarizing model is refused
   400 as the schema requires, but **at RESPONSE time rather than at the door** — the
   caller gets 400 while the activity row records the backend's 200, because the only
   honest signal available is what came back. A substring check on the type string was
   rejected: it would 400 a NeMo-backed model that diarizes under another name, which is
   worse than a late refusal. **The fix is a declaration** — a `diarizes` flag, or an
   `audio` `ModalitySpec` field beside `formats`, probed-beats-declared like everything
   else there. **This is your call because it changes the config schema**, and because the
   alternative (leave it late, document it) is defensible: the request really did cost GPU
   time, and the row says what was served upstream. Evidence and reasoning:
   [`p35-corral-layer.md`](p35-corral-layer.md) §12.9(g), §12.11.

**Not blocking today** — phase 1, the `stream=true` contract and the canonical
transcription body are all shipped and self-contained, and P35 phase 2 (PCM bounds on
`ModalitySpec`) needs none of these. **Nothing is blocking a phase any more:** the two
that were (the `stream=true` answer and the canonical schema) are both shipped. What is
left blocks the three contracts nobody has started — 2 blocks contract 3, 5 blocks
contract 4, 6 blocks contract 5 — and 7 only improves one that already works.
