# clieasy — a corrallm that spins up empty and is set up from the browser

How this plan works: see the Planning rules in `~/CLAUDE.md`. Status marks: ◻ todo · ◐ in
progress · ✅ done · ⏸ parked · ❓ blocked. Completed trees move to `plan/done.md`.

## Goal

`corrallm serve` on a machine with nothing configured must (a) boot, (b) serve a working web
UI, and (c) let an operator add servers, models and lanes from that UI — with all state under a
single home, defaulting to `~/.corrallm`.

Today none of the three hold. This plan is the gap list, measured rather than guessed.

**Status: implemented and verified 2026-07-31.** Sections A–D below are done; the
"Measured starting state" is kept as the before-picture the changes were aimed at.

## Measured starting state (2026-07-31, `bin/corrallm` @ dev)

Run in an empty scratch directory, `ADDR=:6599 corrallm serve`:

1. **Fatal at boot.** `inspect bench_probe_results: unable to open database file (14)`.
   `store.Open` (`internal/store/store.go:281`) opens `./home/var/corrallm.db` without creating
   the parent. `config.Save` MkdirAlls; `store.Open` does not.
2. With `home/var/` precreated it boots clean — `servers=0 models=0 groups=0`, admin token
   generated, `/health` and `/v1` up, `/api` correctly 401s. An **empty config is already a
   non-event**: `config.Load` returns `&Config{}` on ENOENT (`internal/config/config.go:876`).
3. **`/` → 404.** `webui.Handler` is `os.DirFS(./ui/dist)`. A binary that was downloaded rather
   than built in-tree has no `ui/dist`, so there is no UI in which to do the setup.
4. **Fresh install + valid admin token + `PUT /api/v1/config/models/demo/yaml` → 409**
   `cannot read the config at ./corrallm.yaml`. `requireManaged` (`internal/api/enroll.go:154`)
   reads the file and requires the literal `MANAGED CONFIG` marker. With no file at all, *every*
   write path is refused — config edits and agent enrollment alike. The guard that protects a
   hand-written config also locks out the case where there is nothing to protect. **This is the
   central blocker.**
5. `/v1/models` on an empty config returns `"data": null`, not `[]`.

## Decisions

- **One home, three derived paths.** `--home` resolves once (`CORRALLM_HOME` → `~/.corrallm`),
  and `--config` (`<home>/config.yml`) + `--db` (`<home>/var/corrallm.db`) derive from it when
  not set explicitly. `--tune-cache` already derives from the db dir — this extends that pattern
  rather than inventing one. Explicit flag/env always wins.
- **Bootstrap only the derived default.** First serve writes an empty managed config *only* when
  the path was derived and nothing exists. Never when `--config` was passed: stamping a
  `MANAGED CONFIG` header onto a path a human named is precisely what `requireManaged` exists to
  prevent.
- **Embed the UI as a fallback, keep `--web-root` authoritative.** `internal/webui`'s own
  docstring argues against `go:embed` so the UI can be swapped without rebuilding. That holds for
  slot deploys and fails for `curl | sh`. Both survive if the on-disk root wins when present and
  the embedded copy serves when it is not.
- **`ui/dist` stays gitignored.** The embed target is `ui/dist` with a committed `.gitkeep`
  (gitignore exception) so `go build` works on a clean tree; `make dist` builds the UI *before*
  the binary, so a release embeds the real thing. A dev `go build` embeds only the placeholder
  and the handler says so rather than 404ing.
- **The token path is disclosed to private-network callers only.** The operator needs it exactly
  when they cannot authenticate, so it cannot sit behind `/api`. But this daemon is
  internet-fronted, and the path carries the server's home directory. Public `/health` includes
  `tokenPath` only when `middleware.RealIP` resolves the caller to loopback/private space.

## Work (complete)

### ✅ A. Boot from nothing

