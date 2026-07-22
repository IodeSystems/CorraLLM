# P16 — Free-tier aggregator (quota-aware remote routing)

Status: **design, not started.** Pointer from §6 roadmap in plan.md.

> This is a design doc, not a changelog. It states the problem, the shape of the
> solution, the facts it rests on, and the unknowns to close before building. It
> follows §0: each build sub-unit ships green + tested; the plan updates in the
> same commit.

## 1. Problem & insight

A single free LLM endpoint is a toy: OpenRouter's `:free` tier is **20 req/min,
50–1,000 req/day aggregate** across all its free models — a rounding error for a
serving proxy. The insight that makes this worth building: **free quota is
enforced per ACCOUNT and those budgets are independent** — across providers AND
across multiple accounts of the same provider. Pool many accounts (Groq ×N,
Cerebras, OpenRouter, …), route across them as each exhausts, and the union is a
real serving budget from free tiers.

So P16 is a **virtual model** (served name, e.g. `free`) backed by many remote
backends — each one endpoint + one key — with a **quota ledger** that tracks each
backend's remaining budget and a **selector** that picks a backend with budget +
acceptable privacy, proactively avoiding exhaustion rather than only reacting.

This extends P14's lane concept from *react-on-error failover* to
*avoid-before-exhaust selection*. Lanes already fail over on a backend error; P16
adds the quota accounting that lets it swap **before** the 429.

## 2. Facts this rests on (research 2026-07-21; verify credential-gated ones at build)

All target providers expose OpenAI-compatible `/v1/chat/completions` with
`Authorization: Bearer`, so corrallm's existing `proxy` backend takes them
**unchanged** — modulo base-path (see Open Questions).

| provider | base URL | strongest free | quota | reset-headers | private? |
|----------|----------|----------------|-------|---------------|----------|
| **Groq** | `api.groq.com/openai/v1` | Llama-3.3-70B | 30 RPM · 1K RPD · 100K TPD (70B); 8B: 14.4K RPD · 500K TPD | ✅ `x-ratelimit-remaining-{requests,tokens}` + `-reset-*` on **every** response; `retry-after` on 429 | no-train, but retained for abuse (ZDR enterprise-only) |
| **Cerebras** | `api.cerebras.ai/v1` | gpt-oss-120b (very fast) | 5 RPM · 30K TPM · 1M TPD/model | ✅ standard headers; 429 body names which bucket | — |
| **OpenRouter** | `openrouter.ai/api/v1` | nemotron-3-super-120b:free, gemma-4-31b:free | 20 RPM · 50 RPD (<$10 lifetime) / 1,000 RPD ($10+ lifetime, permanent) | ❌ **none on success**; poll `GET /api/v1/key` (tracks $ credit, NOT the free RPM/RPD) | account toggle to avoid training providers (separate free/paid) |
| **Together** | `api.together.ai/v1` | varied | unverified this pass | partial | — |
| **NVIDIA NIM** | `integrate.api.nvidia.com/v1` | Nemotron family | unverified | — | — |
| **HF Inference Providers** | `router.huggingface.co/v1` | varied (repo:provider ids) | unverified; token needs `inference.serverless.write` | — | — |
| **Google Gemini free** | (not OpenAI-native) | Gemini | generous | — | ⛔ **trains on prompts + human review** — disqualified for sensitive |

Providers whose free-tier numbers did **not** survive adversarial verification and
must be re-confirmed if used: Mistral La Plateforme, GitHub Models, Cloudflare
Workers AI, Chutes. (Cloudflare does expose an OpenAI-compat path.)

## 3. The one design constraint that shapes everything: observability split

Providers divide sharply on whether they tell you your remaining budget:

- **Header-driven (Groq, Cerebras):** every successful response carries
  `x-ratelimit-remaining-requests/-tokens` and reset timestamps. The ledger reads
  them off each proxied reply — **exact, free, realtime.** This is what makes
  "swap realtime" actually work, and why Groq ranks first.
