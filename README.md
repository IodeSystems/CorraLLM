# corrallm

> Corral + LLM. An OpenAI-compatible reverse proxy, model lifecycle manager, and
> priority/fairshare scheduler with cost-aware overflow. Similar in spirit to
> [llama-swap](https://github.com/mostlygeek/llama-swap).

corrallm puts one OpenAI-compatible API in front of many LLM backends: local
processes it starts and stops, and remote or paid endpoints it forwards to. Under
load it decides who gets served, on which backend, at what quality, and at what
cost, based on the caller's identity.

It runs in production today, fronting a mixed embeddings and chat workload.

## What it does

- **OpenAI-compatible proxy** — `/v1/chat/completions`, `/v1/completions`,
  `/v1/embeddings`, `/v1/rerank`, `/v1/models`, and the audio endpoints (STT, TTS,
  realtime, with optional diarization; see below). Streaming is preserved.
- **Model lifecycle** — starts a backend's `cmd` on demand and waits for a real
  health check (llama-server's `/health`, not just an open port). Concurrent cold
  loads are coalesced, and process groups are cleaned up on shutdown.
- **Residency and eviction** — each server declares its capacity as a vector of
  named memory pools (per-GPU VRAM, system RAM, and so on). A model loads only if
  it fits every pool. When it doesn't, idle unpinned residents are evicted to make
  room (ttl-expired first, then by `evictCost`, then least-recently-used) before
  spilling elsewhere. `persistent` models stay pinned and preloaded.
- **Fairshare scheduling** — each API key maps to a priority group with a weight,
  a share currency, interruptibility, and a per-backend saturation policy.
  Requests are admitted by weighted fairshare over each backend's slots. When a
  backend is full a request queues, is rejected, spills to another backend, or
  preempts a lower-priority one. Preemption is cooperative and safe for in-flight
  streams.
- **Reservations** — a caller can lease a few slots on a model for its own lane so
  interactive work keeps headroom against saturating batch. The slots are held
  *free* (batch drains into the rest and backs off), the lease is short and
  renewed by a heartbeat, and it auto-expires so a dead client can't starve batch.
- **Ordered fall-through and quality degrade** — a served model lists its backends
  in order (round-robin within a cost-equivalent `type`, ordered across types).
  `quality` ranks the tiers. A group can opt into being served by a lower tier
  (`acceptDegrade`, `qualityFloor`), with an optional `maxTokens` clamp on the
  smaller model.
- **Cost model** — everything resolves to dollars: local energy (tokens to
  watt-hours to dollars via `costPerKwh`), paid usage times a cost factor, and the
  energy spent loading a model. Groups can set spending limits over a sliding
  window, and every request records its dwell time, tokens, and cost.
- **Backpressure** — a saturated caller gets a `429` with `Retry-After` (from a
  measured dwell average), `X-RateLimit-Capacity/-InFlight/-Waiting` headers, and
  a JSON hint. `maxWait` and `maxQueueDepth` are configurable.
- **Dashboard** — model, lane, and command definitions; declared capacity and live
  admission load; per-key and per-lane usage (cost, requests, energy, and time as
  bars and time series); queue pressure and sampled queue depth; backend logs with
  parsed `n_ctx`/`n_slots`; and on-demand load/unload. Updates stream over SSE.
- **Auth** — an auto-generated admin token (`<home>/admin.token`) protects the
  management API (`/api/*`). The inference proxy and `/health` stay open.

## Quick start

```sh
make dist                 # build the UI + the ./bin/corrallm binary
ADDR=:8111 ./bin/corrallm serve --config ./corrallm.yaml
```

On first run an admin token is generated at `home/admin.token`; the dashboard,
served at the same address, prompts for it. The OpenAI API is then available:

```sh
curl localhost:8111/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hi"}]}'
```

Useful `serve` flags (all have env equivalents): `--config`, `--db`, `--web-root`,
`--health-timeout` (raise for big models with large KV), `--activity-retention`
(default 30d), `--reservation-max-ttl` (default 5m), `--home`. The listen address
is `ADDR` (default `:6502`).

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

A backend with a `cmd` is spawned and proxied. Without one it's a pure proxy to a
remote or paid endpoint, with auth supplied via `headers`. The caller key is the
API key on the request (`Authorization: Bearer` or `X-Corrallm-Key`); unkeyed
callers fall into the `default` group.

## API surface

- **Inference** (open): the OpenAI endpoints above, plus `/v1/reservations` (lease
  interactive headroom, keyed by caller — see below) and `/upstream/<model>/…`, an
  untracked passthrough to a backend's own web UI that bypasses the scheduler.
- **Liveness** (open): `GET /health`, `/healthz`.
- **Management** (admin token via `Authorization: Bearer` or the `corrallm_token`
  cookie): `POST /api/graphql` and REST at `/api/v1/*` — `overview`, `lanes`,
  `reservations`, `residency`, `activity`,
  `usage/{rollup,by-key,series,series-by-group,queue-depth}`,
  `models/{load,unload,logs}`, and the `events` SSE stream.

## Reservations

Batch work can fill every slot and leave no room for interactive requests.
A reservation fixes that by *proactively* holding capacity free for a lane — the
inverse of preemption, which reactively reclaims a slot after the fact.

Any keyed caller can reserve slots on a model for **its own lane** (the lane its
key maps to). While the lease is live, the effective capacity other lanes see on
that backend is `capacity − slots reserved by other lanes`, so batch is admitted
only up to the unreserved slots and an interactive request finds a free one
immediately. The reserved slots are never force-taken from running batch — batch
just isn't admitted into them, and drains naturally.

The lease is short (default max **5m**, `--reservation-max-ttl`) and must be
renewed by re-POSTing (a heartbeat); it auto-expires, and a reaper frees stale
leases so a crashed client can't hold capacity forever.

```sh
# reserve 1 slot on my-chat for your lane; renew every few minutes
curl -X POST localhost:8111/v1/reservations \
  -H 'Authorization: Bearer my-coder' -H 'content-type: application/json' \
  -d '{"model":"my-chat","slots":1,"ttl":"5m"}'
# → {"model":"my-chat","lane":"interactive","slots":1,"expires_at":"…","renew_within_seconds":300}

curl localhost:8111/v1/reservations                       # list live reservations
curl -X DELETE 'localhost:8111/v1/reservations?model=my-chat' \
  -H 'Authorization: Bearer my-coder'                     # release early
```

`slots` defaults to 1 and may not exceed the backend's `maxConcurrent`; `ttl`
defaults to (and is capped at) the max. Reservations target a model's primary
(top-quality) backend. The dashboard's Lanes page shows live reservations with a
countdown, and `GET /api/v1/reservations` returns the same data.

## Audio (STT / TTS / realtime)

corrallm proxies the OpenAI audio endpoints. An audio backend is just another
`cmd`+`proxy` model whose cost `type` is an audio class, which also marks it
`modality: audio` in the catalog and meters it by bytes rather than tokens. There
are four endpoints:

- `POST /v1/audio/transcriptions`, `/v1/audio/translations` — batch STT
  (Whisper-compatible). `multipart/form-data` with `model` and `file`.
- `POST /v1/audio/speech` — TTS. JSON in, binary audio out.
- `GET /v1/realtime` — realtime STT over WebSocket (the OpenAI Realtime
  transcription schema): stream PCM16 frames and receive live partial and final
  transcripts. A session holds one fairshare slot and is reaped when idle or after
  a max duration.

STT models declare a delivery `modes` list of `batch`, `realtime`, or both (empty
means unrestricted). A batch-only model like parakeet has no `/v1/realtime`, and a
realtime-only model has no batch transcription. The dashboard and the capabilities
manifest gate each surface accordingly, so a client is never pointed at an endpoint
the model can't serve.

A diarizing STT model returns speaker-labeled output alongside the OpenAI `text`:

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

`GET /v1/capabilities` lists every endpoint, the mode-filtered models for each,
working curl and WebSocket examples, and the diarized response shape. Reference
CPU backends live under [`examples/`](examples): `sherpa-realtime-adapter` for
realtime WebSocket STT and `sherpa-diarize` for batch diarized transcripts, both
on sherpa-onnx.

## Stack

- **Backend** — Go 1.26, [Huma](https://huma.rocks) + [gat](https://github.com/iodesystems/gwag)
  (`gw/gat`): one typed handler serves both REST and GraphQL. chi router.
- **Store** — embedded SQLite (`modernc.org/sqlite`, no CGO).
- **Frontend** — React 19 + Vite + TanStack Router/Query + MUI, with a typed
  GraphQL client generated from the committed SDL snapshot.

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

Register once, typed everywhere: add a `gat.Register(...)` in
`internal/api/gateway.go`, run `make gen`, and the typed GraphQL client appears in
`ui/src/gql`.

## Status

Running in production: the proxy, model lifecycle, fairshare scheduler, ordered
fall-through, residency and eviction, preemption, the cost model, quality-degrade
routing, the dashboard, auth, the audio surface (STT, TTS, realtime, and
diarization), request error and payload observability, and the discovery manifest
with the per-model console. Still to come: awareness across multiple nodes.
