# P16 — Free-tier aggregator (quota-aware remote routing)

Status: **design, not started.** Pointer from §6 roadmap in plan.md.

> This is a design doc, not a changelog. It states the problem, the shape of the
> solution, the facts it rests on, and the unknowns to close before building. It
> follows §0: each build sub-unit ships green + tested; the plan updates in the
> same commit.

## 1. Problem & insight

A single free LLM endpoint is a toy: OpenRouter's `:free` tier is **20 req/min,
50–1,000 req/day aggregate** across all its free models — a rounding error for a
serving proxy. The insight that makes this worth building: **the daily/minute
caps are per-provider and independent.** Pool N providers, each with its own free
quota, and route across them as each exhausts, and the union is a real serving
budget from free tiers.

So P16 is a **virtual model** (served name, e.g. `free`) backed by many remote
provider endpoints, with a **quota ledger** that tracks each provider's remaining
budget and a **selector** that picks a provider with budget + acceptable privacy,
proactively avoiding exhaustion rather than only reacting to it.

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

A provider backend is a `proxy` model plus a `freeTier:` block. Sketch:

```yaml
models:
  groq-llama-70b:
    proxy: { host: api.groq.com, basePath: /openai/v1,
             headers: { authorization: "Bearer ${GROQ_API_KEY}" } }
    type: chat
    freeTier:
      provider: groq                 # ledger key (shared across a provider's models)
      private: true                  # false → excluded when request marked sensitive
      track: header                  # header | counter
      # header mode: which headers to read (defaults cover the OpenAI-style set)
      # counter mode: declare the limits to count against
      limits: { rpm: 30, rpd: 1000, tpm: 12000, tpd: 100000 }
```

`lanes:` then composes them, best-first, with the local models as the floor:

```yaml
lanes:
  free: [groq-llama-70b, cerebras-oss-120b, openrouter-nemotron-super, Qwen3-6-27B-MPT]
```

Requesting `model="free"` gets quota-aware selection across the remotes, falling
to the **local** MTP when every free provider is exhausted — the gateway-SPOF
lesson from the OpenRouter research: remote free is never the sole path.

## 5. Quota ledger (the new component)

Per `(provider, window)` state, held in memory (persistence optional — a lost
ledger just re-learns from the next response's headers, or resets a counter):

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
  - ◻ model-id rewrite (served name → upstream id in the request body) — unit-
    testable without a key.
  - ❓ **parse Groq's rate-limit headers into a ledger + expose in console — GATED
    ON A GROQ API KEY.** The design's whole point ("prove the header path
    end-to-end") is a live Groq call, and open questions #2–4 (real header names,
    429-vs-daily body, token counting) can only be closed against the live API.
    No key is on this box. Building the ledger before probing the real responses
    would be building against unverified assumptions — do not. Get a key (env
    `GROQ_API_KEY` or a file; never pasted in chat) and resume here.
- **P16b — the ledger + selector.** Generalize to a `free` lane; quota-aware
  selection with cooling; fall to local floor. Test with Groq + one more.
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