- **Local-counter (OpenRouter):** nothing on success. `/api/v1/key` tracks dollar
  credit, not the free request quota, so it cannot answer "how many free calls are
  left today." Only trackable by **counting our own requests** against the known
  20 RPM / 50–1,000 RPD, and confirming exhaustion by eating a 429.

⇒ The ledger needs **two backends behind one interface**: `HeaderTracked` (parse
response headers) and `LocalCounter` (count sent requests against configured
limits, reset on a wall-clock window). A provider's config declares which it is.

## 4. Config schema (extends the existing `proxy` model)

**The ledger unit is the BACKEND (one model definition = one key), not the
provider.** Free quota is enforced per ACCOUNT, so pooling a bigger budget means
multiple keys/accounts for the same provider, each an independent backend with
its own ledger entry. `groq-a` and `groq-b` below are the same endpoint and
upstream model with *different keys* → two independent 30-RPM/1K-RPD budgets.
Keying the ledger on "provider" would collapse them into one and throw away the
second account's quota — the whole reason to have it. `provider:` survives only
as a label (for privacy defaults / display), never as the budget key.

A provider backend is a `proxy` model plus a `freeTier:` block. Sketch:

```yaml
models:
  groq-a:
    proxy: { host: api.groq.com, basePath: /openai, model: llama-3.3-70b-versatile,
             headers: { authorization: "Bearer ${GROQ_API_KEY}" } }
    type: chat
    freeTier:
      provider: groq        # LABEL only (privacy defaults, display) — NOT the budget key
      private: true         # false → excluded when a request is marked sensitive
      track: header         # header | counter (how remaining budget is learned)
      limits: { rpm: 30, rpd: 1000, tpm: 12000, tpd: 100000 }  # counter mode / sanity bound
  groq-b:                   # SAME provider + model, DIFFERENT account key → its own budget
    proxy: { host: api.groq.com, basePath: /openai, model: llama-3.3-70b-versatile,
             headers: { authorization: "Bearer ${GROQ_API_KEY_2}" } }
    type: chat
    freeTier: { provider: groq, private: true, track: header,
                limits: { rpm: 30, rpd: 1000, tpm: 12000, tpd: 100000 } }
```