- **A1** ✅ `store.Open` MkdirAlls the DB's parent (`internal/store/store.go`).
- **A2** ✅ `cmd/corrallm/paths.go`: `defaultHome()` → `~/.corrallm` (`CORRALLM_HOME`
  overrides); `derivePaths()` resolves config/db/token from it, flag > env > home. `serve` and
  `introspect` both use it, and `serve` logs `paths resolved` so the locations are never a guess.
- **A3** ✅ `--home ./home` pinned in ml-kit's `bin/run`, with the reason inline.
- **A4** ✅ `bootstrapConfig()` writes an empty managed config on first serve — derived paths
  only.

**risk, resolved**: A2 moves the admin token. Verified the pin holds: with the new binary run
from the ml-kit root, the log reads `loaded admin token path=home/admin.token` (not
*generated*) and the file's checksum is unchanged. Confirmed the danger was real —
`~/.corrallm/` contains `config.yml` and **no** `admin.token`, so an unpinned restart would have
minted a fresh one and silently invalidated every enrolled agent and browser session. Rejected
alternative: defaulting to `./home` when it happens to exist — an existence-probe default
silently picks the wrong root later.

### ✅ B. A UI to set it up in

- **B1** ✅ `ui/embed.go` (`//go:embed all:dist`) + tracked `ui/dist/.gitkeep`;
  `webui.Handler(webRoot, embedded)` prefers the on-disk root, falls back to embedded, then to a
  503 page naming `make dist`. Selection tests on **index.html presence**, not directory
  existence — an existing-but-unbuilt `ui/dist` is the common case on a machine that merely
  contains the repo.
- **B2** ✅ Public `/health` reports `tokenPath` to loopback callers; `Login.tsx` fetches and
  renders it, falling back to naming `~/.corrallm` when withheld.
- **B3** ✅ `config.tsx`: first-run panel at 0 models/0 servers pointing at the proxy-model path
  (no host required), and the stale "read-only" docstring corrected.

**risk, resolved**: `vite build` **empties `dist/`**, deleting the tracked placeholder — caught
when the first `pnpm build` removed it. A `postbuild` script in `ui/package.json` restores it, so
`pnpm build` no longer shows up in git as a deletion and the next clean checkout still compiles.

**security note on B2**: the locality check reads the address captured *before*
`middleware.RealIP` runs (`cmd/corrallm/localcaller.go`). RealIP overwrites `r.RemoteAddr` from
`X-Forwarded-For` / `X-Real-IP` whether or not the deployment sets them, so gating on the
post-RealIP value would let any remote caller claim to be `127.0.0.1` and read the server's home
directory off the fronted dashboard. Loopback only, not private ranges: the daemon serves a LAN,
and every other host on it is a client rather than an operator.

### ✅ C. Honest behaviour when empty

- **C1** ✅ `/v1/models` emits `[]`.
- **C2** ✅ `/install.sh` serves a self-explaining script that exits 1 when no agent binaries
  exist — still HTTP 200, because `curl -fsSL … | sh` prints nothing at all on a 4xx.
- **C3** ✅ Bench start resolves `--bench-bin` via `LookPath` **before** taking the exclusive
  lease. The old order granted the lease, evicted every resident model, and only then failed.

### ✅ D. Tests

- `cmd/corrallm/paths_test.go` — derivation, flag > env > home precedence, `configDerived` false
  for any named path, bootstrap writes a loadable managed file and never clobbers.
- `cmd/corrallm/localcaller_test.go` — spoofed `X-Forwarded-For` / `X-Real-IP` do not make a
  remote caller local; a missing capture withholds rather than guesses.
- `cmd/corrallm/spinup_test.go` — **the end-to-end claim**: builds the binary, runs it in an
  empty directory, asserts home creation, `/` 200 from the embedded UI, `tokenPath` disclosure,
  `[]` for no models, and that a `PUT` of the first model succeeds *and is served afterwards*.
- `internal/store/open_test.go` — parent created; `:memory:` creates no directory.
- `internal/webui/webui_test.go` — disk beats embedded, empty disk falls back, no-UI explains
  itself, SPA shell for unknown paths, cache headers.
