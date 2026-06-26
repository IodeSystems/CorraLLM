# corrallm

> **Corral + LLM.** An OpenAI-compatible reverse proxy, model lifecycle manager,
> and priority/fairshare scheduler with cost-aware overflow — a successor in
> spirit to [llama-swap](https://github.com/mostlygeek/llama-swap).

One control plane that herds many LLM backends — local processes it spawns and
remote/paid endpoints it forwards to — behind a single OpenAI-compatible surface.
It decides **who gets served, on which backend, at what quality, and at what
cost**, under contention, per caller identity.

corrallm is running in production, fronting a mixed embeddings + chat workload
(replacing a llama-swap deployment). See [`plan/plan.md`](plan/plan.md) for the
full design, decisions, and roadmap.

## What it does

- **OpenAI-compatible proxy** — `/v1/chat/completions`, `/v1/completions`,
  `/v1/embeddings`, `/v1/rerank`, `/v1/models`, plus the **audio** surface
  (STT / TTS / realtime ws, with optional diarization — see below). Streaming
  preserved.
- **Model lifecycle** — spawns a backend's `cmd` on demand, health-checks it
  (waits for llama-server `/health` 2xx, not just a bound port), coalesces
  concurrent cold loads, and reaps process groups on shutdown.
- **Residency & eviction** — each server declares capacity as a **vector over
  named memory pools** (per-GPU VRAM, system RAM, …); a spawn is admitted only if
  it fits every pool. When a cold model doesn't fit, an eviction solver frees
  idle, non-pinned residents on the binding pool (ttl-expired → low `evictCost` →
  LRU) — **evict-then-spill**. `persistent` models are pinned and preloaded.
- **Fairshare scheduling** — a caller key maps to a **priorityGroup** (weight,
  share currency, interruptibility, per-backend-type saturation policy). Weighted
  fairshare over per-backend slots, with **queue / reject / spill / preempt**
  exits. Cooperative, streaming-safe preemption of lower, interruptible groups.
- **Ordered fall-through & quality degrade** — a served model maps to an ordered
  backend list (round-robin within a cost-equivalent `type`, ordered across
  types). `quality` ranks tiers; groups opt into degrade (`acceptDegrade` /
  `qualityFloor`) and an optional `maxTokens` clamp on the lower variant.
- **Cost model** — everything resolves to `$`: local energy (tokens → Wh → kWh ×
  `costPerKwh`), paid usage × `costFactor`, and swap/load energy. Per-group TCO
  **limits** over a sliding window; per-request dwell/tokens/$ metered.
- **Good-citizen backpressure** — a saturated caller gets `429` + `Retry-After`
  (from a measured dwell EWMA) + `X-RateLimit-Capacity/-InFlight/-Waiting` + a
  JSON hint, with configurable `maxWait` / `maxQueueDepth`.
- **Observability + control plane** (the dashboard) — model/lane/cmd definitions,
  declared capacity, live admission load, per-key & per-lane usage analytics
  (cost / requests / energy / time as bars, line and stacked-area time-series),
  queue-pressure + sampled queue-depth (interactive-starvation watch), backend
  logs with parsed `n_ctx`/`n_slots`, and on-demand **load / unload** of models.
  Updates live over SSE.
- **Auth** — an admin token (`<home>/admin.token`, auto-generated) gates the whole
  management surface (`/api/*`); the inference proxy and `/health` stay open.

## Quick start

```sh
make dist                 # build the UI + the ./bin/corrallm binary
ADDR=:8111 ./bin/corrallm serve --config ./corrallm.yaml
```

On first run an admin token is generated at `home/admin.token`; the dashboard
(served at the same address) prompts for it. The OpenAI API is then available:

```sh
curl localhost:8111/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hi"}]}'
```

Useful `serve` flags (all have env equivalents): `--config`, `--db`, `--web-root`,
`--health-timeout` (raise for big models with large KV), `--activity-retention`
(default 30d), `--home`. Listen address is `ADDR` (default `:6502`).

## Configure

`corrallm.yaml` is the domain config. A minimal two-model example:

```yaml
costPerKwh: 0.14
commandCosts:
  chat:  { generateWattsPerToken: 0.0013, processWattsPerToken: 0.00005 }  # Wh/token
  embed: { generateWattsPerToken: 0,      processWattsPerToken: 0.000002 }

servers:
  box1:
    pools:   { gpu0: 32GB, system: 125GB }   # capacity vector
    reserve: { gpu0: 1GB,  system: 24GB }     # headroom

models:
  my-embeddings:
    persistent: true                          # pinned + preloaded
    backends:
      - cmd: "llama-server --embeddings -hf … --port 5801"
        server: box1
        ramUsage: { gpu0: 1.5GB, system: 1GB }
        proxy: 5801
        type: embed
        maxConcurrent: 1
  my-chat:
    sticky: { ttl: "600s", evictCost: medium }
    backends:
      - cmd: "llama-server -hf … -ngl 99 --port 5800"
        server: box1
        ramUsage: { gpu0: 29.5GB, system: 4GB }
        swap: { loadSeconds: 30 }
        proxy: 5800
        type: chat
        maxConcurrent: 1

priorityGroups:
  interactive:                                # high priority
    weight: 10
    onSaturated: { chat: { queue: true }, embed: { queue: true }, default: reject }
  batch:                                      # yields under load
    weight: 1
    interruptible: true
    onSaturated: { chat: { queue: true }, embed: { queue: true }, default: reject }

keys: { my-coder: interactive, my-indexer: batch }   # API key → group
scheduler: { maxWait: "60s", maxQueueDepth: 8 }
```

A backend with a `cmd` is spawned + proxied; without one it's a pure proxy to a
remote/paid endpoint (auth via `headers`). The caller key is the API key on the
request (`Authorization: Bearer` or `X-Corrallm-Key`); unkeyed callers fall into
the `default` group.

## API surface

- **Inference** (open): the OpenAI endpoints above, plus `/upstream/<model>/…`
  (untracked passthrough to a backend's own web UI — bypasses the scheduler).
- **Liveness** (open): `GET /health`, `/healthz`.
- **Management** (admin token via `Authorization: Bearer` or `corrallm_token`
  cookie): `POST /api/graphql` and REST at `/api/v1/*` — `overview`, `lanes`,
  `residency`, `activity`, `usage/{rollup,by-key,series,series-by-group,queue-depth}`,
  `models/{load,unload,logs}`, and the `events` SSE stream.

## Audio (STT / TTS / realtime)

corrallm proxies the OpenAI audio surface. An audio backend is just another
`cmd`+`proxy` model whose cost `type` is an audio class (which also flags it
`modality: audio` in the catalog and meters it by **bytes**, not tokens). Four
endpoints:

- `POST /v1/audio/transcriptions`, `/v1/audio/translations` — **batch STT**
  (Whisper-compatible). `multipart/form-data` with `model` + `file`.
- `POST /v1/audio/speech` — **TTS**. JSON in, binary audio out.
- `GET /v1/realtime` — **realtime STT** over WebSocket (OpenAI Realtime
  transcription schema): stream PCM16 frames, receive live partial + final
  transcripts. Holds one fairshare slot for the session (idle/max-session reaped).

STT models declare a delivery **`modes`** list — `batch`, `realtime`, or both
(empty = unrestricted). A batch-only model (e.g. parakeet) has no `/v1/realtime`;
a realtime-only model has no batch transcription. The dashboard and the
capabilities manifest gate each surface accordingly, so clients aren't pointed at
an endpoint a model can't serve.

**Diarization.** A diarizing STT model returns speaker-labeled output alongside
the OpenAI `text`:

```jsonc
{ "text": "…",
  "segments": [{ "speaker": 0, "start": 0.0, "end": 3.3, "text": "…" }],
  "num_speakers": 2 }
```

Plain OpenAI clients ignore the extra fields and read `.text`; the console's STT
playground renders the per-speaker segments.

```sh
# batch transcription
curl -sS localhost:8111/v1/audio/transcriptions -H 'Authorization: Bearer <key>' \
  -F model=parakeet -F file=@speech.wav
# text-to-speech
curl -sS localhost:8111/v1/audio/speech -H 'Authorization: Bearer <key>' \
  -d '{"model":"kokoro","input":"hello","voice":"af_heart"}' --output speech.mp3
```

`GET /v1/capabilities` is the machine-readable source of truth — every endpoint,
the mode-filtered models for each, working curl/ws examples, and the diarized
response shape. Reference CPU backends live under [`examples/`](examples)
(`sherpa-realtime-adapter` = realtime ws STT; `sherpa-diarize` = batch diarized
transcripts; both sherpa-onnx).

## Stack

- **Backend** — Go 1.26, [Huma](https://huma.rocks) + [gat](https://github.com/iodesystems/gwag)
  (`gw/gat`): one typed handler → REST + GraphQL. chi router.
- **Store** — embedded SQLite (`modernc.org/sqlite`, no CGO).
- **Frontend** — React 19 + Vite + TanStack Router/Query + MUI; a typed GraphQL
  client generated from the committed SDL snapshot.

## Layout

```
cmd/corrallm/    server binary (cobra: serve, dump-graphql, version)
internal/api/    typed handlers + gat gateway (register once → REST+GraphQL)
internal/proc/   spawn/health/residency/eviction + backend log capture
internal/sched/  fairshare admission, preemption, limits, backpressure
internal/proxy/  OpenAI passthrough + metering, quality-degrade routing
internal/cost/   energy/paid/swap → $ cost model
internal/auth/   admin-token gate for /api/*
internal/store/  SQLite (activity log, rollups, queue-depth samples)
internal/events/ SSE broker for live UI updates
ui/              React SPA; ui/gen/schema.graphql is the committed SDL snapshot
corrallm.yaml    domain config (servers, models, priorityGroups, costs)
```

## Develop

```sh
make dev          # air (Go :6502) + Vite (UI :6503, proxies /api)
make gen          # dump SDL → ui/gen/schema.graphql → typed TS client → lint
make build        # build ./bin/corrallm
make dist         # build UI + binary
make test
```

The "register once → typed everywhere" loop: add a `gat.Register(...)` in
`internal/api/gateway.go`, run `make gen`, and the typed GraphQL client appears
in `ui/src/gql`.

## Status

P0–P11 of the roadmap are shipped and running in production: proxy, lifecycle,
fairshare scheduler, ordered fall-through, residency/eviction, preemption, cost
model, quality-degrade routing, the full observability/control-plane dashboard,
auth, the **audio surface** (STT / TTS / realtime ws / diarization), honest
error/payload observability, and the discovery manifest + per-model console.
Remaining roadmap item: multi-node peer awareness. See
[`plan/plan.md`](plan/plan.md).