The ledger keys on the served name (`groq-a`, `groq-b`) — each an independent
budget. (Caveat: keys under the SAME account share that account's quota, so the
two would double-count. The pooling assumption is one account per backend;
document it, don't enforce it.)

`lanes:` then composes the backends, best-first, with the local models as the
floor:

```yaml
lanes:
  free: [groq-a, groq-b, cerebras-oss-120b, openrouter-nemotron-super, Qwen3-6-27B-MPT]
```

Requesting `model="free"` gets quota-aware selection across the remotes, falling
to the **local** MTP when every free provider is exhausted — the gateway-SPOF
lesson from the OpenRouter research: remote free is never the sole path.

### Live Groq evidence (P16a smoke test, 2026-07-21) — read before building

A real corrallm→Groq call confirmed the header contract AND surfaced a format the
research missed:

```
X-Ratelimit-Limit-Requests: 1000      X-Ratelimit-Remaining-Requests: 999
X-Ratelimit-Limit-Tokens:   12000     X-Ratelimit-Remaining-Tokens:   11938
X-Ratelimit-Reset-Requests: 1m26.4s   X-Ratelimit-Reset-Tokens:       310ms
```

- **Header names confirmed** (Go canonical caps): `X-Ratelimit-{Limit,Remaining,Reset}-{Requests,Tokens}`.
- **Reset is a Go-DURATION STRING** (`1m26.4s`, `310ms`), NOT a unix timestamp or
  an integer seconds — the ledger's parser must handle `time.ParseDuration`, not
  `strconv`. This is the single most important build detail and the research did
  not have it.
- **Requests bucket ≈ daily** (limit 1000), **tokens bucket ≈ per-minute** (limit
  12000) — two different windows on one response, so the ledger tracks them
  separately (a `resetsAt` per bucket).
- **Continuous/leaky refill**, not a hard reset: `Reset-Requests` was ~86s (time to
  the next refill tick) even for the daily-ish bucket — so `coolingUntil` should be
  min(reset of the exhausted bucket), and budget trickles back rather than snapping
  to full at midnight.
- **Headers pass straight through corrallm's reverse proxy to the client**, so the
  ledger reads them off the upstream response inside the proxy path — no extra
  round-trip, no polling. Still open: the **429 body/headers** (the smoke test did
  not trip a limit) — capture opportunistically before finalizing cooling logic.

## 5. Quota ledger (the new component)

Per `(backend, window)` state — backend = one model definition = one key, so two
keys for the same provider are two independent budgets (see §4). Held in memory
(persistence optional — a lost ledger just re-learns from the next response's
headers, or resets a counter):

- `remaining` (requests, tokens), `resetsAt`, `coolingUntil`.
- Updated on every proxied response: header mode parses `x-ratelimit-*`; counter
  mode decrements by 1 request (+ measured tokens) and rolls at the window edge.
- On **429**: set `coolingUntil` from `retry-after`/`x-ratelimit-reset`; the
  selector skips the provider until then. Distinguish per-minute (short cool) from
  daily (cool until next UTC/rolling day) — see Open Questions, this is the gap
  that needs empirical probing per provider.

## 6. Selector algorithm

Given a request to a free lane (+ optional `sensitive` flag):
1. Candidate = lane members whose ledger shows `remaining > 0` and not
   `coolingUntil > now`, and (`private` if `sensitive`).
2. Order = lane order (best-first) — NOT round-robin; we want the best model that
   has budget, deterministically.
3. On send, pre-decrement counter-mode ledgers optimistically; reconcile from
   headers on response.
4. On 429/5xx, mark cooling, drop to next candidate (this is existing lane
   failover + the new cooling state).
5. All exhausted → fall to the local floor model.

## 7. Overlap with OpenRouter's own routing — reconcile, don't duplicate

OpenRouter itself does provider-failover (default on) and a `models[]` fallback
array. Within a **single** OpenRouter lane entry, let OpenRouter handle its own
sub-fallback; corrallm's ledger treats the whole OpenRouter entry as one provider
budget. Do not try to micromanage OpenRouter's upstreams from corrallm.

## 8. Open questions — close before/at build (several credential-gated)

- ✅ **Base-path handling** (P16a, done). It was NOT supported — the Director set
  scheme/host but forwarded the client path unchanged, silently dropping any base
  path. Added `ProxyTarget.BasePath` (object-form `basePath:`, normalized to a
  single leading slash) and a `joinPath` prefix in the reverse-proxy Director.
  Groq is `basePath: /openai` (the `/v1` arrives on the request), OpenRouter
  `/api`; empty is a no-op so local backends are untouched. Tested:
  `/v1/chat/completions` → `/openai/v1/chat/completions`.
- **429-vs-daily distinction.** For each provider, the exact status/body/headers
  distinguishing "slow down (per-minute)" from "done for the day" is **not
  documented reliably** (the OpenRouter 402 assumption was refuted). Must probe
  empirically with a real key per provider — capture status + body + headers at
  both a per-minute and a daily exhaustion. Do NOT assume 429-vs-402.
- **Header set variance.** Confirm Cerebras/Together/NIM actually emit the OpenAI
  `x-ratelimit-*` names (documented for Groq; inferred for others).
- **Token counting in counter mode.** OpenRouter has no TPM headers; counting
  tokens locally needs the response `usage` block — confirm it's always present.
- **Privacy enforcement.** `sensitive` is a per-request flag — where does it come
  from? A header, a key-to-group mapping, or a lane property? Decide before build.
- **Streaming.** Do rate-limit headers arrive on streamed responses (before the
  body)? If not, counter mode must handle streams.

## 9. Phased build order (each a green, tested sub-unit per §0)

- **P16a — one provider, header-tracked.** ◐ In progress.
  - ✅ base-path support (this commit) — the blocker; fully tested, no key needed.
  - ✅ model-id rewrite (this commit) — `proxy: {model: <upstream id>}` →
    `ProxyTarget.Model`, substituted into the outbound body's `model` field at
    the dispatch site (beside clampMaxTokens). No-op when unset (local backends)
    or on a non-JSON body. Tested unit + end-to-end (upstream receives the
    substituted id, not the served name).
  - ✅ **live Groq validation** (2026-07-21). Key wired via `.env` → `${GROQ_API_KEY}`.
    One real corrallm→Groq call proved base-path + model-rewrite + auth end-to-end
    (Groq echoed its own id "llama-3.3-70b-versatile"; reply "groq works") and
    captured the real rate-limit headers — see the evidence block in §5. The
    header contract is now known, including the Go-duration reset format the
    research missed.
  - ✅ **quota ledger + API** (2026-07-21). `internal/quota.Ledger` learns each
    backend's per-window budget (requests + tokens) from the X-Ratelimit-* headers
    on every proxied response — parsing the Go-duration reset with
    time.ParseDuration — sets `coolingUntil` on a 429 (Retry-After, else the
    exhausted bucket's reset), and answers `Available(backend)`. Wired via the
    reverse-proxy `ModifyResponse` hook (no-op for local backends); exposed at
    `GET /api/v1/quota`. Live-validated: two Groq calls, ledger showed
    remaining 1000→998, resetsIn 2m53s, available=true. 12 tests
    (quota + api). **429 cooling is unit-tested but not yet live** (no limit
    tripped); capture a real 429 body opportunistically to confirm the header set.
  - ✅ **self-cap, staleness, selector, console card** (2026-07-21) — the four
    P16b pieces, all live-validated:
    - **Self-cap** (`freeTier.cap: {requests, tokens}`): treats budget as spent
      once usage reaches the cap, leaving provider headroom unspent —
      `EffRemaining = remaining - (limit - cap)`. Live: Groq with `cap.requests: 1`
      read `available=false` at 999 provider-remaining.
    - **Staleness**: the API marks a bucket `stale` once its reset has passed
      (window rolled, count is last-known) and reports `observedAgoSec` — the
      count is a snapshot, not a live tick, and now says so.
    - **Selector** (`filterByQuota`): the candidate walk drops backends the ledger
      says are exhausted/cooling; keeps the unfiltered walk if that would empty it
      (never a blind 503). Live: `model="free"` with Groq capped-out routed to the
      local MTP floor.
    - **Console card** at `/quota` — per-backend budget bars, cap chip, cooling
      state, staleness. UI typechecks + prod-builds (node_modules was fine; the
      earlier "blocked" read was LSP noise, not the real build).
- **P16b — DONE** (folded above): the `free` lane + quota-aware selection with
  cooling + local floor, all live-validated.
- **P16c — counter-mode + OpenRouter.** Local-counter tracking; the empirical
  429/daily probe; OpenRouter as a counter-tracked member.
- **P16d — privacy tiering.** `private` flag + `sensitive` routing.
- **P16e — refresh.** Periodic pull of OpenRouter's `:free` roster (the volatile
  one) into proxy entries; other providers have stable model lists.

## 10. Non-goals / risks

- **Not a production SLA.** Free tiers are best-effort; the local models remain
  the floor. Never let a free provider be the only path (gateway SPOF).
- **Terms-of-service risk.** Aggregating free tiers to build a serving budget may
  violate some providers' terms — flag per provider at build; this is the user's
  call, not corrallm's to assume.
- **Privacy.** Gemini free trains on prompts; several others retain for abuse
  monitoring. The `private` flag is a guardrail, not a guarantee — sensitive
  traffic should prefer the local models.
- **Quota values are volatile.** Every number here is version-dependent and has
  changed before; the ledger must degrade gracefully when a limit shifts under it
  (header mode self-corrects; counter mode needs its config kept current).