- `internal/agentdist/dist_test.go` — no-binaries script; `hasBinaries` ignores non-artifacts.
- `ui/embed_test.go` — embedded FS is non-empty (states the compile-time contract).

**Amended test**: `TestBenchRunner_ReleasesLeaseWhenBinaryMissing` →
`TestBenchRunner_TakesNoLeaseWhenBinaryMissing`. Its premise no longer holds: with C3 the runner
takes no lease at all, which is strictly stronger than releasing one it took.

## Verification

- `go build ./...`, `go test ./...` — full suite green.
- `golangci-lint run ./...` — 42 issues, all pre-existing; none in any file added or changed here.
- `cd ui && pnpm build` — `tsc -b` typecheck + vite build clean.
- `npx eslint src/Login.tsx src/routes/config.tsx` — clean. Repo-wide `pnpm lint` fails on 4
  pre-existing `no-explicit-any` errors in `src/routes/model.tsx`, untouched by this work and
  already failing on `main`.
- Live `:8111` instance left running and unaffected throughout.

## ✅ E. Migrate the ml-kit deployment onto the root home

Section A made `~/.corrallm` the default and pinned ml-kit to `--home ./home` so the live token
would not move. This finishes the job: corrallm's state moves to the root home and the pin comes
back out. Done in the SAME restart that picks up sections A–D, so it costs one blip, not two.

### What moves

| State | From | To |
|---|---|---|
| Admin token | `ml-kit/home/admin.token` | `~/.corrallm/admin.token` |
| SQLite store (2.4 GB) | `ml-kit/local/corrallm.db` | `~/.corrallm/var/corrallm.db` |
| VRAM tune cache | `ml-kit/local/vram-profile.json` | `~/.corrallm/var/vram-profile.json` |
| Secrets | `ml-kit/.env` | `~/.corrallm/secret.properties` |
| Managed config | — | already at `~/.corrallm/config.yml` |

`llm-bench.yaml` was in this table and has been **taken back out** — see the correction below.

`ml-kit/home/` holds **only** `admin.token` — there are no `.properties` files in ml-kit at all,
so nothing else travels with the home. `~` and `ml-kit/local` are the same filesystem
(`/dev/nvme0n1p2`), so the 2.4 GB store moves by rename, not copy. `bin/run` is the only
consumer of any of these paths (checked: no cron, no systemd unit, no other script).

### Secrets stop being the launcher's problem

`serve` already calls `config.LoadInto(home, service)` before anything else, and
`secret.properties` is one of the layers it merges (`internal/config/properties.go:24`). The
parser takes `KEY=value` with `#` comments — which is exactly what `.env` already is, so the file
moves verbatim. `${GROQ_API_KEY}` still expands in the proxy headers
(`internal/config/proxytarget.go:66`) because the env is populated before the config is read.

`bin/run` loses its `set -a; . ./.env; set +a` block. corrallm loading its own secrets is better
than depending on a launcher to arrange an environment for it: it works the same way whether
started by `bin/run`, by hand, or by anything else.

### What stays in ml-kit, deliberately

- **Backend binaries** — `local/bin/llama.cpp/llama-server`, `local/bin/oidio`. Outputs of
  ml-kit's own build pipeline (`llama-rebuild`, `llama.cpp.pin`); the absolute paths in
  `config.yml` keep pointing at them. They are ml-kit's product, not corrallm's state.
- **`tmp/corrallm.{pid,log}`** — the launcher's bookkeeping. `bin/stop` reads the pidfile
  relative to the ml-kit root.
- **`--web-root "$CORRALLM_REPO/ui/dist"`** — kept, correcting an earlier call that the embed
  made it redundant. The embed is a fallback for a binary with no repo; this deployment
  deliberately serves the live directory so a UI change needs a browser reload rather than a Go
  rebuild (plan.md §8). Dropping the flag would trade that away for nothing.
- **`--config "$CORRALLM_CONFIG_PATH"`** — kept explicit even though it now derives to the same
  path. `bin/run` validates `$CORRALLM_CONFIG_PATH` before restarting, and the script's own
  comment says validating one config while serving another is the failure that check exists to
  prevent. Passing it keeps the two provably identical instead of coincidentally equal.

