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

P0 (scaffold) complete: gat gateway, SDL dump, config loading, SQLite store, SPA
shell. Proxy core (P1) and scheduler (P2) are next — see the roadmap.
