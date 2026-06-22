# corrallm

> Corral + LLM. An OpenAI-compatible reverse proxy + model lifecycle manager +
> priority/fairshare scheduler with cost-aware overflow.

One control plane that herds many LLM backends — local processes it spawns and
remote/paid endpoints it forwards to — behind a single OpenAI-compatible surface.
It decides who gets served, on which backend, at what quality, and at what cost,
under contention, per caller identity.

See [`plan/plan.md`](plan/plan.md) for the full design and roadmap.

## Stack

- **Backend**: Go 1.26, [Huma](https://huma.rocks) + [gat](https://github.com/iodesystems/gwag)
  (`gw/gat`) — one typed handler → REST + GraphQL (+ gRPC later). chi router.
- **Store**: embedded SQLite (`modernc.org/sqlite`, no CGO). Config is YAML.
- **Frontend**: React 19 + Vite + TanStack Router + MUI, typed client via
  graphql-codegen + graphql-request off the committed SDL snapshot.

## Layout

```
cmd/corrallm/    server binary (cobra: serve, dump-graphql, version)
internal/api/    typed handlers + gat gateway wiring (register once → REST+GraphQL)
internal/config/ YAML domain config + layered .properties loader
internal/store/  SQLite (activity log + metric rollups)
internal/webui/  SPA handler (serves ui/dist from disk)
ui/              React SPA; ui/gen/schema.graphql is the committed SDL snapshot
home/            layered .properties config (application + per-slot)
corrallm.yaml    the domain config (servers, models, priorityGroups, costs)
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

- **P0 (scaffold)** — done: gat gateway, SDL dump, config loading, SQLite store, SPA shell.
- **P1 (proxy core)** — done: served model → first local backend. Lazy spawn +
  health-check + load-coalescing (`internal/proc`), OpenAI passthrough
  (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/rerank`,
  `/v1/models`), untracked non-inference bypass (`/upstream/<model>/…`), activity
  log, graceful process-group shutdown.

- **P2 (scheduler engine)** — done: priorityGroups + key resolution (default
  group synthesized), weighted-fairshare admission over per-backend slots
  (`maxConcurrent`), queue/reject saturation stages, and informative backoff
  (429 + `Retry-After` + `X-RateLimit-*` + JSON hint). `internal/sched`.
- **P3 (backend-list fall-through)** — done: a served model walks its ordered
  backend list — round-robin within a cost-equivalent `type`, ordered across
  types. Per-type `onSaturated` spill/fallThrough advances to the next backend
  (a backend that won't become ready also spills); queue waits; reject is
  terminal; an exhausted list returns a terminal 429.
- **P4 (residency)** — done: a spawn is admitted only if it fits its server's
  per-pool memory budget (`pools` − `reserve`, vector over named pools). When a
  cold backend doesn't fit, the eviction solver frees idle, non-pinned residents
  on the binding pool — ordered ttl-expired → unprotected → low `evictCost` →
  LRU — to make room (**evict-then-spill**: only if it can't free enough does the
  request spill). In-flight backends (held ref) and `persistent` models are never
  evicted; persistent models are preloaded at boot. `internal/proc`.

Next: P5 preemption (cooperative cancel of interruptible groups). See the roadmap.

The `model` field selects a served model from `corrallm.yaml`; the caller key
(`X-Corrallm-Key` or the bearer token) maps to a priorityGroup that governs
admission. Its first backend is spawned on demand and proxied to. Multi-backend
fall-through is P3.