### What the launcher drops

`--home` (the pin, now unnecessary), `--db` (derives), `--bench-probes` (probes are embedded in
llm-bench — `probes/probes.go` calls the flag an override for user-defined probes, not a
requirement for the built-ins), and the `.env` sourcing.

### Order, and why

Stop → move → edit `bin/run` → rebuild+start. The stop must come FIRST: SQLite's open handle
follows the inode, so moving the store under a running daemon sends writes to a file nothing will
ever read again.

**Rollback** is symmetric and needs no backup, because nothing is rewritten — every step is a
rename within one filesystem: `mv` the five paths back, `git checkout bin/run` in ml-kit,
restart.

### Executed 2026-07-31 11:28–11:31, one restart covering A–E

Stopped cleanly (`bin/stop` — backends exited, VRAM released, ports free, no stray `-wal`/`-shm`),
moved the five paths, edited the launcher, rebuilt and started detached (pid 1169517).

Boot log confirms every derivation:

```
INFO properties loaded keys=3 home=/home/nthalk/.corrallm service=dev
INFO paths resolved home=/home/nthalk/.corrallm config=…/config.yml db=…/var/corrallm.db
INFO config loaded servers=2 models=12 groups=3
INFO loaded admin token path=/home/nthalk/.corrallm/admin.token
```

- **Token preserved** — `loaded`, not `generated`, and the md5 is byte-identical to the
  pre-migration snapshot. Both enrolled servers (`box1`, `carlsmacbookpro`) still present; the
  Mac agent's credential lives in the DB, which moved intact.
- **History preserved** — activity continues at id 113564; `nomic-embed-text` served 36 live
  raglit requests within the hour, so the 2.4 GB store carried over and local spawning works.
- **Secrets load natively** — `keys=3` from `secret.properties`, with nothing sourcing anything.
  Verified per provider rather than assumed: Groq returned content; OpenRouter authenticated and
  billed tokens. Cerebras returned **402**, which is an *authenticated* response — proved by
  hitting the upstream directly, where the real key gets 402 (out of credit) and a bogus key gets
  401. No 401/403 anywhere in the log. The 402 is an account-billing state, unrelated to the move.
- **Embedded probes work** — 20 resolve with `--bench-probes` gone.
- **Qwen cold-loaded and answered** in 7s (the tune cache moved with the DB, so the slot profile
  survived).

### Two corrections

**`--web-root` is kept.** I had said the embed made it redundant; wrong for this deployment, per
the reasoning above.

**`llm-bench.yaml` stays in ml-kit, tracked.** Moving it was a mistake, caught when `git status`
showed a deletion: the file is hand-written, has real commit history, and its own docstring
documents running it by hand from the ml-kit root. It is deployment *input*, like the
llama.cpp/oidio binaries that correctly stayed — not corrallm state. The migration table had
grouped it wrongly. Restored with `git checkout`, and `bin/run` points at `$PWD/llm-bench.yaml`
again.

The live process was started with `--bench-config` aimed at the home copy, so
`~/.corrallm/llm-bench.yaml` is now a **symlink** to the tracked file — one file, no possible
drift, and the running daemon's flag still resolves. The link is vestigial once the next restart
picks up the direct path; delete it then. No second restart was forced for this, since the flag
currently resolves correctly either way and bench runs are UI-triggered.

**Log truncation, noted not fixed**: `bin/run --detach` redirects with `>`, so each start discards
the previous `tmp/corrallm.log`. That cost the pre-restart history during this verification.
Appending (`>>`) would keep it; out of scope here, filed in the icebox.

## Out of scope

**Local model backends.** None of this gives a fresh user a `llama-server`. The managed config on
this box holds absolute spawn paths into `ml-kit/local/bin/`, and adding a spawned model in the
UI still means typing a path to a binary you built yourself. The honest out-of-box story is
**proxy models only** — Groq/OpenRouter/Cerebras need an API key and already work through the
existing YAML editor. Local serving stays a host-setup problem.

### Residuals after the switchover

Verified 2026-07-31 11:35: daemon serving from the new root, all four old paths gone,
`ml-kit/home/` (left empty once the token moved) removed — an empty `./home` is a trap, since
anything later run with `--home ./home` would silently mint a fresh token there.

Live argv matches what `bin/run` now produces on every flag except `--bench-config`, which points
at `~/.corrallm/llm-bench.yaml` (the symlink) rather than `$PWD/llm-bench.yaml`. Same file, so
nothing is wrong; the two converge textually at the next restart, when the symlink can be
deleted.

Uncommitted in both repos — nothing has been committed.

## ✅ F. Bench on a busy box without locking it

Every UI-started run took corrallm's calibration lease: every resident model
evicted, every other caller answered 429 for the duration. An outage with a
progress bar. Replaced with: wait out the backpressure, and measure the waiting
so it can be subtracted.

- **F1** ✅ `internal/proxy/timing.go` — every response carries
  `X-Corrallm-Queued-Ms`, `X-Corrallm-Load-Ms`, `X-Corrallm-Upstream-Ttfb-Ms`.
  Written from `ModifyResponse`, the last point a streaming response can still
  take headers; a client subtracts queue+load from its own total. No
  total-execution header — the body has not streamed yet, and subtraction works
  for streamed and buffered alike.
- **F2** ✅ Both counters accumulate across the spill walk. They were
  per-candidate assignments, so a request that queued, spilled and queued again
  reported only the last wait — wrong in the activity log, and worse in a header
  a client subtracts, where under-reported overhead inflates computed execution.
- **F3** ✅ `run.NewBenchClient` — `RetryBudget = -1` (unbounded on 429; agentkit
  caps 5xx separately) and an `OnRetry` hook accumulating the backpressure wait.
- **F4** ✅ `StageMetrics.QueuedMs` / `.ExecMs`; `tokPerSec` divides by ExecMs.
  `Retries429` stops being hardcoded 0.
- **F5** ✅ Exclusive is opt-in end to end (API field, dialog offering both).
  A shared run takes no lease and starts even while someone else holds one.
- **F6** ✅ Cold probes are **skipped** when shared, and the skip is recorded.

**Decisions (user's):** cold probes dropped entirely on a shared box rather than
run degraded — a cold pass without eviction rights may hit a resident model and
stand as evidence for a path it never tested, which is the bonsai failure. And
429 retry is truly unbounded, no wall-clock cap.

**Measured:** a cold local model reported `Load-Ms: 6705` against
`Ttfb-Ms: 375`. Wall time would have called that a seven-second model.

**Known imprecision:** the queue counter is process-global while the matrix runs
combos concurrently, so overlapping stages can each be charged the same wait.
That over-attributes queueing and under-reports execution — the direction that
never makes a probe look faster than it was.

**Not done:** the in-request `queued`/`load` the headers report are not yet
subtracted by the bench. Only the client-side 429 backoff is. Closing that needs
either an agentkit field carrying response headers out of `llm.Client`, or
correlating against corrallm's activity log by the run's caller key (`apiKeyEnv:
CORRALLM_BENCH_KEY`, already keyed per run). The activity route is self-contained
and also sums correctly across an agent loop's many calls.

## Icebox

- **`bin/run` truncates its log on every start** (`>` rather than `>>`), discarding the previous
  run's history exactly when a restart is being diagnosed. One-character fix in ml-kit, plus
  whatever rotation keeps it from growing without bound.
- **`ml-kit/corrallm.yaml`** is now doubly dead — it was already unread as the migration source,
  and the state it described has moved. Worth deleting from git with a pointer in its place.

## Optional extensions (not now)

- Embed the cross-compiled agent binaries so `/install/` works on a downloaded primary (~37 MB
  each — a large cost for a narrow gain).
- A guided first-run wizard rather than a raw YAML editor.
- Migrate an existing cwd-rooted install into `~/.corrallm` automatically.
