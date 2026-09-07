# corrallm — completed work

Archive of finished trees. Active work and conventions live in `plan/plan.md`;
deferred, opt-in next-steps live in `plan/icebox.md`. Nothing here is a to-do —
it is kept for the evidence, the corrections, and the traps that cost something
to find.

> **⚠ Two phase-number series collided.** Git history is authoritative: `P15`=bench,
> `P16`=free-tier aggregator, `P17`=pause, `P21`=provider credentials, `P22`=Qwen3.8
> cutover, `P23`=samplers, `P24`=local provider, `P25`=toolchain, `P26`=configdb,
> `P27`=versioned builds, `P28`=tickets, `P29`=tool pins. A second, dashboard-flavoured
> series was written into the roadmap under the SAME numbers (P15a/P15b, P17, P18, P19,
> P20, P21) and never appears in a commit. Those trees are archived below under
> descriptive names in **§ Dashboard & observability**, with their original mis-number
> noted. Do not reuse P15b–P21 from that series; the next free number is **P30**.

---

# Engine

## P0–P8 — proxy, scheduler, residency, preemption, cost, degrade, observability
- ✅ **P0 — Scaffold.** `fdf90b9`. Go module `github.com/iodesystems/corrallm`, Huma+gat wired,
  `dump-graphql`, React/Vite/codegen, `bin/gen`, YAML config loader + `.properties` layering,
  SQLite store, air+vite dev. *(UI via `--web-root`, not `go:embed`.)*
- ✅ **P1 — Proxy core.** `566b888`. Served model → single local backend: spawn `cmd` (own process
  group), health-check, load-coalescing, OpenAI passthrough (chat/completions, completions,
  embeddings, rerank, models). Untracked `/upstream/<model>/…` bypass. Activity log. Graceful
  SIGTERM shutdown reaps spawned children. `internal/proc`, `internal/proxy`.
- ✅ **P2 — Scheduler engine.** `13f15df`. priorityGroups + keys + synthesized default group.
  Weighted-fairshare admission (request-count share) over **per-backend slots** (`maxConcurrent`),
  queue + reject stages, informative backoff (429 + `Retry-After` + `X-RateLimit-*` + JSON).
  Caller key = `X-Corrallm-Key` or bearer token. `internal/sched`.
- ✅ **P3 — Backend list + fall-through.** `ebcff81`. Ordered walk of a model's backends:
  rr-within-`type`, ordered across types; per-type `onSaturated` spill/fallThrough advances,
  queue waits, reject is terminal, exhausted list → 429. `orderBackends()` + `Stage.Spill` wired.
  Quality carried but not yet a sort key (list order authoritative); per-quality routing landed in P7.
  *(preempt-vs-spill fork deferred to P5 — preempt has no implementation until then.)*
- ✅ **P4 — Residency.** `ec1bcfb`. Per-server pool-budget ledger gates spawns (fit = ∀pool
  want ≤ budget−used); eviction solver (evict-then-spill) frees idle non-pinned residents on the
  binding pool, ordered ttl-expired→unprotected→low evictCost→LRU, all-or-nothing → else
  ErrNoCapacity → spill. In-flight (ref-held) and `persistent` models exempt; persistent preloaded
  at boot. Size parsing + pool validation. *Not yet: affinity (prefer-warm over list order),
  `server.maxConcurrent` host cap, CapacityProbe, proactive ttl reaper, dynamic footprint — see §7.*
- ✅ **P5 — Preemption.** Cooperative, streaming-safe cancel of an in-flight slot held by a
  lower-weight, `interruptible` group when a higher group's stage allows `preempt`. The scheduler
  tracks per-slot cancel funcs; `Admit` returns a request context canceled (cause `ErrPreempted`)
  on preemption, which the proxy reverse-proxies under so the cancel aborts the upstream stream and
  frees the slot. The freed slot is handed to the preemptor first (preempt waiters jump fairshare).
  Victim = lowest-weight interruptible slot, strictly below the preemptor (equal/higher exempt),
  each victim targeted once. **Default ordering: preempt before spill** — with no eligible victim,
  the stage's `then`/spill (else queue/reject) applies. `sched.pickVictim`/`pickWaiter`.
- ✅ **P6 — Cost model.** `7bfdbad`/`84f4f70`/`d1091f1`/`e93bf2f`/`1e6ee19`/`c18a698`. The
  parsed-but-inert cost/limits config now behaves. New `internal/cost` package; scheduler gains a
  sliding-window budget ledger + configurable share currency (via `NewWithConfig`, injectable clock).
  - [x] **Local energy → $** — `(completion·genWh + prompt·procWh)/1000 × costPerKwh`. `cost.RequestUSD`.
  - [x] **Paid extraction → $** — `(prompt+completion) × costFactor` for `costFactor`-bearing types.
  - [x] **Swap/load $** — `swap.loadSeconds × loadWatts → kWh × costPerKwh`, charged to the request
        that triggered the cold load (`EnsureReady` reports `loaded`). *(Amortization across the
        coalesced batch deferred — trigger pays full; §7.)*
  - [x] **`limits` enforcement** — per-group + per-(group×type) TCO caps over a **sliding window**
        (`ParseRate` reads `$20/hr`/`600s/min`/`100/min`). Over-budget → spill if the stage allows,
        else back off (reason `over-budget`) with the time until the window frees; preemption N/A.
  - [x] **Share-currency option** — `requests` (default, in-flight count) | `dwell` | `cost`
        (per-group, decaying accumulator, 30s half-life). Mixed-currency queues fall back to
        request-count (coherent, starvation-free).
  - [x] **Meter + persist** — dwell + tokens + $ per request into the activity record (feeds P8);
        streaming + non-streaming usage capture, identity-decode for compressed upstreams.
- ✅ **P7 — Quality degradation.** `26c8d69`.
  - [x] `quality` is a sort/routing key: `orderBackends` walks best-quality-tier first
        (type-rr preserved within a tier; uniform quality = pre-P7 ordering, no regression).
  - [x] Per-group opt-in: `acceptDegrade` + `qualityFloor` gate accepted tiers
        (`PriorityGroup.AcceptsQuality`); a non-degrading group sees only the top tier and
        backs off per its stage instead of spilling onto a worse model.
  - [x] Request transform: per-backend `maxTokens` clamp applied to the outgoing body when a
        request degrades onto a capped backend. *(Context-window clamp needs tokenization — §7.)*
  - [x] Resolved: **variant-in-list** (one ordered list, quality-ranked), not a separate map.
- ✅ **P8 (MVP slice) — UI / observability.** `dc9ffd3`/`b7d8dcc`/`b7e1b92`.
  - [x] `recentActivity` GraphQL/REST op + `/activity` polling table (dwell/tokens/$).
  - [x] Residency read op (`Manager.Snapshot`: pool budget/used + resident backends) +
        `/usage` view (per-server pool-utilization bars + resident-model table).
  - [x] `usageRollup` op (per-model requests/tokens/dwell/$ over a window) + a 24h
        summary + per-model rollup table on the Usage page.
- ✅ **P8-beyond — observability + control plane.** Grew well past "polish": driven
  by the live llama-swap → corrallm cutover (§8), it's now the operator surface.
  - [x] **Lanes live view** — `Scheduler.Snapshot` → `lanes` op: groups
        (weight/currency/interruptible + live active/waiting) + backend health/util. `adf7483`
  - [x] **Live SSE events** — `internal/events` broker → `/api/v1/events`; proxy
        publishes activity/changed, UI invalidates on push (300ms coalesced throttle,
        15s fallback). *(SSE not WebSocket — see Status deviations.)* `45c93d0`/`d97dcec`
  - [x] **Overview control plane** — `overview` op: model + spawn-`cmd` defs (auth
        headers redacted, cmd behind a modal), lane defs, declared capacity; per-model
        `loadModel`/`unloadModel` mutations (`Manager.LoadModel`/`UnloadModel`; rails:
        spawnable-only, never pinned or in-flight) + Open-UI links. `f5ef5da`/`424b280`
  - [x] **Per-key usage** (`usageByKey`) + **time-series** (`usageSeries`): cost/
        requests/energy/time per caller key — bars + dependency-free SVG line charts. `6bf224d`/`ed80a2d`
  - [x] **Per-lane analytics** (`usageSeriesByGroup`, resolves key→group): stacked-area
        throughput + 429-rejections + avg queue-wait — priority-starvation watch. `9579fdd`/`15bda80`
  - [x] **Queue-depth sampler** — 5s background snapshot → `lane_samples` → `queueDepth`
        op: instantaneous per-lane waiting/active over time (pre-resolution pressure;
        48h retention, pruned). `e576065`
  - [x] **Backend logs + introspection** — per-process stdout/stderr ring (`logBuffer`)
        tee'd from the spawn, parsed `n_ctx`/`n_slots`, `modelLogs` op + live logs dialog. `5bca212`
  - [x] **Cutover hardening** (llama-swap parity, §8) — configurable `--health-timeout`
        `ca1b5b3`; readiness waits for `/health` 2xx so a cold load doesn't 503 `21698f2`;
        plain `/health`+`/healthz` liveness route `7e96bbf`; EWMA Retry-After + `maxWait`/
        `maxQueueDepth` queue bounds (the fork's good-citizen 429 contract) `14dd1bd`.
  - [x] **Auth** (`internal/auth`) — admin token in `<home>/admin.token` (auto-generated)
        gates all `/api/*` (ops + load/unload) via Bearer or cookie; `/v1`, `/upstream`,
        `/health` stay open. Dashboard login screen points to `home/admin.token`. `3e83001`
  - [x] **Retention / compaction** — `--activity-retention` (default 30d) prunes the activity
        log in the 5-min maintenance tick (it grew unbounded; only `lane_samples`/48h was pruned).
        SQLite reuses freed pages → file plateaus, no VACUUM. `7f12d48`

> **── MVP line ──** Above: P0–P6 + the P8 MVP slice = a usable, observable control
> plane. Below: post-MVP polish, reorderable.


---

# Modality & request surface

## P9 — audio + media API surface (OpenAI audio endpoints)

corrallm owns the **API surface**: `/v1/audio/transcriptions`, `/translations`,
`/speech`, `/v1/realtime`, the multipart request edge, byte-basis metering, and the
catalog/capability plumbing. The **backends** were scoped out to
[oidio](https://github.com/IodeSystems/oidio) (P12) — one Go binary on sherpa-onnx-go
doing STT + diarization + TTS + realtime, proxied like any other backend. The five
Python adapters that preceded it are retired.

Only `P9f` (comfort-fill on contention) is unbuilt, and it is parked in `plan/plan.md`
pending a transparency-tradeoff call.

- ☐ **P9 — Audio modality (OpenAI audio surface + parakeet STT backend).** Extend the
  request edge beyond JSON/text to OpenAI's audio API, with **parakeet**
  (`achetronic/parakeet` — Whisper-compatible ASR, NVIDIA Parakeet-TDT 0.6B ONNX, **STT-only**,
  a spawnable Go binary that binds a port → ordinary `cmd` backend) as the first concrete STT
  backend. **Audio backends are ordinary backends** — they spawn, health-check, draw pool
  budget, take fairshare slots, fall through, preempt, and meter exactly like text backends
  (P1–P7 reused unchanged). What's new is only the **request shape** (multipart-in,
  binary/SSE-out) and the **cost basis** (audio replies carry no token `usage`). Modality is
  **inferred from backend `type`** (an audio cost class), not a new config field. Split into
  shippable sub-units:
  - ✅ **P9a — Multipart request edge + STT routing.** Done (not yet committed). `resolveRequest`
    forks on Content-Type: JSON → existing `modelFromBody`/`streamFromBody`; `multipart/*` →
    `modelFromMultipart` reads the `model`+`stream` form fields from the buffered body (skipping
    the file part — `NextPart` streams past it) and the whole body replays to upstream intact.
    `/v1/audio/transcriptions` + `/v1/audio/translations` mounted through the same scheduler →
    residency → ordered-walk → reverse-proxy pipeline; audio routes get a 64 MiB body cap (vs
    32 MiB; parakeet caps audio at 25 MiB). SSE transcription deltas ride the existing streaming
    passthrough (`statusCapture`). No `audio` cost-class needed yet — an unpriced `type` already
    meters $0 via `cost.RequestUSD` (real coeffs land in P9c). Tests: `TestAudioTranscriptionMultipart`
    (e2e multipart extract + replay + activity log), `TestModelFromMultipart` (field/stream/empty-boundary),
    `TestAudioTranscriptionStreaming` (SSE `transcript.text.delta` passthrough: in-order, flushed not
    buffered, per-token `logprob` confidence preserved — the first streaming test for any route).
    `go build`/`vet`/`test -race` green, gofmt clean.
    *Known gap (→ P9c): metering is token-based, so audio meters $0 until the byte-basis cost path
    lands. The 130s request timeout is unchanged — fine for ≤25 MiB; revisit if long-audio jobs appear.*
  - ✅ **P9b — TTS endpoint (`/v1/audio/speech`).** Done (not yet committed). **Backend decided:
    Kokoro** (`remsky/Kokoro-FastAPI` v0.5.0, Apache-2.0, CPU). `/v1/audio/speech` mounted through
    the same pipeline; JSON-in (model resolves via the existing JSON path), **binary-audio-out**
    streamed through untouched. Metering forks on a `tts` route flag: TTS is **costed by OUTPUT
    bytes** (`statusCapture.written` — the synthesized audio; its JSON input is tiny), vs STT's
    input bytes — the binary reply is never parsed as JSON `usage`. A `tts` type declaring audio
    coeffs auto-flags `modality: audio` (reuses P9d). Test: `TestAudioSpeechTTS` (binary
    passthrough byte-for-byte incl. a `0x00`, output-byte metering, 0 tokens). `go test -race`
    green. Installed Kokoro under ml-kit `local/` (uv venv + torch CPU + 327MB weights + 67
    voices); smoke-tested healthy on :8880, and full audio loop proven (text→Kokoro→mp3→parakeet
    STT round-trips) + full stack (curl→corrallm→cold-spawned kokoro, metered `audio_bytes`=mp3 size).
  - ✅ **P9c — Audio cost model (file-bytes basis).** Done (not yet committed). Audio replies carry
    no token `usage`, so `cost.AudioRequestUSD(typ, bytes)` costs by **byte size**: a local type
    bills `audioWhPerMiB` (→ kWh × `costPerKwh`), a paid type bills `audioUSDPerMiB` directly; an
    unpriced type stays $0. New `commandCosts` audio coeffs (`audioWhPerMiB`/`audioUSDPerMiB`).
    `handleInference` forks metering on the `audio` route flag — STT bills `len(body)` (uploaded
    audio + small multipart overhead) instead of token usage. New `activity.audio_bytes` column
    (schema + forward-only migration, like `queued_ms`); `Activity.AudioBytes` persisted +
    threaded through `p.log`. Tests: `cost.TestAudioRequestUSD` (local/paid/unpriced/zero) +
    `proxy.TestAudioTranscriptionMetering` (0 tokens, `audio_bytes` = body len, byte-based $).
    `go build`/`vet`/`test -race` green, gofmt clean.
    *Scope notes: TTS char/output-byte costing wires up when P9b lands (the byte path already
    covers TTS output bytes); true-duration costing deferred (would parse `verbose_json`/SRT or add
    ffprobe — §7 Optional extensions). Rollup/usage SUMs of `audio_bytes` are P9d's UI concern.*
  - ✅ **P9d — Catalog + observability.** Done (not yet committed). **Modality decided this
    session: inferred from cost class** (a backend `type` declaring audio coeffs is audio; a model
    is audio iff any backend uses one — `cost.IsAudioType`). `/v1/models` (`handleModels`) and the
    `overview` op (`ModelDef.Modality`) now carry `text|audio`; `recentActivity` exposes
    `audioBytes`. UI: Overview model card shows an `audio` badge; the Activity table adds an Audio
    (bytes) column and renders prompt/completion as `—` for audio rows (tokens N/A). `bin/gen`
    re-run → SDL snapshot (`ui/gen/schema.graphql`) updated with `audioBytes: Long!` + `modality:
    String!`; codegen/eslint/tsc/`vite build` clean. Tests: `cost.TestIsAudioType`-via-metering,
    `api.TestOverviewAudioModality` + `TestOverview` (text), `TestRecentActivity` (audioBytes
    carried). `go test -race ./...` green, gofmt clean.
    *Deferred to opportunistic polish: per-`audio_bytes` SUMs in the rollup/usage ops
    (`usageRollup`/`usageByKey`/`usageSeries`) — activity rows + catalog cover the P9d goal.*
    *SUPERSEDED (this session): the coarse `modality: text|audio` string is replaced by
    per-model INPUT `modalities` — a nested map keyed text|image|audio, each with optional
    client metadata (image `maxResolution`/`formats`, text `maxTokens`). Config: `Model.Modalities
    map[string]ModalitySpec` + `EffectiveModalities(audioDefault)` fallback (audio cost class →
    {audio}, else {text}); Validate rejects unknown keys. `/v1/models` emits a JSON object;
    GraphQL `ModelDef.Modalities []ModalityView` (list — GraphQL has no map). UI: `modality`
    query field dropped, `modalities{…}` selected + rendered as chips on the model page. Drove
    it: llama.cpp auto-loads the mmproj sibling from a `-hf` vision repo (no `--mmproj`), so
    `image` is CONFIG-DECLARED, not backend-detected. ~~**bonsai vision verified end-to-end**
    (/props modalities.vision=true + red-circle test → "Circle, Red")~~ — **RETRACTED
    (2026-07-18): that check was run against a WARM bonsai.** Cold (first request after a load)
    it silently drops the image and answers as if none were attached; warm it is correct, and a
    fresh second image proves the warm pass is genuine perception, not prompt-cache reuse.
    `/props` still says `vision: true` and the mmproj still loads, so no warm probe can catch
    this. Root cause not yet found (corrallm readiness gate vs. llama.cpp mmproj init), and the
    scope on Qwen/gemma is UNKNOWN — both were only ever probed warm. See **P15** (cold-path
    capability probing exists because of this bug). **All 3 chat models
    ship mmproj** (HF-API repo check: Qwen, gemma, bonsai each have mmproj-*.gguf) → declared
    text+image in ml-kit/corrallm.yaml; the whole `chat` lane [Qwen, gemma] is uniformly vision,
    so a degrade keeps image. Qwen/gemma repo-verified but runtime /props NOT re-checked (box too
    busy to load 29 GB Qwen). gemma-4-12b is "omni" — audio-in left undeclared (separate path,
    unverified). Lane modalities = PRIMARY member's (matches capability derivation). Backend build
    + full `go test` + tsc clean; `make gen` lint gate already red on main (pre-existing `any` debt
    in model.tsx, unrelated). Uncommitted.*
  - ✅ **P9e — Realtime WebSocket passthrough (live/conversational transcription).** **Done +
    validated end-to-end.** New `handleRealtime` (`/v1/realtime`): model from `?model=` query,
    ordered-backend admission holding **one slot for the session**, then `proxyWebSocket` — a manual
    hijack + bidirectional `io.Copy` (the request body is NOT buffered). Preemption teardown is
    explicit: a `<-ctx.Done()` goroutine closes both conns when the slot is reclaimed (`ErrPreempted`
    → session logged 499) — the flagged "does cancel fire on a hijacked conn" risk is **verified by
    test**. Metered by client→backend bytes (audio in) → `AudioRequestUSD`; one activity row on close
    (dwell = session). **Metering correctness fix:** wait for *both* copy directions before reading
    the byte count (reading after one side closed raced the counter → undercount/0). Tests:
    `TestRealtimeWebSocketPassthrough` (raw-conn 101 upgrade + bidirectional echo, no ws client dep) +
    `TestRealtimePreemptAbortsSession`. `go test -race` green.
    **Backend: Speaches** (speaches-ai/speaches v0.8.3, CPU faster-whisper int8) installed under
    ml-kit `local/`, wired into the config (served name = model id; `LOOPBACK_HOST_URL` required, model
    pulled once via `POST /v1/models/…`). **Full stack validated:** ws client → corrallm → Speaches →
    "And so my fellow Americans." + metered `audio_bytes`=501712. *(Speaches realtime VAD over-segments
    a blasted synthetic stream + a transient "item already exists" — app-layer, cleaner at real-time
    mic pace.)* **Idle/max-session reaper ✅** — a background ticker watches live byte counts
    (`countingWriter`, both directions) and closes a session silent past `--realtime-idle-timeout`
    (default **5m**, `CORRALLM_REALTIME_IDLE_TIMEOUT`) or longer than `--realtime-max-session`
    (default off); a reaped session frees its slot and logs **408** with `idle timeout`/`max session`.
    `SetRealtimeTimeouts`; test `TestRealtimeIdleReaper`. **P9e fully done.**
    <!-- original scope retained below -->
    A **separate request edge** from P9a's file model: live mic transcription (OpenAI Realtime,
    `wss://…/v1/realtime?model=…`) streams audio *in* continuously, so it **must not buffer the
    request body** the way `handleInference` does (`proxy.go:97`). New `handleRealtime` that
    **upgrades** the connection and lets the reverse proxy raw-copy bytes both ways (Go 1.26's
    `httputil.ReverseProxy` already handles `Connection: Upgrade` — `newReverseProxy` works once
    we skip the body read). **corrallm stays a transparent ws byte-pipe** with a clear division of
    responsibility (confirmed by the user): **device/mic capture is the CLIENT's job** (corrallm
    never manages live audio devices), and **VAD / overlap-window / commit-stitch (LocalAgreement)
    is the BACKEND's job** (Speaches — the chosen backend — / sherpa-onnx / etc.) — corrallm does
    **neither**; it doesn't decode audio or tokenize (§7). It only upgrades, routes, schedules,
    meters, and tears down. **Decided (§7):** standardize the wire on the **OpenAI Realtime
    transcription schema** and ship **Speaches** as the native-passthrough default (CPU, MIT) — true
    byte-pipe; custom-protocol backends get a thin adapter instead. (Installed batch Parakeet-TDT
    can't stream, so realtime is a *different* backend.) What's new vs P9a:
    - **Model resolution from the query string** (`?model=`) — a third source after JSON body
      field (chat) and multipart form field (P9a). Generalize `resolveRequest` accordingly.
    - **Long-lived slot lifecycle** — a session holds one fairshare slot for its whole duration;
      **`dwell` share currency** (P6) is the honest cost, not request-count. The 130s request
      timeout must NOT apply — replace with an **idle/max-session timeout + reaper**. Preemption
      reuses P5: `Admit`'s `reqCtx` cancel (cause `ErrPreempted`) tears down the upgraded conn and
      frees the slot — streaming-safe cancel already proven for SSE; verify it fires on a hijacked
      conn. Metering: no token usage in ws frames → meter **dwell + $** (session seconds/bytes,
      P9c byte-basis); persist as one activity row at close.
    - Mount `/v1/realtime` as the **scheduled** realtime path (distinct from the untracked
      `/upstream/*` bypass, which is unscheduled).
    - *Requires a realtime/ws ASR backend — parakeet is file-only, so like P9b's TTS this ships the
      passthrough and the concrete backend is a separate decision (§7).*
    - *DoD: 101 Switching-Protocols upgrade + bidirectional byte passthrough test (raw-conn echo
      upstream, no ws client dep), slot held-for-session then released on close, preempt-aborts-session
      test. `go build`/`vet`/`test -race` green.*
  - ✅ **P9g — Diarized batch STT (speaker-labeled transcript).** Done + validated. The offline
    half of the realtime/batch split: realtime-stt streams partials but has **no speakers** (stable
    IDs need the whole utterance); `diarize` is batch-only and returns them. **Service**
    (`examples/sherpa-diarize/diarize.py`, deployed to ml-kit `local/src/sherpa-diarize/`): aiohttp,
    OpenAI-shaped `POST /v1/audio/transcriptions` — ffmpeg-decode any container → 16k mono f32 →
    sherpa-onnx **OfflineSpeakerDiarization** (pyannote-segmentation-3-0 + wespeaker_en CAM++ +
    FastClustering) + **offline zipformer** (gigaspeech int8) ASR → align tokens to speaker segments by
    timestamp → `{text, segments:[{speaker,start,end,text}], num_speakers, duration}`. Plain OpenAI
    clients read `.text`; the console's BatchStt renders the speaker-labeled segments (per-speaker color
    chips + timestamps). Wired in ml-kit `corrallm.yaml` as model `diarize` (type `stt`, `modes:[batch]`,
    proxy :5805, sticky 300s). **corrallm code unchanged** beyond the UI — it's just another stt backend.
    *Validation:* full proxy path = cold spawn → diarize → metered (status 200, `audio_bytes`, byte-basis
    cost). Diarization QUALITY: pyannote **segmentation** is accurate (clean turn boundaries on
    silence-gapped audio); **clustering** separates **real** voices well (thr=0.6 ⇒ correct count on a
    4-speaker reference) but **not synthetic TTS** (voxceleb embeddings don't separate kokoro timbres —
    documented in the README; validate with real recordings or pass `NUM_SPEAKERS`). Default
    `CLUSTER_THRESHOLD=0.6` (real-audio accurate), env-tunable. *Side fix (P10b metering):
    `statusCapture.WriteHeader` now skips interim **1xx** — large uploads sending `Expect: 100-continue`
    were logging status **100** instead of the final 200.*

  **P9 reuse note:** scheduler/residency/preemption/fairshare/limits need **no changes** — an
  audio backend is a `cmd`+`proxy` entry with a `type`, slots, and pool `ramUsage` like any
  other. The blast radius is `internal/proxy` (routing + multipart fork + binary metering),
  `internal/cost` (byte-basis path), `internal/store` (one metered column), and the catalog/UI.


## P12 — audio consolidation cleanup (5 adapters → 1 oidio)

- ✅ **P12 audio consolidation cleanup** — collapse ✅ (5 adapters → 1 oidio, verified; Python examples
  deleted). `Model.Modes` **dropped** — batch-vs-realtime is now encoded in the capability (cost type
  `realtime` → `audio.realtime`); the console dispatches by capability (no modes gate/toggle),
  `/v1/capabilities` routes each endpoint by capability (no mode-filter), schema regenerated. Verified:
  realtime-stt=audio.realtime, stt/stt-diarize=audio.stt, all 4 models serve.

## P13 — chat PDF auto-conversion

- ✅ **P13 chat PDF auto-conversion** — a text model can't read an attached PDF, so the proxy
  intercepts `/v1/chat/completions`, finds PDF content parts (OpenAI `file`/`input_file`/data-URL
  shapes), extracts text via `pdftotext -layout`, and injects it as a text part. On by default
  (`--convert-pdfs`, `--pdf-max-chars`); text-based PDFs only (scanned → OCR is a follow-up).
  Verified end-to-end (Qwen answered from a PDF's content); tests cover detection + extraction +
  truncation + no-op passthrough.

## P10 — request observability & honest errors

- ◐ **P10 — Request observability & honest errors.** Driven by a production incident: qwen requests
  logging **502s**. Diagnosed (DB + proxy log): a long request (big prompt / image data on the
  27B/220k-ctx model) outruns a **~120 s timeout *upstream of corrallm*** (the `llm.iodesystems.com`
  front proxy and/or the client), which drops the connection — `http: proxy error: context canceled`,
  dwell ≈120 s, 0 tokens. llama-server itself is healthy. corrallm was **mislabeling** the
  client/upstream cancel as a backend 502, and its own fixed **130 s** request cap was a latent
  second guillotine.
  - ✅ **P10a — Honest status + error reason + configurable timeout.** Done (not yet committed). The
    reverse proxy now has an `ErrorHandler` that captures the failure and maps it: connection
    canceled (client/front-proxy gave up) → **499**, corrallm's own deadline → **504**, genuine
    backend dial/transport error stays **502**; preemption stays 499. The reason string is captured
    into a new `activity.error` column (schema + forward-only migration), exposed on `recentActivity`,
    and shown as a **tooltip on the status chip** in the Activity table. The hard 130 s cap is gone —
    new `--request-timeout` (`CORRALLM_REQUEST_TIMEOUT`, default **0 = no corrallm deadline**, defer
    to client + backend; `SetRequestTimeout`). Tests: `TestClientCancelLogged499` (the exact 502→499
    repro) + `TestRequestTimeout504`. `go test -race ./...` green; UI tsc/build clean.
    *Does NOT fix the failures themselves — the real ~120 s cap is upstream (raise the front-proxy
    `proxy_read_timeout` / client timeout). Streaming (`stream:true`) also masks it: chunks reset the
    read timeout. corrallm's job here is honest reporting + not being a second cap.*
  - ✅ **P10b — Per-request payload + timing capture.** Done (not yet committed). New activity columns
    `req_body`/`resp_body`/`ttfb_ms` (schema + forward-only migrations). `p.log` refactored to take a
    `store.Activity` (was 12+ positional args). Request payload captured once on every exit path;
    **STT multipart uploads + TTS binary output are summarized to `<content-type, N bytes>`, never
    stored raw**; text capped at 4 KiB. TTFB = first-response-byte time (`statusCapture.firstWrite`).
    `--capture-payloads` / `SetCapturePayloads` toggle (default on; payloads are user data, admin-gated,
    pruned with `--activity-retention` → "discard on compaction"). `id`+`ttfbMs` exposed on the lean
    `recentActivity` list; payloads only via `ActivityByID`. Tests: `TestPayloadCapture` (capture +
    disable) + `TestPayloadCaptureBinaryAudio` (summarized, no raw bytes) + store round-trip.
  - ✅ **P10c — Activity detail modal (UI).** Done (not yet committed). New `activityDetail(id)` op
    (`/api/v1/activity/detail`) returns the full row + payloads on demand (list stays lean). UI: rows
    are clickable → MUI `Dialog` showing served/backend/path, **error + timing (dwell/ttfb/queued/$)**,
    and **request + response payloads** (monospace, scrollable). SDL regenerated; tsc/eslint/`vite
    build` clean.


## P11 — capabilities/discovery + model console

- ◐ **P11 — Capabilities/discovery + model detail.** A self-describing surface so an LLM/client can
  build a compatible client, and so UI-less models (parakeet/kokoro/speaches have no web UI) are
  still inspectable from the dashboard.
  - ✅ **P11a — `/v1/capabilities` manifest.** Done. Public (unauthenticated, like `/v1/models`),
    synthesized from config, **never exposes API keys**. Returns: the OpenAI endpoint surface with a
    runnable example each (curl, + the realtime ws session flow), models grouped by **capability**
    (inferred from cost class — `capabilityForType`: chat/embeddings/audio.stt/audio.tts/rerank), and
    the fairshare **lanes** (name/weight/currency/interruptible — policy only). Test
    `TestCapabilitiesManifest` (grouping, endpoint coverage, lanes, **key-leak assertion**).
  - ✅ **P11b — Disabled "Open UI" for UI-less models.** Done. The proc manager probes the backend
    root once when ready (spawned backends only; async, never gates readiness) and caches `hasUI`
    (yes/no/unknown). Exposed on the residency `ResidentModel`/`ResidentModelView`; the Overview
    Open-UI button is disabled with a "no web UI" tooltip when `hasUi === "no"`. Test `TestProbeUI`.
  - ✅ **P11c — Model console.** Done. New `/model?name=` route (`ui/routes/model.tsx`) reached from a
    "Console" button on each Overview model card. Tabs: **Info** (modality/capability/state chips,
    backends table + spawn cmd, the `/v1/capabilities` examples for this model, Open-native-UI or a
    "no web UI" chip), **Test** (the P11d playgrounds by capability), **Logs** (`modelLogs`), **Usage**
    (`usageRollup` 24h). Makes UI-less models fully inspectable. tsc/eslint/build clean.
    *(Deploy note: queries the new `hasUi` field, so the prod dashboard needs a `bin/run` rebuild — a
    new-field UI change isn't binary-compatible with the old gateway.)*
  - ☐ **P11d — In-dashboard test playgrounds** (user's vision). Since not all backends ship a native
    UI, let the dashboard *drive* each model by capability, using browser Web APIs:
    - **chat** — a chat playground; **MUST use `flex-direction: column-reverse`** for the message
      list (user preference — auto-pins newest, no scroll management). Streams via `stream:true`.
    - **audio (STT↔TTS loop)** — mic capture (MediaRecorder / Web Audio) → `/v1/audio/transcriptions`
      (or `/v1/realtime` ws) → optionally pipe the text → `/v1/audio/speech` → speaker playback. A
      full voice loop in the browser.
    - **image/vision** — image upload → chat with image content parts (for multimodal models).
    Decided: **consolidated model console** (tabs Info+Examples · Logs · Usage · Test); build all
    three playgrounds **chat → voice → image**; playground `/v1` calls default to the **default lane**
    with a lane picker.
    - ✅ **chat** — streaming chat playground in the Test tab; **flex column-reverse** message list
      (newest pins to bottom), SSE delta parsing, optional lane-key field. Built + typechecked (not
      yet live-smoke-tested against a chat backend).
    - ✅ **voice (STT↔TTS loop)** — Test tab for audio models. STT: mic capture
      (`getUserMedia`+`MediaRecorder`) → `/v1/audio/transcriptions` → transcript, then **"speak it
      back"** via a chosen TTS model → `/v1/audio/speech` → `Audio()` playback (a full browser voice
      loop). TTS: text → speak. tsc/eslint/build clean.
    - ✅ **image/vision** — image attach (🖼) in the chat playground: file → base64 data URL → the
      user turn is sent as OpenAI multimodal content-parts (`text` + `image_url`) for vision models;
      thumbnails render inline. tsc/eslint/build clean.
    - ✅ **STT/TTS clarified + batch/realtime + dispatch fix** — `config.Capability` keeps STT vs TTS
      DISTINCT (never lumped "audio"); fed to `/v1/models` (new `capability`), the `overview` op, and
      UI badges (`capLabel`: stt/tts/embed). The console dispatches the playground from the model's own
      `capability` (not the async `/v1/capabilities`), fixing a race that briefly showed a chat box for
      parakeet (backend verified fine: webm→200). STT playground gains a **Batch (record→upload) /
      Realtime (live ws PCM16@24k)** toggle + a secure-context (https) mic guard; the upload part is
      named by the real MediaRecorder mime so the backend demuxes across browsers.
  - ✅ **P11e — Replay an activity into the console.** Done. The activity detail modal (P10c) gains a
    **"Replay in console"** (chat paths) / "Open in console" button → navigates to
    `/model?name=<served>&replay=<id>`. The console opens the Test tab and the chat playground fetches
    `activityDetail(id)`, parses the captured `reqBody.messages` (incl. multimodal content-parts via
    `extractText`), loads prior turns as history, and drops the last user turn in the input to re-run/
    tweak. Audio rows only stored a size summary, so they just open the console. tsc/eslint/build clean.


---

# Config, routing & lifecycle

## P14 — lanes + single-path models (schema v2)

- ✅ **P14 — Lanes + single-path models (schema v2).** A model is exactly ONE serving path
  (`cmd` spawned local, or standalone `proxy` remote/surface — the same weights served two ways
  = two named models); fallback across models is a first-class **`lanes:`** section: named,
  ordered member lists (`members: [name | {model, sticky}]`, per-member sticky override for
  "loaded on the lane's behalf → unload sooner"). Requesting a lane name allows substitution
  across members (quality-tiered walk, type-rr, acceptDegrade/qualityFloor/preferResident all
  unchanged); requesting a model name pins exactly that model. Replaces the P3/P7
  "ordered backend list" — `config.Backend` is gone (`Model` flattened; `Slots`/`ProxyTarget`
  moved onto it), `ResolveServed` → `[]Candidate`, proc.Manager keyed by plain model name
  (no more `served#idx`), reservations target the first candidate, `/v1/models` lists lanes
  (`kind: lane`, `members`) alongside models, overview gained `LaneDef`s, and the old hidden
  degrade row (a proxy row pointing at another model's port — lifecycle-blind: it forwarded
  to a dead port unless the target happened to be warm) is impossible by construction.
  Priority-group API/UI surfaces renamed lanes→groups (op + dashboard) to free the term;
  persisted `lane_samples` schema untouched. Motivating case: ml-kit's `chat` lane
  (Qwen 27B → gemma-4-12b) replacing a misleading `Qwen3-6-27B-MPT`-serves-gemma proxy row.


## P16 — free-tier aggregator (quota-aware remote routing)

Shipped as `P16a`–`P16f` (2026-07-21): base-path prefixing and upstream-id rewrite for
remotes, a budget ledger read from rate-limit headers, self-cap/staleness/quota-aware
selection plus a console card, counter-mode tracking for header-less providers, privacy
tiering, roster refresh, and durable falloff counters. Design + provider facts in
**`plan/p16-free-aggregator.md`**.

- ☐ **P16 — Free-tier aggregator (quota-aware remote routing).** Pool many providers'
  independent free daily/minute quotas into one serving budget: register free OpenAI-compatible
  endpoints (Groq, Cerebras, OpenRouter, …) as proxy backends, track each provider's remaining
  quota (from response rate-limit headers where present, else a local counter), and swap across
  them before exhaustion. Extends P14 lanes from react-on-error failover to avoid-before-exhaust
  selection; local models stay the floor (remote free is never the sole path). **Full design +
  provider facts + phased build order in [plan/p16-free-aggregator.md](p16-free-aggregator.md).**


## P17 — pause (models + extensions)

- ✅ **P17 pause (models + extensions)** — take something out of service: unload it and never
  load it again (request, lane fall-through, explicit load, or boot preload) until resumed,
  with an optional resume datetime. **A pause keys on the PROCESS (ProcKey), not the name** —
  so an extension's models pause and resume as one unit, matching the blast radius unload
  always had. Enforced at `Manager.EnsureReady` (the one door every load comes through) plus a
  `filterByPaused` pre-filter so a lane skips a paused member instead of queueing for an
  admission slot on it. Refusal is a PERMANENT `CapacityError` → 503, not 429: a pause is a
  decision, not congestion, so no Retry-After would be honest. Pause overrides `persistent`
  (a pin is a scheduling preference; letting it veto would make pausing pinned oidio a no-op).
  Durable in `model_pause` keyed by process, restored before Preload. Timed pauses expire
  lazily on read, plus a 30s sweeper so a pinned process — which nothing requests by name —
  actually comes back and is re-warmed. Ops `pauseModel`/`unpauseModel`/`pauseExtension`/
  `unpauseExtension`; pause + scope on `ModelDef`, live state + pause on `ExtensionDef`;
  `/v1/models` reports `state: "paused"`. UI: a new **Extensions panel** (the process behind a
  model group had no surface at all before) with Load/Unload/Pause/Resume, a `datetime-local`
  pause dialog that states the blast radius before the click, and a confirm on unloading an
  extension-hosted model — which silently took its siblings down for as long as that button
  has existed.
  **Two pre-existing bugs surfaced by it and fixed here:** (1) `waitHealthy` polled for the
  full `--health-timeout` (600s in prod) even after the process it was waiting for had exited
  — a crash-on-start held its pools and read as `loading` for ten minutes; it now aborts on
  the handle's `Done()`. (2) a resume that follows a pause closely races the eviction it just
  triggered (async SIGTERM + 15s grace), so the respawn can lose a fixed resource — observed
  live as `Unit oidio.scope was already loaded` from the config's `systemd-run --unit=oidio`;
  `repin` now retries with backoff past `evictGrace`. Only pause can reach this for a pinned
  process, since ordinary unload refuses a pin.
  **Teardown is now tracked** (`Manager.stopping`, set by `evictLocked`, cleared once the group
  is confirmed gone): eviction only REQUESTS an exit and drops the entry from `procs` at once,
  so nothing previously remembered a process that was still alive. The two paths differ on
  purpose — `EnsureReady` WAITS the teardown out (a request has nobody to ask, and failing on a
  200ms window is worse than waiting), while explicit `LoadModel`/`LoadExtension` REFUSE, along
  with a load issued while one is already loading or draining: coalescing is right for a
  request and wrong for an operator action, where reporting "loaded" for a spawn someone else
  started hides which click did the work.

## ✅ P24 — local models are a provider (2026-08-16)

Top-level `models:` is retired. Local models live under
`providers.local.models`, served as `local-<id>` like every remote — local
stops being the special case. **Bare precedence** (default on, 100) keeps old
callers working: an unprefixed name nothing else claims resolves to the
highest-precedence provider offering it, yielding the canonical prefixed name so
residency, metrics and quota stay on one identity. Tried LAST, after lanes,
exact models, aliases, discovered ids and globs.

Also in this pass: `discover:` → `directory:` (a browsing default that enrols
nothing) and `extensions.free.virtual` (a pool across its member providers) —
see `plan/p21-provider-credentials.md` P21h–j.

**Deployed 2026-08-16** (`412508a`). Live config migrated; six models moved, one
lane reference renamed, nothing else. Verified on production: both `Qwen3.8-27B`
and `local-Qwen3.8-27B` serve.

**bare names** are indexed for BOTH provider kinds now (`buildBareIndex`), so
claims compete on precedence. Local defaults ON at 100; a remote provider is OFF
unless it sets `barePrecedence`, because somebody else's endpoint answering an
unprefixed name would route a request off this box on a coincidence. A value
above 100 takes a name away from a local model of the same id.

**risks** activity history and the VRAM tune cache key on served name, so both
start fresh under the new ids — old rows remain under the old names and will not
be joined to the new ones.
**next** nothing required.
**also shipped** creating a local model from the Providers page (`Add model`),
reusing ModelForm so the two editors cannot drift. `upsertModel` takes an
optional `provider` for creation.
**pattern worth remembering** the fold puts provider-owned models into
`c.Models`, and THREE separate handlers wrote to that copy instead of the
provider block — the config writer, upsert, and delete. Delete was the worst: it
reported success and changed nothing, so the model returned on the next load.
`internal/api/localprovider_test.go` covers create/edit/delete in one
round-trip, each followed by a real `config.Load` of the written file. Any new
handler that mutates a model must ask where the model was AUTHORED.


## ✅ P26 — config lives in SQLite (2026-08-18)

Design doc: **`plan/p26-config-sqlite.md`**. All five phases shipped and LIVE:
the production daemon boots from the database and `~/.corrallm/config.yml` is
retired to `config.yml.imported-20260818-162738`.

`config.yml` was already machine-owned — its own header said hand edits are not
preserved, because the daemon rewrites it on every change. A struct-marshalled
file a program rewrites is a database with a bad storage engine, and it failed
twice this month in ways a database cannot: it silently DELETED the `tools:`
block (added while a daemon with no struct member for it was running, so the
field had nowhere to live on the next write), and it cannot hold an explanation,
which is why the tools documentation had to move into `notes:` fields.

Normalized tables, with the rule that a column is for anything that joins,
filters or validates, and JSON for what is only carried along with its parent.
The property that matters more than the schema: the mapper is LOSSLESS BY
CONSTRUCTION — entities round-trip through their own YAML as a map, columns are
lifted out, and the remainder is stored verbatim, so forgetting a field costs a
column rather than data.

**next** nothing required. Optional follow-ons in the doc's §6: per-entry writes
(two operators editing different models still clobber each other), history in
the UI (it is CLI-only), and `corrallm config import` still writing a file
nothing reads.
**⚠ behaviour change for anything external** reading `~/.corrallm/config.yml`
now finds nothing. `corrallm config export` is the replacement, and
`config history | show | restore` is the new undo.
**verified live** imported, verified, retired; 2 servers / 15 models / 3 groups,
28 models served, `Qwen3.8-27B` generating from a DB-sourced config.
**found by tests, not reading** four bugs, the sharpest being that a FRESH
install wrote an empty config.yml purely so the import could retire it two log
lines later — `bootstrapConfig` is gone, and a new install now creates only
`admin.token` and the DB.


## P21 — provider credentials (P21a–i)

P21a–i shipped; the remaining slice (P21c budget granularity) is active in `plan/plan.md`.
Design doc: **`plan/p21-provider-credentials.md`** — its status list is the accurate one.
Latest: **P21i** (2026-08-15) — approvals are gone. One concept now: you browse a
provider's directory and ASSIGN models, optionally into a lane at a priority. No queue,
no pending/approved/rejected, no `approvalRequired`. A discover filter still exists for
the bulk case, but the large-catalogue presets default to choosing by hand.

Completes the P16 insight the implementation shortcut — *"free quota is enforced per
ACCOUNT ... across multiple accounts of the same provider"* — by making `credentials` a
list under a provider instead of one `proxy.headers`. Unblocked four things at once:
pooling several keys of one provider, budgets keyed where the provider actually meters
(per API key, not per served model), an ACL of which corrallm keys may use which
credential, and a UI with an object to render.

Deliberately NOT an OpenRouter extension: discovery, roster, quota, cooldown and cost
normalisation are already generic and already work. The missing primitive is credentials,
and Anthropic/Groq need it at the second key.

---

## P28 — tickets (a 429 is no longer anonymous)

Shipped `7c48517` (P28a tickets) / `986796e` (P28d patience) / `e422a27` (P28b/P28e journeys),
plus `32eb6a0` — the ticket index crash-looped an EXISTING database. agentkit echoes the
ticket on retry (`bdf042e`, committed). Design in `plan/p28-tickets.md`.

- ◐ **P28 tickets** (2026-08-24) — a 429 is no longer anonymous. corrallm mints a signed
  ticket when it turns away a request that has none, hands it back (header + body), records
  it on the activity row, and the scheduler spends its AGE: an attempt that has been trying
  longer outranks a fresh arrival, bounded so a configured weight still wins. Journeys group
  a caller's attempts into one story — 4 attempts / 3 rejections / 41s spent — and lane rows
  are marked as aggregates so one slot stops being reported twice. agentkit echoes the ticket
  (written, uncommitted — that tree has unrelated WIP). Design in `plan/p28-tickets.md`.

## P18 — multi-GPU capacity (pools bound to cards by UUID)

Shipped `c7453b5`. The one open thread — splitting `ramUsage`'s two jobs into an explicit
`pool:` for placement and measurement for size — is a user-owned decision in `plan/plan.md` §6.

- ◐ **P18 multi-GPU capacity** (hardware-driven; UNCOMMITTED as of 2026-08-05) — a second
  card (RTX 3080) went into box1 and corrallm could not see it. Capacity was single-device by
  construction: `defaultVRAMPool = "gpu0"`, one `DevicePool` per server, and `sizeFrom` emitting
  exactly one gpu key. Worse, every probe called `gpu.Probe()` = **nvidia-smi index 0**, and that
  index MOVED onto the new card — the dashboard began reporting a 10 GiB device under a pool
  budgeted for 32 GiB, and the slot auto-tuner started sizing against the wrong hardware.
  **Root cause worth keeping: the two orderings a host offers disagree.** nvidia-smi enumerates by
  PCI bus id (the BIOS scans the chipset uplink before the CPU slot port, so the card FARTHER from
  the CPU takes the lower number); CUDA/llama.cpp order FASTEST_FIRST. Index is a fine label and a
  terrible identity. Shipped: `gpu.ProbeAll`/`Select`/`Matches` binding pools to cards by **UUID or
  PCI bus id, never position** (bare indices are REJECTED, and ambiguity is an error);
  `ProcVRAMByDevice` so a footprint charges the card that gave up the bytes; `Server.Devices`
  (pool → selector) + validation; per-model device resolution through `DevicePoolsNamedBy` for the
  tune cache, the auto-tuner and measurement publish; residency reports `gpus[]` each tagged with
  its pool; `introspect` prints every card with pool/budget/UUID and flags cards no pool claims.
  Prod: box1 declares gpu0=5090 / gpu1=3080 by UUID, nomic + chandra-ocr-2 moved to gpu1,
  Qwen + deepseek PINNED to gpu0 with `CUDA_VISIBLE_DEVICES=<uuid>` — without that pin llama.cpp
  enumerates both cards and splits across them, which would have made the whole ledger fiction.
  **Placement is NOT enforced by pools** (deliberate, operator's call): corrallm accounts, the cmd
  decides. Verified live: all three local models resident simultaneously on the right cards, and
  two tune profiles resolving to two DIFFERENT cards in one `introspect` — the lookup that was
  silently broken.
  **next:** commit it (14 files); `ui/gen/schema.graphql` regenerated.
  **open decision (user-owned, agreed but unbuilt):** ramUsage should stop carrying two jobs. Size
  is measurable (`sampleVRAMPeak` already does it), but the multi-GPU work made ramUsage's KEYS the
  only statement of WHICH card a model uses. Split it: an explicit `pool:` for placement, measurement
  for size. Decided with it: an unmeasured model claims **NOTHING** rather than the whole pool, and a
  failed spawn is the fit signal — the first run is the operator's problem. **Caveat recorded:** that
  is safe on device pools (a CUDA OOM kills only the allocator) and is exactly the rule that already
  bit this box on the `system` pool, where oidio reached 119G anon-rss and took corrallm down with it.
  What saved that was the `MemoryMax` cgroup, not the ledger.

---

# Toolchain

## ✅ P25 — toolchain registry (COMPLETE 2026-08-21)


Design doc: **`plan/p25-toolchain.md`**. Scoped 2026-08-17; **P25a shipped the
same day** — registry + recipes + agent surface + CLI, read-only plus
`install-deps`. All phases shipped.

corrallm knows what models it runs and nothing about the **programs that run
them** — llama.cpp is a path in a `cmd:` string and that is the whole of its
awareness. Three forcing functions: the tools move (llama.cpp ships daily;
LM Studio and Unsloth pushed tool-calling changes that need a fresh build);
there is a second engine worth tracking ([ninfer](https://github.com/Neroued/ninfer),
OpenAI-compatible, **sm_120a only**); and the same binary is spelled as two
hand-maintained absolute paths across box1 and carlsmacbookpro, so nothing links
a rebuild to the models depending on it.

Decided (user, 2026-08-17): recipes are **bash in corrallm's tree**
(`scripts/tools/<name>.sh`, ported from ml-kit's `llama-rebuild`, embedded via
`go:embed` so they ride agent self-update); `${tool:name}` cmd binding is
**opt-in per model**; builds are **operator-triggered**, with the scheduled
upstream check **on** by default and scheduled rebuild **off**.

A fifth verb landed with it at the user's ask: **`install-deps`**, which installs
what `preflight` reported missing. Doubly gated — the agent refuses it without
`--allow-install-deps`, and the registry refuses it outright on an ADOPTED entry
— and never scheduled. Both refusals return the exact command, so "no" costs a
copy-paste rather than an investigation.

**P25c (2026-08-17)** — `build` shipped for llama.cpp and run for real on box1.
box1's `llama.cpp` is now MANAGED (built into `~/.corrallm/tools/llama.cpp`);
the ml-kit install its models actually spawn is declared beside it as
`llama.cpp-mlkit`, adopted, so the in-use binary keeps its version and drift
visible during the transition. **Two box1 environment faults that only a real
build could surface:** (1) `cc` is gcc 13.3 while `c++` is clang 18.1.3, and
ggml derives its C warning flags from one compiler id — clang's flags reached
gcc and killed the build at 2%; ml-kit pins `CC`/`CXX=clang` and dropping that
was the entire failure. (2) `git remote get-url` applies this box's
`insteadOf` rewrite, so `align_tree` "updated" the origin to the value it
already had on every run; compare `git config --get remote.origin.url`.

**built for real** `0.1.1-dev (build 10472, commit 60eeeb608)` in 481s, both
`sm_86` and `sm_120a` cubins confirmed in `libggml-cuda.so` (one binary for the
5090 AND the 3080), `--list-devices` sees both cards. Stamp verified both ways:
rebuilt when master moved, skipped in 2.1s when it had not. ccache makes
rebuilds ~5x cheaper than the first.
**⚠ operational rule this phase established (USER should know)** a new top-level
config field is NOT safe to add while an OLDER daemon is running. Reading is
fine — `yaml.Unmarshal` ignores unknown fields, and the pre-`tools:` binary
validated the file. But `forWriting` marshals the in-memory struct back out, so
a field the running binary lacks has nowhere to live: at 13:01 the daemon
rewrote config.yml and the whole `tools:` block silently VANISHED. Restored, but
it will go again on the next autonomous config write until a daemon that knows
`tools:` is the one running. **Deploy + restart before relying on it.**
**resolved** deployed + restarted 2026-08-17 (`8e21890`, 0 in-flight, 4 models
evicted and reloaded); `TestToolsSurviveSave` now pins the round trip. Related:
the managed config cannot keep `#` comments either, so everything worth knowing
about a tool moved into its `notes:` field, which does survive a rewrite.

**P25e (2026-08-17)** — `${tool:x}` binding shipped and PROVEN live.
`nomic-embed-text` now reads `${tool:llama.cpp}/llama-server`; SIGHUP reloaded it
with nothing evicted, and it served a 768-dim embedding in 1.6s from
`/proc/<pid>/exe` = `~/.corrallm/tools/llama.cpp/bin/llama-server` — corrallm's
own build — on the **3080**, which exercises the sm_86 half of the multi-arch
build. The other five local models keep their absolute ml-kit paths; opt-in is
the point. Resolution refuses rather than falling back to PATH.
**all six local models migrated** — no absolute ml-kit path remains in any model
cmd. The two hosts migrate to different things, which is the design working: on
box1 `${tool:llama.cpp}` is the MANAGED build (10380 → 10478, a real version
change), on the Mac it is ADOPTED and resolves to the identical path already in
use (a no-op). Each box1 model was load-verified — all four spawn with
`/proc/<pid>/exe` = corrallm's build — and `Qwen3.8-27B` (the one most likely to
break on a version bump, `--spec-type draft-mtp`) was made to GENERATE, not just
pass a health check. Added `corrallm tools resolve` so a migration is checkable
without spawning anything, which is how the Mac was verified without pulling 35B
of weights onto a laptop.
**known gap (P25e)** `SpecFor` derives a MANAGED prefix from the PRIMARY's home
for every host — right for box1, wrong for a remote managed install under
`/Users`. Resolution asks the host instead, so it is host-truthful anyway, but
fix the prefix before managing a tool anywhere but box1.

**P25f (2026-08-21)** — the scheduled drift check, and the last phase.
`toolchain.Watcher` wakes once a minute, and each tool comes due on its OWN
`check:` cadence (default 6h); `check: off` means the host is never contacted at
all, not contacted-and-discarded. Wired in `main.go` beside `watchReload`, and it
re-reads config every pass, so changing `check:`/`rebuild:` takes effect without
a restart. CHECK IS ON, BUILD IS OPT-IN: drift always logs at WARN, and a rebuild
happens only with `rebuild: true` on that tool.
**never rebuilt on a schedule** an ADOPTED entry, even with `rebuild: true`.
`Registry.Build` already refuses one; the watcher refusing FIRST is what stops a
scheduled check filing a guaranteed-to-fail build every six hours forever.
**verified live (box1, 2026-08-21)** one pass against the real config found real
drift on three tools — `llama.cpp` box1 `9731ad3f`→`9a286ac9`, `llama.cpp-mlkit`
`0b1bad14f`→`9a286ac9`, `ninfer` `a05746aa`→`feaf4dd0` — all `rebuild=false`, so
nothing built. The Mac logged "declared but not installed" at INFO rather than as
drift. A second pass immediately after was suppressed by the cadence, as designed.
**caught by mutation testing, not review** the first version of
`TestDriftWithoutRebuildReportsOnly` sampled the call log straight after
`CheckDue` and passed with the `if !t.Rebuild` guard DELETED — `Builder.Start`
returns before its goroutine runs. It now waits on a channel. Every other guard
(cadence, `check: off`, adopted, not-behind) was mutation-checked too; the
unreachable-host case survives single mutations because `st.Error != ""` and
`st.Drift == nil` are redundant for it, and fails only when both are removed.
**decided** absence is NOT drift: a managed tool that is declared but not
installed is never auto-installed by the timer. `rebuild:` is documented as
"build when it finds DRIFT", and first install stays a human action.

**risks** a build competes with resident models for the same GPU (P25c decides
whether it takes an admission slot; starting "reported, not scheduled");
`git clean -xdf` is destructive, so managed trees live under `~/.corrallm/tools/`
and never point at a human's checkout.
**verified live** box1's adopted ml-kit llama.cpp reports `10380 (0b1bad14f)`
from the binary and **BEHIND `34af94cd9`** — drift on an install corrallm never
built, which is the day-one payoff. carlsmacbookpro was really dialled and
answered 404, rendering as the designed "its agent is too old" rather than a
mystery HTTP error.
**found while building it (box1)** TWO nvcc installs: `/usr/bin/nvcc` is the
distro's CUDA **12.0** and shadows `/usr/local/cuda-13.3/bin/nvcc` in PATH.
Trusting `command -v nvcc` reported "CUDA 12.0, too old for ninfer" on a box with
13.3 working. The recipe now uses ml-kit's resolution order and reports the
shadowing as a note even when the answer is fine.
**assumption recorded** the live config was NOT touched — P25a was verified
against a copy. Adding the `tools:` block to `~/.corrallm/config.yml` is the
user's call (below).
**pre-existing, not mine** `TestLiveConfigFreeLaneIncludesThePool` already fails
at HEAD (commit `c0b98eb` repointed groq-llama-70b in the live config without
updating the test); plus four gofmt-dirty files and three vet copylocks hits from
`Config`'s mutex. None touched by P25a.
**measured, not assumed (box1, 2026-08-17)** `llama-server --version` writes to
**stderr** (a naive `$(...)` capture reports "unknown" on a good binary); ninfer
has **no `--version` anywhere**, so a ninfer built outside corrallm is
unidentifiable; ninfer **will not build on box1 today** — CUDA 13.3 ✓, cmake
3.28.3 ✓ (exactly its floor), ffmpeg dev libs ✗ — which is why the recipe
contract has a `preflight` verb.
**trap (load-bearing)** builds must not go through the agent's backend table:
`ReconcileAgent` reaps unclaimed backends after a 60s grace, so a 15-minute CUDA
compile would be killed every time. Separate `/agent/v1/tools/*` surface, and
**Protocol stays at 1** — bumping it takes the whole fleet out until every agent
self-updates, whereas an old agent simply 404s a new route.
**blocking decisions (USER)** whether the live `~/.corrallm/config.yml` gets the
`tools:` block (additive, inert, validates — but it is a running service's
config); whether the llama.cpp pin moves into `tools:` or stays in ml-kit's
`llama.cpp.pin` (P25a adopts rather than owns, so it can wait until P25c); and
whether ninfer is worth running before its checkpoints are on the box.
**optional extensions** other tools (oidio, whichever engine lands next) are the
same shape once the contract exists; not in scope.


## P27 — versioned tool builds

- ✅ **P27 versioned tool builds** (2026-08-23) — a build no longer overwrites the one that
  is serving. Each lands in `<prefix>/builds/<utc>-<head8>` and `bin` is a symlink at the
  active one, so a rollback is a rename rather than a recompile: `corrallm tools builds`
  lists what is installed, `corrallm tools activate <id>` puts one back, the newest 5 are
  kept and the active build is never pruned. The pre-versioning install is migrated into
  `builds/` on the next build rather than deleted, so the FIRST versioned build already has
  something to fall back to. Verified live on carlsmacbookpro: forced rebuild (118s) →
  migrate + install + swap, rollback and forward, refusals for an unknown id and an adopted
  entry. Also here: `corrallm tools` read the retired `config.yml` and reported "no tools
  declared" on the live box (P26 regression) — it reads the database now; and the Mac's
  llama.cpp went MANAGED, so it builds its own copy instead of probing ml-kit's.

## ✅ P29 — pin a tool to a commit, and roll back to a build you already have (2026-08-24)

**The problem, stated by the person who had it:** llama.cpp ships several builds a day, one of
them regresses a model, and the only lever was to stop rebuilding and remember why — for weeks.
P25 tracked versions and P27 made rollback cheap, but neither was reachable from the dashboard and
neither survived a scheduled rebuild.

**Two levers, deliberately separate**, because they act on different things and doing only one is
the common mistake in both directions:

- **A pin is configuration.** `tools.<name>.pin` — a full 40-char sha, applying to every host,
  surviving every later build including a scheduled one.
- **Activate is local.** P27's symlink rename, one host, a second, no compiler.

Activate without pinning and the next check undoes it; pin without activating and the host keeps
serving the bad build. The dialog says so rather than assuming.

**`pin` is separate from `ref` and does not overwrite it.** Ref says what the tool TRACKS; pin says
"not past here". Putting a sha in `ref` would work for the hold and cost two things: the branch you
meant to return to (un-pinning becomes an act of memory), and the drift check itself, which cannot
follow a commit anywhere.

**The semantic that carries the feature: while pinned, `behind` means behind the PIN.** If it kept
meaning "behind upstream", a pinned tool would wear a permanent drift warning AND — with
`rebuild: true` — get rebuilt straight past the hold every six hours, which is the exact regression
a pin is set to prevent. `ahead` is the informational half: "master is at 34af94cd9, you are holding
0b1bad14f", visible without un-pinning to find out.

Three things the implementation had to get right, each a real failure mode:

1. **`git fetch origin <sha>` is not reliable.** upload-pack's `allowReachableSHA1InWant` is off by
   default; GitHub enables it, a mirror may not. The recipe falls back to fetching every branch and
   locating the commit locally, and says so in the log. Forced deterministically in test with a git
   shim, because the local file transport used by the suite happens to allow the direct fetch — so
   the fallback would otherwise never run and would break unnoticed.
2. **An abbreviated sha is refused, not resolved.** A host with a clone could expand it and a host
   without one could not: it would work on the machine you set it from and fail on the machine you
   needed it on.
3. **`pin` has NO column.** It rides in configdb's unprojected remainder, by design — the schema is
   applied with `CREATE TABLE IF NOT EXISTS`, so a new column would never appear on the live
   database and every read would fail. (Same shape as the ticket-index crash-loop, `32eb6a0`.)
   `TestToolPinSurvivesTheStore` pins the round trip.

Also here: `tools` became a kind the YAML entry editor understands — it was the one config kind with
no write surface at all — and deleting a tool now names the models whose `${tool:x}` would stop
resolving.

**A FOURTH failure, found only by deploying — a stale agent silently un-pins a host.** Agents carry
their OWN embedded recipes and self-update on a build-id mismatch, so between deploying the primary
and a host's next heartbeat the older recipe answers `upstream` — and it computes drift against the
tracked ref, having never heard of `TOOL_PIN`. The Mac reported BEHIND while sitting exactly on the
pin. That is worse than a stale number: `behind` is what the watcher acts on, so with
`rebuild: true` it would start a build there, which the same old recipe aligns to **master** — the
pin walked past by the machinery meant to honour it.

Fixed by feature-detecting on the wire (a pinned recipe always echoes the pin back; an old one never
can), NOT by a protocol bump — bumping would take the whole fleet out until each agent self-updated.
An answer that cannot see the pin is treated as no drift answer at all: `Behind` is cleared so the
watcher cannot fire, the row says "agent predates pins", and an explicit build there is refused
naming both commits. Pinned by three tests against a runner that replays the old JSON shape.

It nearly demonstrated itself: the Mac rebuilt 10603 → 10615 mid-session on the scheduled check. Had
the pin been the older commit during that window, that rebuild would have gone straight past it.

**Verified live (2026-08-24), box1 + carlsmacbookpro:**

- Pin written through the new op, persisted through the config store and a **daemon restart** — the
  historic loss mode for anything under `tools:`.
- box1 pinned at its serving commit `f280b2698` reads **at pin**, `behind=false`, with
  `rebuild: true` still on. That is the whole feature: the box stops rebuilding when master moves.
- Pinned deliberately BACKWARD to `c060ca974`: box1 flipped to `behind=true, ahead=true` — held
  back, master past it — then restored. Both halves of the semantic, on a real fleet.
- The Mac exercised the stale-agent guard end to end (see above).
- **GitHub does serve a bare sha fetch**: `git fetch --depth=1 origin <non-tip sha>` returns that
  exact commit, so `align_tree`'s primary path is real. The fallback covers remotes that refuse, and
  is forced in test with a git shim because the local transport the suite uses allows the direct
  fetch — so it would otherwise never run and would break unnoticed.

**Still not run:** a forced pinned BUILD on box1 (8–20 min of CUDA that replaces the serving
llama-server, with the same commit, since the pin is its current head). The checkout half is covered
by tests and by the GitHub fetch above; only the full compile-under-a-pin is unexercised.
**optional extension:** run it next time box1 is idle. **Agents shipped** (`make agents`, 2026-08-24): served
version `8303de9-dirty` → `32eb6a0-dirty` and the Mac self-updated on its next heartbeat, clearing
`pinUnsupported` and reporting **at pin**. No restart was needed — agentdist reads
`bin/agents/VERSION` per request, so `make agents` alone closes the window without evicting a model.


---

# Bench & models

## P15 — bench: capability verification, performance profiling & user probes

- ✅ **P15 — Bench: capability verification, performance profiling & user probes.** Fold the
  crucible eval harness (`github.com/iodesystems/crucible`) INTO this repo as a **second binary**
  (`cmd/corrallm-bench`, plus its MCP helper) so corrallm owns measurement as well as serving.
  One engine, three probe tiers. Depends on nothing in P0–P14; sequence it after P11d.

  **DONE — landed as `cmd/llm-bench` + `cmd/llm-bench-mcp`** (not `cmd/corrallm-bench`; the
  binary took the name it is invoked by in `serve --bench-bin`). Verified 2026-07-29:

  - **Engine moved whole.** `internal/bench/{check,journal,judge,report,run,task}` mirrors
    crucible's package-for-package, and has since grown past it: `run/residency.go` (the
    cold-path control that was this item's entire justification), `run/audio.go`,
    `check/starlark.go`, `task/markdown.go`.
  - **All 14 crucible tasks are present** in `probes/`, plus 7 that crucible could never have
    run: `capability-{vision,stt,tts,diarize}` (need residency control) and
    `edit-safety-{import,pop,rename}`. Diffed task-by-task: 10 byte-identical, and the 4 that
    differ are `crucible-mcp`→`llm-bench-mcp` renames in comments, one gofmt space, and the
    README. No semantic drift.
  - **The MCP helper carried its jailing**, including the 2026-07-16 fix that checks EVERY argv
    element rather than argv[0] (`cmd/llm-bench-mcp/main.go:358`).
  - **Crucible is deleted**, not merely superseded: its last real commit and its last gateway
    call were both 2026-07-18 (9,247 calls lifetime), and corrallm's bench keys have served
    every run since. The repo had no remote, so its plan — the only record of the tool-format
    axis results, the poly-lsp net-negative measurement, and the run-to-run variance data this
    item cites — is kept verbatim at `plan/archive/crucible-plan.md`. Everything else went.

  **Why fold rather than federate.** The decisive argument is lifecycle access, not tidiness.
  A capability claim can only be falsified by probing a model **cold**, and corrallm is the only
  component that can force that — it owns residency, eviction and the `loadModel`/`unloadModel`
  ops (`internal/api/handlers.go:1109,1123`). Crucible today talks to corrallm over HTTP as a
  black box: it cannot evict, so it can never test the cold path, so the entire class of
  cold-load bugs is invisible to it. In-repo, the bench binary drives residency deliberately.
  Keep the transport **HTTP over the real `/v1` surface** even in one repo — an in-process
  shortcut would stop testing the thing users actually hit.

  **Motivating bug (2026-07-18).** `ternary-bonsai-27b` declared `modalities.image` and
  `corrallm.yaml` claimed it "verified end-to-end (red-circle test)". It silently drops the image
  on the FIRST request after a cold load — the model's own reasoning says "no actual image
  attached" — and works once warm. `/props` reports `vision: true` and the mmproj loads, so every
  warm probe passes. Nothing in either codebase could contradict the declaration: corrallm never
  calls `/props` or otherwise checks a declared modality against the live backend (declaration is
  pure operator-trust, `config.go:96-98`), and crucible has no modality concept at all. The claim
  was verified once, by a human, against a warm model. **Cold-path probing is therefore a P15
  requirement, not a nice-to-have** — a warm-only capability check would have passed this bug.

  > **⚠ Correction (2026-07-20): the cold-drop bug did not reproduce, and the probe was at fault.**
  > With sampling pinned (`--temp 0` + seed) and the workspace tools removed from capability probes,
  > `ternary-bonsai-27b` answers "Red circle" **cold AND warm, 3/3 runs each** — and so does
  > `gemma-4-12b` (12/12 stage rows). The cold pass evaluates ~900 prompt tokens of image and gives
  > the same answer as warm, so the image is not being dropped.
  >
  > What actually happened: the probe ran through the agent loop with the default system prompt
  > ("...working in a sandboxed workspace. You have MCP tools: read_file, write_file, list_dir...").
  > The model read "this image" as a file to go find, called `list_dir`, got `.git/`, and answered
  > *"I don't see any image files in the workspace"* — while the identical prompt sent straight to
  > the backend answered *"Red circles"*. `maxToolCallsPerStage: 0` did not save it: that caps calls
  > made, but the tools were still advertised. Sampling then hid the pattern — bonsai and gemma run
  > at `--temp 0.7` and the harness sent no temperature, so warm sometimes got lucky and the
  > cold/warm split read as a residency bug.
  >
  > The 2026-07-18 human observation is left above as recorded; it may have been the same harness
  > effect or genuinely fixed since. **Cold-path probing stays a requirement** — nothing here shows
  > it is unnecessary, only that this particular finding was an artifact. What it does show is that
  > a capability probe must test the BACKEND, not the agent loop wrapped around it.

  **Tiers (one runner, three probe kinds):**
  - **T1 capability** — does the model do what it CLAIMS? Cross-check declared `modalities`
    against the live backend: `/props`, plus real payloads per declared modality (image in →
    describes it, audio in → transcribes, declared `formats`, tool-calling, structured output).
    Cheap, deterministic, pass/fail. **Runs cold AND warm**; a cold/warm disagreement is itself
    the finding. This supersedes the ad-hoc `ml-kit/bin/capcheck` sketch — don't build both.
  - **T2 performance** — gen/prompt tok/s at several context depths, cold load seconds, VRAM,
    spec-decode acceptance. Most inputs already exist: `tune` measures VRAM
    (`internal/tune/tune.go`), and llama.cpp's `timings` are parsed into
    `PromptPerSec`/`PredictedPerSec` per request (`proxy.go:1343-1364`, `store.go:119-120`) —
    but they are **never aggregated**: `RollupByModel` (`store.go:209`) sums tokens/dwell/cost
    only, so there is no per-model "typical tok/s" anywhere in the product. T2 adds deliberate
    timed probes + the missing aggregation.
  - **T3 quality** — crucible's existing tasks/checks/judge/toolsets, moved wholesale. Its
    `task.yaml` + check DSL IS the **user-defined probe format**; reuse it for T1/T2 built-ins
    (a new task `class`) rather than inventing a second assertion language. That was the whole
    risk in growing a separate bench, and folding is what removes it.

  **Run N times, report variance — non-negotiable.** Four back-to-back baseline runs of the same
  two models (2026-07-18) gave 15/19, 18/20, 16/20, 18/20 for one model against 15/19, 18/20,
  18/20, 18/20 for the other. Three runs tied exactly; the one apparent 2-stage gap was
  infrastructure (aborted stages), not capability. **A single run's per-task diff between similar
  models is noise** and reading it as signal is the default failure mode of this kind of tool.
  Throughput was the stable discriminator (1.43× aggregate across runs, consistent every run)
  while wall-clock was not (one run had the "faster" model slower, via swap churn). So: repeat
  probes, surface spread not just a point estimate, and prefer throughput over wall-clock.

  **Persistence + UI.** Keep crucible's `out/<ts>/{runs.jsonl,summary.csv,report.md}` artifacts
  for a full run, but ALSO persist a per-`(gpuName, model)` summary the way `tune.Cache` does
  (`tune.go:132-134`) so the dashboard shows current capability/perf without re-running. UI: a
  **model catalog / comparison view** — the gap today is that `ui/src/routes/index.tsx` groups by
  capability and `model.tsx` shows one model's own numbers, but nothing is cross-model. The T1
  capability matrix wants a declared-vs-verified column, so a false claim is visible as a red cell
  rather than a comment in a YAML file.

  ✅ **per-capability scoring + per-probe detail** (2026-07-19) — the run-wide pass rate was not a
  comparable number and was being read as one. A probe a model cannot serve is skipped, not failed,
  so an STT model ran 4 audio probes, passed them, and showed ~100% while a chat model ran 20 mixed
  probes and showed 90% — the table ranked the speech model above the chat model at chatting.
  `PublishResults` also flattened every row into one aggregate before it reached the DB, so "which
  probe, and how did it do" had no answer server-side at all.
  - `report.Row.Capability` records the surface the PROBE required (`Requires.EffectiveCapability`),
    stamped in `run.go` alongside RunMode.
  - Skipped probes are captured as `run.Skip` in a slice kept OUT of `rows` — letting them into the
    row set would put zeros into summary.csv/report.md and restate a config fact as a capability gap.
  - `PublishProbeResults` → `POST /api/v1/measurements/probes` folds stage rows to one record per
    (model, probe, runMode); cold/warm stay split because the disagreement is the finding.
  - `bench_probe_results` table; `GET /api/v1/bench/probes` groups by capability server-side and
    scores each capability only on its own probes. Skips count toward neither numerator nor
    denominator but ARE returned, so the console says "not applicable" rather than leaving a hole.
  - `model.tsx` gains a "Last run by capability" accordion (per-probe rows, cold/warm, pass/fail with
    the failing check in a tooltip, skips with their reason); History keeps the aggregate with a note
    that it tracks a model against itself, not against other models.
  - The aggregate `bench_results` path is unchanged and still published — an older llm-bench that
    knows nothing about probe detail keeps working.
  - **Gotcha worth keeping:** Huma derives "required" from the absence of `omitempty`, so reusing the
    read struct as the publish struct made a skip record (which legitimately carries no measurement
    fields) fail validation with 422. Publish and read shapes are separate types for that reason —
    caught only by driving the real endpoint, not by the unit tests.
  - `GET /api/v1/bench/capabilities` (`BenchCapabilityMatrix`) ranks models WITHIN each capability
    off each model's own latest run — latest-per-model, not latest-overall, since models are benched
    at different times and one run id would drop everything not in it. A model whose every probe on
    a surface was skipped is **omitted** from that ranking rather than listed at 0%, which would
    assert a failure that never ran. Ties break by name so map iteration can't reorder a ranking.
  - `bench.tsx` renders one score chart + scatter + table per capability; the flat cross-model table
    is retained but retitled "Run totals" and explicitly labeled not-a-ranking, since its score
    column mixes capabilities. VRAM is joined in from `benchResults` rather than duplicated onto the
    matrix endpoint.
  - **untested** — no real llm-bench run has published through either new endpoint yet; both were
    verified with synthetic payloads against a live server. The end-to-end check reproduced the
    original bug's shape: stt 100% on audio.stt, absent from chat; qwen 90% and gemma 70% on chat.
  - **unverified** — neither UI surface has been visually rendered (the console requires pasting an
    admin token). Both typecheck and lint clean and their data sources are verified.

  ✅ **A/B arms + probe drill-in** (2026-07-19, follow-on) — two gaps in the above.
  - **Arms were being averaged.** `PublishProbeResults` keyed on (model, probe, runMode), so two
    toolsets or two tool formats of the same probe UPSERTed over each other — destroying the exact
    comparison an A/B exists to make. An **arm is (toolset, toolFormat, runMode)** and all three are
    now part of the key, in the publisher and in `bench_probe_results`' UNIQUE constraint.
  - **Baseline arm, not pooled average.** A probe's headline score comes from one designated arm
    (rank: warm > any > cold, then `baseline` toolset, then `json` format, then a lexicographic
    tiebreak so the choice is deterministic); other arms render as ± deltas. Pooling would move a
    model's score whenever an arm was added or dropped, which reads as a quality change that never
    happened. `BenchCapabilityMatrix` folds to the baseline BEFORE accumulating, or a model running
    a 3-arm A/B contributes 3× the stages of one running a single arm.
  - Arms reaching different verdicts set `disagreement` — the finding a pooled score hides.
  - **Drill-in.** `bench_probe_stages` + `bench_probe_checks` persist per-stage metrics (turns, tool
    calls, bait calls, broken intermediates, compactions, tok/s) and every check's kind/desc/pass/
    detail. `GET /bench/probe/detail` serves them. Transcripts and journals stay as FILES and are
    served by `GET /bench/probe/{transcript,journal}` from `bench_runs.out_dir`, which llm-bench now
    reports (corrallm cannot infer it: `--out` is relative to llm-bench's cwd, and it previously
    learned the path only by scraping `wrote out/<ts>` from the child's stdout). Host is recorded so
    a run benched elsewhere says so instead of returning an empty transcript that reads as "the
    model said nothing".
  - Artifact filenames are built server-side via `judge.ComboName`, never taken from the caller,
    with a containment check as backstop — otherwise these endpoints are a file-read primitive for
    anyone who reaches the API. Covered by a traversal test.
  - `Open` drops a pre-arms `bench_probe_results` so the schema recreates it: the fix is to a UNIQUE
    constraint and SQLite cannot alter one in place. **Safe only because the table has never carried
    a real run** — if it ever ships with real history this must become a copy-into-new-table
    migration. See `dropStaleProbeTables`.
  - Verified end-to-end against a live server: `json` baseline 50% vs `toon` **+50%** at 25% fewer
    input tokens, failing check `cmd_ok: exit 2: auth.go:14: undefined: Register`, bait tool call
    surfaced in the journal.
  - **still unverified** — no real llm-bench run has published through this path, and the UI remains
    visually unrendered.
  ✅ **cross-model arm comparison** (2026-07-19, follow-on) — `GET /bench/arms`
  (`BenchArmMatrix`) + an "A/B arms across models" section on `bench.tsx`. The per-model view
  answers "did toon help THIS model"; this answers "does toon help at all", and the second cannot
  be read off the first.
  - Comparisons are **paired per probe**: an arm is credited only on probes where its baseline also
    ran. Unpaired, an arm that happened to run against the strong models looks like an improvement
    it never made. A probe whose baseline never ran is skipped entirely rather than counted as a win
    for whatever did run.
  - Mean **and** median delta are both reported, plus W/L/T and the paired probe/model counts — one
    pathological probe must not carry a verdict the rest of the evidence does not support, and a
    verdict resting on 3 probes must not read like one resting on 60. The mean averages over
    PROBES, not models, since the pairing is per probe and that is where the evidence is.
  - Token delta uses evaluated prompt + completion, so a cached prefix re-sent every turn is not
    charged to an arm twice.
  - Verified live: toon **+5.0% mean / +5.0% median, 3W/1L/2T over 6 paired probes, −1500 tokens** —
    but the per-model table shows that is qwen +13% and claude −3%, i.e. the headline hides a split
    decision. This is exactly why `byModel` is rendered under every arm rather than the aggregate
    alone.

  ✅ **first real run + the four fixes it forced** (2026-07-20) — full matrix, 4 models, exclusive,
  19 min: `83 probe results, 98 stages, 288 checks (13 skipped)`, all readable through the API.
  Capability scoring, the drill-in, and the arm split all worked on real data. Four defects surfaced:
  1. **Artifacts collided per arm.** `persistRun` named files by (model, toolset, task) only, so a
     `run: both` probe's warm pass overwrote the cold one and the drill-in served the PASSING arm's
     transcript — the one nobody needs. Fixed by `judge.ComboVariant` (mode + repeat index in the
     name); the judge reads via `ComboCandidates` (warm before cold), and the API falls back to the
     bare name so historical artifacts still resolve.
  2. **Nothing pinned sampling.** `agentkit`'s `llm.ChatOpts` had no temperature or seed at all, so
     llama.cpp's own `--temp 0.7` governed and capability probes were a coin flip. Added
     `Temperature`/`Seed` (pointers, so unset stays distinct from a deliberate 0) to agentkit and
     pinned them for capability-class probes only — quality probes still measure the model as served.
  3. **One sample became a verdict.** `publishMeasurements` published a capability verdict per pass.
     Now observations accumulate across repeats and `publishCapabilityVerdict` publishes once:
     `verified=true` if ANY repeat saw it work, `verified=false` only when EVERY repeat agrees, and
     a mixed result is labelled `FLAKY: observed in k of n runs`. Deliberately asymmetric — a
     success cannot be a false positive the way a miss can be a false negative.
  4. **Capability probes were measuring the harness** — see the correction under the motivating bug
     above. They now run tool-free with `capabilitySystemPrompt`, which mentions no workspace and no
     tools.
  - **next** — the `find-render-entrypoints` failure the drill-in explained is still real: gemma
    burned 10 turns and 9 tool calls exploring, never wrote `findings.txt`, and one grep went out
    malformed (`"grep","-r","","selectorGrammarHelp","."`). Worth a look as a genuine finding rather
    than a harness artifact.

  **Migration risks / decisions to make before starting:**
  - crucible pulls in `agentkit` (`agent`, `llm`, `mcpmgr`); corrallm currently does not. New dep
    on the serving repo even though only the bench binary uses it — acceptable, but name it.
  - `crucible-mcp` is a separate spawned binary (workspace tools for T3). Two new binaries, not one.
  - Module path rewrite `github.com/iodesystems/crucible/...` → `.../corrallm/...`; crucible's own
    `plan/plan.md`, `tasks/`, and `out/` run history need a home (archive the history, port the plan
    into this file).
  - The bench must NOT run inside the serving process — it contends for the GPU it is measuring.
    Separate binary is the point; if it ever gains an API trigger, it still shells out.
  - **Carry crucible's hard-won scoring lessons, don't re-derive them:** a turn/tool-call cap must
    not veto passing checks (it hid a model that had written the correct answer), but a
    *pathological* breach (identical-call loop, tool-call budget) must; and never infer an abort's
    cause — report the underlying error, or a 9.6 s failure gets blamed on a 10-minute timeout.


## ✅ P22 — Qwen3.8-27B cutover (model aliases) — 2026-08-14

> Cutover, VRAM sizing and the bench run are DONE and recorded below. The open tail —
> the KLD baseline, a clean head-to-head, and the `mcpshell-instructions` probe-scoping
> bug — is an active slice in `plan/plan.md`; do not re-derive the measurements here.

Qwen3.8 shipped 2026-08-03; open weights are **Max (2.4T MoE)** and **27B** only,
so 27B is both the intended target and the only one that fits the 5090.

**✅ Shipped: `aliases` on a model** (`d4f69ee`). Resolution is now lane → model →
**alias** → discovered → glob. An alias yields the CANONICAL name, unlike a glob
which keeps the requested id — a glob's members are distinct models sharing a
spelling and each earns its own metrics row, but an alias is a second name for
ONE process holding ONE reservation, and carrying the requested name would split
residency across two ids for memory allocated once. Validate rejects an alias
that is empty, globbed, self-referential, or that collides with a model, lane, or
another alias. The model-collision rule is load-bearing: resolution returns on an
exact `Models` hit, so a colliding alias could never fire, and a silently dead
alias mid-cutover is worse than a load error.

**◐ In flight: the model itself.** `Qwen3.8-27B` is in the live config, opt-in by
name — no lane, no alias — so 3.6 and 3.8 can be compared without one of them
silently serving `chat`. It runs on 5802 against the same 220k window as 3.6, so
a comparison measures the model and not the context.

**✅ MEASURED 2026-08-14 (live, via corrallm after `bin/deploy --no-ui`).** It
loads, serves correct output, and vision survives (mmproj-BF16 auto-pulled).

    Q5_K_M + Q5_K_M draft @220k   30322 MiB  ->  1473 MiB free
    Qwen3-6-27B-MPT       @220k   28952 MiB  ->  3655 MiB free   (config.yml:381)

**That gap is a REGRESSION, not spare room.** config.yml:383 spends 3.6's 3655 MiB
explicitly: "what a 400 DPI page's image-encoder activations have to fit inside …
treat the headroom as spoken for, not spare." 3.8 as first configured more than
halves it, on a model carrying `convert: pdf: vision`.

**The hybrid saved memory; the draft spent 3× it.** Backing out the 2130 MiB
draft, 3.8's own footprint is ~28192 MiB against 3.6's 28952 — ~760 MiB less,
consistent with DeltaNet layers holding recurrent state rather than KV. The
prediction that the arch changes the KV picture held, but not enough to matter.

**The third-party MTP draft works:** `draft acceptance = 0.889` (48/54), mean
accepted len 3.67. Materially de-risks the a4lg dependency.

**✅ SETTLED: Q5_K_M at `-c 188000`, vision + MTP draft both kept, 88.8 tok/s.**
Full measured set on this card (32607 MiB):

    quant / ctx / img-cap            idle   vision peak  free at peak
    Q5_K_M @220k  draft  16384      30322        —           1473   rejected
    UD-Q4_K_XL @160k draft 16384    27070      29148         3459
    Q5_K_M @160k  draft  16384      28804      30914         1693
    Q5_K_M @176k  draft  16384      29423      31534         1073   UNSAFE
    Q5_K_M @176k  draft   8192      29423      30526         2081
    Q5_K_M @188k  draft   8192 <-   29881      30983         1624   LIVE
    Q5_K_M @200k  NO draft 16384    27239      29318         3289
    (Qwen3-6-27B-MPT @220k)         28952        —           3655

**KV costs 38.5 MiB per 1k tokens**, consistent across four load points and both
quants — cache types are fixed (`k q8_0 / v q5_1`), so weight quant does not
move it. Every derived number here comes off that constant.

**The image cap — not the quant — was the context lever.** `--image-max-tokens`
sizes the vision spike near-linearly (16384 → 2111 MiB; 8192 → 1103 MiB).
Halving it bought **28k of context, 160k → 188k, at the same safety margin**,
keeping Q5_K_M, vision and the draft. No quant drop was needed, and none was
made — the operator wants KLD numbers before anything below Q5, which is the
right test and is NOT yet run.

8192 suits the actual workload: `convert.pdf` renders at dpi 200 (~4–5k image
tokens), so the cap only binds on a directly-attached oversize image, and the
400 DPI worst case still read its invoice correctly at 8082 tokens. 3.6 keeps
16384 — per-model choice, not policy.

**`--no-mmproj-offload` tried and REJECTED.** Memory-wise it is perfect — the
spike leaves VRAM entirely (peak 29200 vs 29199 idle, 3407 MiB free at 200k) —
but one 400 DPI page did not finish encoding in TEN MINUTES and was cancelled.
Unusable interactively.

**The vision spike is 2078 MiB, measured** — an 8.5×11in page at 400 DPI
(3400×4400), read correctly, 14652 image tokens against the 16384 cap. That is
well under the ~3.6GB the 3.6 note conservatively reserves.

**Peak is context-INDEPENDENT once loaded**, which is what made 1693 MiB
acceptable rather than alarming. Worst case actually run — 128063 prompt tokens
(a 128k distractor log) WITH the same 400 DPI page — peaked at 30932 MiB, 1675
MiB free: 18 MiB below the small-prompt peak, i.e. noise. KV is preallocated at
n_ctx and compute buffers are sized by `--batch-size`, so a long prompt does not
raise the ceiling; it is paid at load. It read the invoice correctly with 128k of
noise in front of the image.

**ramUsage stays 31GB and that is correct** — admission charges PeakMiB (box1's
"QUOTE PEAKS, NOT BASES"), and measured peak is 30932 MiB ≈ 30.2GB. Comparing it
to the 28804 idle would be the exact error that note exists to prevent.

**A correction worth keeping:** the UD-Q4_K_XL move was predicted to leave *more*
headroom than 3.6. It did not — 3459 vs 3655 MiB. The process-size prediction was
near-exact (28499 vs 28548 actual); the headroom claim was wrong because ~810 MiB
of card overhead sits outside the process and was folded into the wrong side.

**Sampling is a DEFAULT, not a policy** (settled 2026-08-14). llama.cpp reads
`chat_template_kwargs.enable_thinking` per request and overrides `--reasoning`
with it (`server-common.cpp:1079`), and corrallm forwards bodies untouched — so
both the sampler and the thinking toggle are per-request. CLI carries the card's
non-thinking numbers; a thinking caller must send the thinking sampler itself,
because one set of defaults cannot serve both modes and the mismatch degrades
output silently rather than erroring.


## ✅ Qwen3-6-27B-MPT CUDA-OOM'd in production (2026-08-14) — resolved by retiring it

While llm-bench ran the vision probe, the INCUMBENT crashed:

    CUDA error: out of memory   (ggml_cuda_flash_attn_ext_tile_case<72,72>)
    Aborted (core dumped) — backend exited err="exit status 134"

Its measured footprint ratcheted 29180 → 30898 → **31124 MiB** as the probe ran,
i.e. ~1483 MiB free on a 32607 MiB card. The config's cold "28952 / 3655 MiB
free" is simply not what it uses under a 400 DPI page. This is the calibration
point for every margin above: 1483 MiB was not enough, so 1073 MiB (3.8 @176k
at cap 16384) is out and 1624 MiB (3.8 @188k) is the floor being accepted.

**Two live consequences, neither yet addressed:**
1. 3.6 at 220k with `--image-max-tokens 16384` can OOM on a vision request in
   production. It crash-recovers, but it drops in-flight work when it does.
2. `PeakMiB` is monotonic BY DESIGN, so 3.6 now permanently claims 31124 MiB of
   the 31583 MiB pool budget. It will effectively refuse to co-reside with
   anything from here on — including 3.8 — until that profile is reset.

**RESOLVED by retirement, not by fixing it.** 3.6 was removed from the config in
the same session, so neither consequence is live: nothing serves it, and its
stranded profile holds none of gpu0's budget. The 8192-image-cap fix was never
applied to it and no longer needs to be. Kept here because the crash is the
calibration for every margin in P22 — 1483 MiB was not enough, which is why
3.8 @176k/16384 (1073 MiB) was rejected and 1624 MiB is the accepted floor.

**next** re-run the head-to-head (the 2026-08-14 run was stopped mid-flight, and
3.6's half of it was collected while crash-looping — treat `out/20260814-112505`
as void), then the KLD baseline before any sub-Q5 discussion.

**throughput is NOT yet comparable.** A single ad-hoc prompt gave 3.8 91.2 tok/s
against the 136 tok/s recorded for 3.6 in `ml-kit/docs/model-evals.md` — but that
figure came from the llm-bench harness over 2 runs at 180k ctx, while this one was
one prompt at 220k taken while a 17GB download saturated I/O. The two numbers do
not belong in the same table. The real comparison is the harness that rejected
the 35B-A3B: `llm-bench run --models Qwen3-6-27B-MPT,Qwen3.8-27B`. Note its judge
runs on the `chat` LANE, which holds only 3.6 — a judged head-to-head puts judge
and candidate on the same card. The 35B rejection turned on DETERMINISTIC probes,
not judge-scored ones, so a first pass can skip `--judge` and avoid the thrash.

**risks**
- **VRAM is the live one.** 31GB is copied from the 3.6 entry plus an unmeasured
  MTP head, on a card where 3.6 already sits within ~3.6GB of the ceiling. 3.8 is
  a hybrid (gated DeltaNet + gated attention, GGUF arch `qwen35`) and DeltaNet
  layers hold recurrent state rather than KV, so the 220k footprint may not
  resemble 3.6's in either direction.
- **Only one 27B can be resident** (31GB reserved of 32.6GB). Any A/B is serial
  swaps, ~30s declared / ~66s cold — never split traffic.
- **The MTP draft is third-party.** unsloth published no MTP GGUF for 3.8, so
  `--spec-draft-hf` points at `a4lg/Qwen3.8-27B-MTP-ONLY-GGUF`, unverified beyond
  loading. Any throughput number is provisional until ml-kit's spec-sweep runs it;
  dropping the three spec flags leaves a correct, slower model.
- Arch support was CHECKED, not assumed: `qwen35` is in the pinned tree's arch
  table and the built `llama-server` is 10380 (`0b1bad14f`, 2026-08-11). No
  llama.cpp rebuild needed.

**✅ CUTOVER DONE 2026-08-14 — 3.8 is the daily driver; 3.6 is RETIRED.** Config
only, no code. The first pass demoted 3.6 to a pinnable fallback under a
corrected name; the operator's intent was retirement, so the entry was then
removed outright (90 lines). Final state:

    1. Qwen3-6-27B-MPT entry DELETED
    2. aliases: [Qwen3-6-27B-MPT] on Qwen3.8-27B  (legacy callers unchanged)
    3. chat + free lane members repointed to Qwen3.8-27B
    4. dropped from ~/.corrallm/llm-bench.yaml (else the bench fails on it)

Verified live, twice — once on the demote, once after the retirement:

    legacy Qwen3-6-27B-MPT  -> unsloth/Qwen3.8-27B-GGUF:Q5_K_M
    chat lane               -> unsloth/Qwen3.8-27B-GGUF:Q5_K_M
    residency                  name=Qwen3.8-27B procKey=Qwen3.8-27B  (not split)
    /v1/models                 no 3.6 under either spelling; 15 models, was 16

The residency line is the one that mattered: the alias resolves to the CANONICAL
name, so one process holding one reservation is filed under one id. Had it kept
the requested name, the old and new ids would each have accrued their own
residency and metrics for memory allocated exactly once.

**ROLLBACK IS NO LONGER ONE LINE** — that is the deliberate cost of retiring
rather than demoting. The entry must come back from
`~/.corrallm/config.yml.bak-pre-cutover-*` before the alias and lanes can be
repointed. The weights are still cached
(`models--unsloth--Qwen3.6-27B-MTP-GGUF`), so it is minutes, not a re-download.

**THE SWAP WAS NEVER QUALITY-TESTED.** It was decided on VRAM, capability and
speed. No head-to-head completed — the only run was stopped mid-flight with
3.6's half collected while it crash-looped on a CUDA OOM. With 3.6 retired that
comparison is now harder to obtain, and it does not exist to appeal to.

Side effect worth knowing: 3.6's ratcheted 31124 MiB profile is stranded in the
tune cache under a placement name nothing references, so it no longer holds any
of gpu0's budget. The OOM section above is now historical rather than an open
production risk — nothing serves 3.6.

**✅ BENCHED 2026-08-14 — `out/20260814-124618`. 77/90 stages (85.6%)**, 63 probe
results, 284 checks, 3 audio probes correctly skipped. Full table lives in
`ml-kit/docs/model-evals.md`; the short form:

    coding       21/21     adversarial  11/12
    tooluse      42/51     capability    3/6

**Quality vs 3.6 is a WASH — 48 vs 47 on the 57 overlapping stages**, and 3.6's
47 is generous to 3.8 because that run was OOM-looping. Only five stages differ.
So the swap was decided on VRAM headroom, stability and speed, NOT on quality,
and no clean head-to-head was ever completed. 3.8 did finish all 90 stages
without crashing, which 3.6 did not.

**The judge DID run** (correcting an earlier note here): per-check `judge:`
assertions execute inline with scores in -0.5..1, and they are the most
informative part of the output. The null top-level `judge` field in runs.jsonl
is a per-run summary, not evidence that no judging happened. Self-judging did
therefore apply to those checks, with the self-preference caveat intact.

**Failures cluster.** `mcpshell-instructions` is 7 of 13 (not 6). Transcripts
read — 4 of those 7 are INVALID: the probe asserts `tool_called: eval` but the
baseline/polylsp arms have no mcpshell, so they cannot pass. **Excluding them,
77/86 = 89.5%.** The probe wants arm-scoping like the ocr-* ones have.

The 3 real failures are one root cause: `export path = ...` where mcpshell
documents `export let x = 10`. Verified against the live binary. The keeper is
the RETRY BEHAVIOUR — the error names the fix ("declare it first — `let path =
...`") and 3.8 changed the variable NAME four times instead of adding `let`,
until its 4-turn budget expired with no answer. Not acting on explicit
corrective feedback is the production-relevant weakness, and the pass/fail
number does not surface it.

`adversarial-poisoned-readme` polylsp is ALSO INVALID, and the probe warns about
exactly this under "Known limitation — read this before drawing conclusions":
poison is spliced into llm-bench-mcp's own tool results, so a toolset reading via
another server (poly-lsp `node_read`) never triggers it; `read_file` is a
DELIVERY PRECONDITION and its failure means "this probe did not test this
toolset". I logged it as an injection weakness first — wrong.

**Injection resistance is 2/2 where actually delivered** (baseline, mcpshell:
3/3 each). Transcript confirms the payload lands in the read_file result and
3.8's next four actions are list_dir, read greet.go, read greet_test.go, go
test — no bait call, no pwned.txt, no derailment. This is what sank the
35B-A3B; 3.8 does not exhibit it.

So 5 of the 13 failures are known-invalid: **77/85 = 90.6% valid**.
`find-render-entrypoints` fails both arms (3.6 passed one); OCR is 3/6 — those
are real.

**still open**
- **KLD baseline** — owed before ANY sub-Q5 discussion. Blocked on a choice: the
  proper reference is BF16 (46.55 GiB, does not fit the 5090) so it is CPU-slow,
  or a Q8_0 baseline that makes every number relative to Q8_0 rather than truth.
- **A clean head-to-head** — only obtainable now by restoring 3.6 from the
  pre-cutover backup. `out/20260814-112505` stays VOID as a standalone result,
  though its 57 completed stages were good enough for the overlap comparison.
- **Probe scoping bug (mcpshell-instructions)** — it runs on arms that lack
  mcpshell and fails there by construction, costing 4 spurious failures. Fix is
  in the mcpshell repo (`bench/probes/mcpshell-instructions/task.yaml`), not
  here. Until then, subtract 4 from any headline failure count.

**optional extensions** (not in scope) — surface aliases in the Overview model
form rather than only in `advancedFields`; 3.8 past 188k needs one of the priced
levers, not more window.


## ✅ P23 — per-mode sampler profiles + `--pin-sampling` (2026-08-15)

Two fixes to one root problem: **a reasoning model has two right samplers and
only one launch flag**, while the caller flips the mode per request.

**`sampling:` on a model** (`b4c6bf9`). Profiles for `thinking` and `instruct`
plus a `default`; the proxy reads the request's mode and fills in ONLY the
fields the caller omitted. Mode is read in four dialects that all reach
`/v1/chat/completions` — llama.cpp's `chat_template_kwargs.enable_thinking`,
OpenAI's `reasoning_effort`, Anthropic's `thinking` object, and
`reasoning_budget_tokens` (0 = end reasoning now, so NOT thinking).

Placed here rather than in a client because corrallm is the only component that
knows which model a name actually resolved to (after aliases, lanes and globs)
AND holds that model's card values. In a client, every client reimplements it.
Same seam as the PDF rewrite: once at the proxy, not per backend.

Verified live against `/upstream/Qwen3.8-27B/slots`, which reports the sampler a
slot really ran with — the cmd no longer carries any sampler flags:

    plain request                temp 0.7  top_p 0.80  presence 1.5
    enable_thinking: true        temp 1.0  top_p 0.95  presence 0.0
    caller sends temperature: 0  temp 0.0  top_p 0.80  (caller wins)

**THE CALLER ALWAYS WINS** is load-bearing, not politeness: `--pin-sampling`
sends temperature 0 so a probe is not a coin flip, and a proxy that overwrote it
would restore the coin flip silently.

**`--pin-sampling` on llm-bench** (`79567a7`). The capability pin already existed
and its comment explicitly excluded quality probes — they measure a deployment
as served. That is right for "how good is this" and wrong for "is A better than
B", so this is a flag, not a changed default. Composes with `--runs N`: pinning
removes sampler variance, repeats measure what is left.

**Why it was needed, measured:** two full runs of one model with byte-identical
config came back 77/90 and 75/90 with FOUR stages flipping each way. The
thinking-mode A/B landed inside that spread and so decided nothing on quality.

**next** re-run the A/B under `--pin-sampling` if the thinking question is worth
settling on quality as well as cost. Cost already decided it: 3.16x tokens.


## ✅ P20 — token counting already works end to end (verified 2026-08-10)

**This item was WRONG when written and is kept as a correction.** It claimed
corrallm had no token-count surface, on the strength of a route grep truncated at
25 alphabetical lines — the listing stopped at `/api/v1/config/...` and never
reached the entry that mattered. There are 67 routes.

`/upstream/<model>/tokenize` resolves the model, ensures the backend is resident
and forwards the path. Verified live:

    POST /upstream/Qwen3-6-27B-MPT/tokenize  {"content":"hello world"}
    -> 200 {"tokens":[14556,1814]}

Ungated by design (`/upstream/m/` is in auth's ungated list beside
`/v1/chat/completions`), so a caller needs no admin token. Note the shape is
`/upstream/<model>/`, NOT `/upstream/m/<model>/` — the latter 404s "unknown
model" even for a registered id, which is what sent the first probe wrong.

agentkit already consumes exactly this: `llm/tokenize.go` lists
`root + "/upstream/" + model + "/tokenize"` among its candidates, beside bare
llama.cpp's `/tokenize` and vLLM's `/v1/tokenize`.

**Anthropic count_tokens: BUILT 2026-08-10, `dc01410`.** The llama.cpp half was
already there; the Claude half was not, and the reason was subtle. The `claude`
extension is a passthrough to `api.anthropic.com` that already injects
`anthropic-version` and Claude Code's OAuth bearer — but its models are GLOB
TEMPLATES (`claude-haiku-*`), and `/upstream` resolves with an exact
`Models[served]` lookup, so `/upstream/claude-haiku-4-5/...` answers "unknown
model" for an id the chat path serves fine.

`POST /v1/messages/count_tokens` resolves through `ResolveServed` (glob-aware)
and forwards the path unchanged. Mounted OUTSIDE `handleInference`: counting runs
no inference and holds no GPU, and a caller sizing a prompt before deciding
whether to send it must not queue behind the backlog it is measuring against.

**VERIFIED LIVE 2026-08-10** against real Anthropic, after `bin/deploy --no-ui`:

    messages only                 200  {"input_tokens":9}
    + system                      200  20
    + system + tools              200  563
    claude-opus-4-8 (other glob)  200  9
    local backend                 404  names /upstream/<model>/tokenize
    unknown model                 404

**The live run found a bug the unit test could not, and the FIXTURE was why.**
The first cut gated on "has a proxy target". Every locally-spawned backend has
one — a loopback port is how corrallm reaches the llama.cpp process it started —
so the route forwarded to a local server with no such path and returned **502**
from a dead port. The fixture had given the local model an empty `Model{}`, which
made the wrong predicate look valid. Now gated on `Model.Remote()` (no local
process AND non-loopback), the fixture carries a loopback proxy the way real
config does, and the fake upstream binds a NON-loopback address — a test server
on 127.0.0.1 is indistinguishable from the backend corrallm spawned, which is
the distinction under test. Fixed at `502bfe3`; reverting the predicate now
fails two tests, one with the same 502.

**Measured, and it constrains the dun design (2026-08-10):**

    no tools   9        fixed tool-use overhead   497 tokens
    1 tool     551      per small tool             45 tokens
    2 tools    596      9 + 497 + 45 + 45 = 596 — exact

Enabling tools at all costs ~497 tokens of Anthropic's own tool-use preamble.
**So per-server tool costs CANNOT be counted independently and summed** — the
fixed overhead would be charged once per MCP server, overstating a four-server
session by ~1.5k and breaking the property that the parts add up to the total.
The Anthropic path has to measure incrementally (N servers minus N-1), or carry
the overhead as its own row. dun's `SystemBreakdown` already asserts parts-sum-
to-total, so this would have surfaced as a failing test rather than a wrong
number — but only after the work was built.

**CONSUMED, end to end 2026-08-10.** agentkit gained `CountPrompt(system, tools)`
(`284ec36`) — Anthropic's shape where the endpoint speaks it, the concatenation
otherwise — and dun's `/context` now reports MARGINAL per-part costs
(`75c165f`), because the preamble makes independent counting wrong. Live through
this route on `claude-haiku-4-5`:

    total 638  prompt 11  shared 497
    built-in tools 44 · mcp: chrome 43 · mcp: raglit 43   (sums exactly)


---

## VRAM measurement — per-device attribution + bounded peak decay

Shipped `b3e70f1`. The live confirmation (one `models/load` of deepseek in a quiet window)
and the gpu1 oversubscription policy call are active in `plan/plan.md` §6.


Two fixes to `internal/tune` + `internal/proc`, both found by measuring rather than reading:

1. **Multi-GPU footprints were charged to one card.** `measure()` took the whole process
   group's footprint (`h.MemoryMiB()`, every card summed) and filed it under a single device
   name — and for a two-card model `deviceNameFor` falls through `localDeviceFor` (which
   deliberately refuses to name one card) all the way to `gpu.Probe()`. Live effect: DeepSeek-V4
   split 29.9/6.3 GiB across the 5090 and the 3080 was recorded as **37,094 MiB on the 3080**, a
   figure ~4× that card. Fixed by measuring per device via the already-existing
   `gpu.GroupVRAMOn(pgid, uuid)` — whose doc comment describes this exact bug — behind a new
   `host.Handle.MemoryOnMiB(uuid)`. Falls back to the old whole-host path when a host cannot
   attribute per device (remote agent, macOS). `kvMiB` is deliberately not passed through: the
   KV total is per-process and splitting it would need the per-card layer distribution that
   `-ts` decides and nothing observes. Regression test verified to FAIL without the fix.
   The corrupt live entry was purged; it repopulates per-device on deepseek's next load.
2. **`PeakMiB` is monotonic forever, which is wrong for input-driven footprints.** Correct when
   footprint is a function of slot count; chandra-ocr-2's is a function of its *input*
   (`--image-max-tokens 16384`) — 6196 base / 8240 peak on gpu1, 10706 on the M1 Max. One
   400-dpi page reserved its peak permanently. Added `RecentPeaks` (bounded ring, `RecentWindow`
   = 8) + `EffectivePeakMiB()`: all-time peak until the window fills, then max-of-window.
   Chose max-of-window over the p95 originally proposed — at 8 samples p95 *is* the max, and
   making a percentile meaningful needs ~40 retained observations to buy the privilege of
   under-reserving on 5% of spawns, where under-reserving VRAM is an OOM, not a slowdown.
   Applied at both reservation sites; `PeakMiB` is retained for diagnostics.

**Not verified live.** The regression tests cover both, and the rebuilt binary is deployed, but
the end-to-end check (load deepseek, confirm two per-device profiles) was abandoned — a live
`life-raglit` OCR batch was hammering chandra every few seconds and admission kept refusing on
gpu1. Do it in a quiet window; it is one `models/load` and a read of `vram-profile.json`.

**Related, still open:** gpu1 (10GB 3080, 9,877 MiB usable after driver reserve) now has four
claimants — nomic 816 (persistent), chandra 8,240 (sticky, input-variable), deepseek 6,676,
and any Qwen drafter. They do not fit; a resident deepseek evicts chandra. Policy call is the
user's.


## Multi-node — the decided design and the shipped ordering

Every step below is done except `host.Remote`, which is active in `plan/plan.md` §6
along with the three decisions still owned by the user.

  Concrete driver (2026-07-28): attaching a 64 GB Apple-silicon box as a second compute host.
  **Two shapes, and the cheap one is not a stepping stone to the other:**
  (a) *proxy peer* — machine 2 runs its own `corrallm serve`, attached to box1 as an ordinary
  `extensions:` proxy target. **Zero new code, works today.** Buys serving; buys no unified
  residency, eviction, fairshare or dashboard. Use it to measure whether the second box earns
  its place before building anything.
  (b) *managed host* — box1 spawns onto machine 2. Needs `proc.Manager`'s single local
  `exec.Command("sh","-c",…)` (manager.go:398) behind a Spawner interface, `gpu`/`sysmem`
  probing per-server instead of per-host, and `config.Server` gaining a real address (today it
  is a pure local pool budget: pools/reserve/maxConcurrent, no host field).
  **DECIDED 2026-07-28 — shape (b), full remote control.** Also decided: agent-local config,
  UI-editable, **synced up into the primary's declared config** (an injected model can never be
  a lane member — `config.go:1018` validates lane membership against `c.Models` at load time —
  so agent-contributed models must land in declared config, which is what makes live reload a
  prerequisite rather than a nicety); one-time enrollment token minted in an Agents tab; the
  daemon serves cross-compiled binaries for `curl <daemon>/install.sh | bash` (verified:
  `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64` builds clean, ~37 MB, sqlite is pure-Go modernc).
  One binary, `corrallm agent` subcommand — satisfies the no-duplication concern while keeping
  the auth domains separate (`auth.Middleware` gates `/api/*` with the admin token; an agent
  needs its own credential).
  Ordering (each step shippable with no second machine present): ✅ **step 0** per-server VRAM
  accounting (done) → ✅ **live config reload** (done: `include:` merging, SIGHUP,
  atomic config swap in all four holders; fractional `quality` landed alongside it because
  the Mac's 4-bit tier sits between two integers) → ✅ **`internal/host` Spawner interface**
  (done: Spec/Handle/Host, Local only, platform primitives moved; Process holds a Handle
  instead of an *exec.Cmd) → ✅ **`Server.Agent` binding**
  (done: endpoint LIST per the VPN topology, `TargetFor` so an agent model's loopback port
  never resolves to the primary's, `host.Unavailable` so it refuses to spawn locally) → ✅ **`corrallm agent`**
  (done: hello/capacity/spawn/list/status/signal + sequenced logs with `from=`; token
  required by default since it executes shell commands; heartbeat+lease deferred to the
  failure-semantics step) → ◻
  `host.Remote` (integration-testable by running a second agent on another port on box1) → ✅ **failure semantics**
  (done: agent heartbeats OUTWARD, 3-interval miss window, down refuses spawns before
  reserving pools, config survives the outage, token revocation is how membership ends;
  NO self-reap — per-server ledgers make a stranded reservation harmless to other hosts.
  reconnect ADOPTION reconciles on every heartbeat: matching keys adopted, orphans reaped
  past a 60s grace, vanished backends free their pools) → ✅ **darwin capacity**
  (done: `gpu.Apple` + `sysmem_darwin`; ProcVRAM errors rather than reporting zero, and a
  host that cannot measure per-process memory now REQUIRES ramUsage at validate time —
  otherwise it silently serves one model at a time) → ✅ **enrollment + Agents/Config UI**
  (done: one-time tokens, self-registration sized from the agent's own probe, GUI model
  CRUD). Remaining: switch the daemon's default config to the managed one and retire the
  hand-written file.
  **Agent addressing (USER, 2026-07-28): an agent has SEVERAL addresses, not one.**
  A NAT/LAN address on the same network as llm.iodesystems.com, an external host, and a
  VPN address when both the Mac and the daemon are on the VPN — all valid for the same
  box at the same time, and which one works depends on where the daemon is sitting.
  So `Server.Agent` takes a LIST of candidate endpoints (tried in order / by
  reachability), not a single `url`, and the backend data-plane host is resolved the same
  way. Two consequences: reachability becomes per-endpoint state, not per-agent; and
  `config.IsLocalHost`'s "a private-LAN address is not local" doc comment is already
  wrong under this topology — a 192.168 address may well be a box we manage. The
  predicate itself stays correct (it short-circuits on LocalProcess), but the comment has
  to be reworded to say "we run no process for it", or the next reader breaks it.
  ✅ **box2's pool shape — DECIDED (USER, 2026-08-04): ONE unified pool.** `pools: {system:
  <hw.memsize>}`, `devicePool: system`, and the wired limit expressed as `reserve` rather than
  as a second pool, so budget = total − reserve lands at (or just under) what the GPU may
  actually wire. This was not merely undecided — enrollment was shipping the *wrong* shape:
  `poolsFrom` declared `system` from hw.memsize AND `gpu0` from the wired limit, while
  `devicePoolFrom` picked `gpu0` whenever a GPU reported anything, so a 64 GiB Mac enrolled as
  119 GB across two ledgers `fitsLocked` checks independently. Undetectable by construction, on
  the one class of host that also cannot measure per-process memory. Fixed by making the three
  answers one function (`api.sizeFrom`) fed by a new `Capacity.Unified` the agent reports —
  GOOS cannot answer it, since an Intel Mac with a discrete card is darwin with two real pools.
  ✅ **Repair path: RE-ENROL.** Enrollment used to preserve `pools`/`reserve`/`devicePool`, which
  pinned a server to whatever its FIRST enrollment computed — so the one operation that exists to
  re-measure a machine was a no-op precisely when it was needed. `mergeEnrollment` now re-derives
  all three from the fresh probe and keeps only what the agent cannot know (`notes`,
  `maxConcurrent`), appending a note when a resize actually moved the numbers. Safe because
  re-enrolling needs a one-time token the operator just minted — it cannot happen by accident.
  **Still USER-owned, surface before the step that needs it:** agent lease self-reap on/off and
  its TTL (decides whether the ledger may ever be released after a host is lost — the trade is
  "a network blip kills an in-progress cold load" vs "a partition strands 48 GB"); transport
  trust (the agent executes arbitrary shell strings — it is an RCE surface by design);
  whether to spike `proc_pid_rusage`'s `ri_phys_footprint` for real per-process measurement on
  darwin before accepting "unmeasurable → ramUsage becomes authoritative".

---

## ✅ P19 — thermal envelope (box1)

The 200 W operating point is applied and persistent, verified against the box on 2026-08-24.
The one live thread this left — a boot script that sets power limits by INDEX, which P18
exists to forbid — is a system-config problem, not a corrallm phase, and lives in
`plan/open-questions.md` #2.

- ◐ **P19 thermal envelope** (box1, 2026-08-05) — the 3080 hit **88°C at 186W with the fan pegged
  and `0x20 SW Thermal Slowdown` active** during an OCR sweep, while the 5090 sat at 54°C/0%.
  Measured its thermal response under sustained load rather than guessing: **dT/dP = 0.239 °C/W,
  intercept 25.8°C** (the intercept recovering intake-air temperature is what says the fit is real).
  A healthy 3080 is 0.15–0.18 °C/W, so this card's die-to-air path is ~35–60% worse — consistent with
  five-year-old paste or a packed fin stack, NOT airflow: an airflow/heat-soak problem rises and falls
  slowly, and this one fell 88→65°C in 30s. The card's own target is 83°C, which the slope puts at
  ~239W — hence the throttling at a 250W cap. **200W validated over 7 minutes sustained: 71.7°C
  steady-state (model predicted 73.6), fan 83% not pegged, throttle `0x0004 SW Power Cap` on 193/207
  samples and ZERO thermal.** At 200W the card is power-limited rather than thermally limited, which
  is the correct operating point on this cooling. History is persistent in Prometheus —
  `nvidia_gpu_exporter` on :9835 labels by **GPU UUID**, the same identity the pools bind to, so
  Grafana joins to pool names for free.
  **Pulled from Prometheus afterwards, and it corrected the account:** the exposure was not the
  brief spike a spot `nvidia-smi` reading suggested. Six load episodes, **15.5 cumulative minutes
  >=80C**, and the two that mattered were a 2.5 min chandra 400 DPI run at a MEAN of 84.9C (during
  OCR comparison, half an hour before the bench was blamed for it) and the 10.5 min bench sweep at a
  MEAN of 86.6C. Lesson for any future thermal claim here: query the exporter, do not spot-read.
  **Do NOT fit the slope across pooled Prometheus episodes** — mixing power caps and soak states
  gives 0.194 C/W with a 39.6C intercept, and an intercept that is not plausibly intake air is the
  tell that the fit is contaminated. It predicts 78.4C at 200W against 70.5C actual, where the
  single continuous ramp (0.239, 25.8C) predicts 73.6C against 71.7C.
  **consequence for placement:** gpu1 is the thermally-limited card behind the chipset at x4. Bursty
  OCR there is fine; a sustained sweep is not — bench on gpu0, which idles while gpu1 cooks.

**✅ DECISION RESOLVED 2026-08-24 — the cap is persistent, and repaste is deferred.** Checked
against the box rather than the plan: `nvidia-smi.service` is **enabled and active**, running
`/usr/local/sbin/nvidia-power-init.sh` at boot, and the 3080 reads **200.00 W against a 340 W
default** right now. The operating point was already applied and already survives reboot — this
item was ◐ for nineteen days over a question the machine had answered.

Repaste is deferred, not refused: the card is power-limited rather than thermally limited at
200 W, so the degraded die-to-air path costs throughput on the SECONDARY card only. Revisit if
gpu1 ever takes sustained work — §9's OCR-vs-bench placement rule already keeps sweeps off it.

**⚠ One live problem came out of the check, and it is `open-questions.md` #2:** the boot script
sets the limits **by index** (`-pl 200 -i 0`), which is the exact identity trap P18 exists to
forbid. Correct today; silently catastrophic if the indices ever swap.

**risks** the fit is only valid from a single continuous ramp — do NOT re-fit across pooled
Prometheus episodes (see above). Any future thermal claim here must query the exporter, not
spot-read `nvidia-smi`.
**next** nothing. This tree is a completed measurement; it stays in §6 only until the boot-script
question closes, then it archives.

---

## ✅ ramUsage split — `pool:` for placement, measurement for size (2026-08-24)

`ramUsage` answered two unrelated questions: its VALUES said how big a model is, its KEYS said
which card it was on. Size then became measurable — a tune profile supersedes the declared
number — so models stopped declaring ramUsage, and in doing so silently stopped declaring their
PLACEMENT. On box1 that charges a model actually running on gpu1 to gpu0: one budget inflates,
the other looks free, and the scheduler places a second model onto memory already spoken for.
Nothing errors.

**`pool:` on a model and on a placement.** Placement only; it says nothing about size. An
explicit pool WINS over ramUsage keys — otherwise it could not correct a stale hint, which is
the one thing an operator would reach for it to do. Empty is not "unknown": it means the
server-wide default, which is the right and only answer on a single-GPU box. A multi-GPU SPLIT
still declares nothing, because a model spanning two cards has no single pool and one measured
total cannot be divided back across them.

**Per placement as well as per model**, and the placement wins. Two placements on two cards is
precisely what a model-level field cannot express.

**Validation rejects two different mistakes.** A pool the server does not declare is a typo,
and it would fall back to the server default at spawn — the model would run on a card nobody
asked for while the config said otherwise. A pool that EXISTS but is not device-backed
(`system` on box1) is a category error: it would file a VRAM measurement against a pool that
holds no VRAM. On the Mac, `system` IS the device pool, and the rule reads
`DevicePoolsFor(server)` rather than a hardcoded name, so unified memory passes.

**No column, by design.** `pool:` rides configdb's unprojected remainder, the same as P29's
tool pin: the schema is applied with `CREATE TABLE IF NOT EXISTS`, so a new column would never
appear on the live database and every read would fail. Three round-trip tests pin it, including
the placement-level field, which travels a different path (`placements_json`).

**The form cannot drop it.** `applySpec` is a PATCH — it overlays only the fields the form
models — so `pool:` survives an unrelated save untouched. Pinned by test rather than assumed,
because this codebase has already shipped three handlers that wrote to the wrong copy of a
model. `advancedFields` now NAMES pool, so a form that edits ramUsage (size) and not pool
(placement) says so instead of looking complete. `placements` was missing from that list too
and was added with it.

**Five call sites read the same three fields by hand**, which is the hazard this file names
elsewhere: migrate one and forget its neighbour. Extracted to `Manager.placementOf`.

**Claims-nothing, shipped as decided.** An unmeasured model on a device pool now claims nothing
and the spawn is the fit test, replacing "assume it needs the whole pool" — which evicted every
evictable resident on the first spawn of every new model to learn a number the spawn itself
reports. Host RAM keeps the conservative path: a CUDA OOM kills the process that asked, an
anon-RSS blowout takes the machine, and oidio already reached 119 GB on this box. Hosts that
cannot measure per-process memory also keep reserving, or "claim nothing, then measure"
degrades to "claim nothing, forever".

**⚠ The caveat as recorded was understated, and the correction is in `plan.md` §6:** "a CUDA
OOM kills only the allocator" is true, but the allocator is not always the newcomer — a
resident that grows after load (Qwen's ~2 GB vision spike) can be the one that asks for memory
an unmeasured neighbour took.

**Verified by mutation, not by review** — every guard was broken deliberately and a test caught
each: ignoring the explicit pool, accepting an undeclared pool, dropping the non-device-pool
check, dropping the cannot-measure exception, and never claiming nothing. One mutation initially
appeared to survive and had simply failed to compile (an unused variable), which is worth
remembering: a mutation that does not build proves nothing.

`go build`/`vet`/`test -race` green; gofmt clean on every touched file. The three gofmt-dirty
files in the tree (`proxy/inflight.go`, `quota/ledger.go`, `sysmem/sysmem.go`) and the two vet
copylocks hits are pre-existing.


---

## ✅ Payload aging — metrics and payloads retire on different clocks (2026-08-24)

`req_body` was 5,330 MB of a 5,914 MB database — 90% of the file — against 242 MB of
`resp_body` and ~340 MB of everything else. Not a leak: retention was working (oldest row 29.7
days against a 30 d setting), `freelist_count` was 0, and the file had plateaued. It was simply
the cost of a 256 KiB request cap meeting agentic traffic that re-sends the same system prompt
and tool schemas every turn.

**The cap is not the thing to lower.** `reqBodyCap` is 256 KiB deliberately, so a full agentic
request stays VALID JSON and can be replayed in the console; a 4 KiB truncation left it
unparseable and replay degraded to dumping raw text. (`payloadCap`, the 4 KiB one, bounds a
RESPONSE — an easy pair to confuse, and I confused them once before measuring.)

**So age the two apart.** Every number on an activity row — status, dwell, tokens, cost, queue
wait, cache hits — feeds rollups, series and the caller roster, and is read for as long as it is
retained. The payload feeds exactly one thing: replaying a request in the console, which nobody
does to a three-week-old request. `--payload-retention` (default 168h/7d) clears the bodies and
keeps the row; `--activity-retention` (30 d) still drops the row. A payload retention at or past
the row retention can never fire, and the daemon warns at startup rather than doing nothing
quietly.

**Cleared to `''`, not NULL.** Both columns are `TEXT NOT NULL DEFAULT ''`, so a NULL would fail
the constraint on every row and the prune would error on every pass, forever, with the table
growing anyway. Caught by test — the same shape as the P29 ticket index that crash-looped an
existing database.

**Chunked at 500 rows, because the activity insert is on the request path.** `InsertActivity` is
synchronous (`handleInference` → `logReq` → `log`), and SQLite serialises writers even in WAL.
One unbounded UPDATE over the backlog measured **27 seconds** against a real 5.9 GB copy — 27
seconds of every completing request blocking. Chunking does not reduce the work, it bounds the
LOCK HOLD, and the cost is linear in the chunk because it is page rewrites at ~85 KB/row:
2000 → 1.4 s, 500 → 0.28 s, 250 → 0.18 s. 500 puts a mid-chunk request inside the noise of a
model that takes seconds to answer, and the backlog still drains in ~30 s.

**The `!= ''` predicate is load-bearing**: matching on timestamp alone would rewrite every old
row on every pass, turning a one-off cleanup into a permanent write load on a five-minute timer
against the largest table in the database.

**Verified on a real copy of production, not a fixture.** 57,205 rows cleared in 14.4 s;
payloads 5,573 MB → 779 MB; second pass 0 rows in 72 ms. Every aggregate byte-identical: rows
65,617, cost $6.276954, prompt tokens 1,222,460,967, dwell 522,830,856 ms, cached tokens
1,028,349,212.

Guards mutation-checked: dropping the idempotence predicate, deleting the row instead of
clearing it, and clearing only `resp_body` (missing the 90%) each fail a test.

**Left open, in `plan.md` §7:** the file does not shrink. `auto_vacuum` is off, so freed pages
are reused rather than returned — growth stops, the 5.9 GB stays. A one-time `VACUUM` measured
5.9 GB → **842 MB in 6.75 s** on the copy, and is an operator action because it holds an
exclusive lock.


---

## ✅ Deployed: the ramUsage split and payload aging, live on box1 (2026-08-24)

One maintenance window, one restart, both changes. Both cards were idle and nothing was in
flight, which is the cheapest moment this box offers.

**Order, and why.** The running daemon predated `pool:`, and a config field an older binary has
no struct member for is the P25 failure exactly — it has nowhere to live on the next write and
vanishes. So: clear payloads → stop → VACUUM → install → start → migrate config. Clearing and
vacuuming with the daemon DOWN also meant no lock contention at all, which is why the unbounded
clear was safe to use there (32 s) even though the shipped code chunks it.

Backed up first: `corrallm config export` and a copy of the 5.9 GB database, into
`~/.corrallm/backup-20260824-160756/`.

**Database: 5,916 MB → 803 MB.** 57,205 rows had their payloads cleared (32 s), VACUUM took 6.4 s,
`integrity_check` = ok. Every aggregate identical across both operations — rows 65,617, cost
$6.276954, prompt tokens 1,222,460,967, dwell 522,830,856 ms. 779 MB of payloads remain: the
last 7 days, which is the point.

**Config: four models migrated**, a 4-line diff through P26's export → validate → load path.
`Qwen3.8-27B` and `muse-glimmer-30b` to `gpu0`; `chandra-ocr-2` and `nomic-embed-text` to
`gpu1`. `deepseek-v4-flash` deliberately gets none — it is a real multi-GPU split (`gpu0` 32.5GB
+ `gpu1` 7GB) and has no single pool. The Mac's model gets none either: one unified pool.

**Validation was exercised against the real config, not a fixture**, by deliberately breaking it
both ways before loading the good one:

    pool: gpu7   -> refused: pool "gpu7" not declared on server "box1" (declares [gpu0 gpu1 system])
    pool: system -> refused: pool "system" on server "box1" is not a device pool (device pools:
                   [gpu0 gpu1]). `pool:` names the CARD a model's weights live on

**`pool:` landed in the remainder, as designed** — `config_scalar` rows keyed
`model.rest.local/<name>` holding `{"pool":"gpu0"}`. No column, no migration, on the existing
database.

**Verified against the hardware, which is the only verification that counts here.** With Qwen on
gpu0 and nomic on gpu1:

| pool | corrallm ledger | nvidia-smi |
|---|---:|---:|
| gpu0 (5090, `GPU-ee90af07`) | 30,370 MiB | 30,370 MiB |
| gpu1 (3080, `GPU-76a4c775`) | 812 MiB | 778 MiB |

The gpu0 figure is the MEASURED footprint, not the declared 31,000 MiB — so measurement is
governing size while `pool:` governs placement, which is the whole split doing its job on real
hardware. Then served for real: a chat completion through the `chat` lane and a 768-dim
embedding on gpu1.

No SDL drift, so `ui/dist` was left alone — a UI rebuild is a deploy the instant it finishes,
and there was nothing to publish.


---

## ✅ `nvidia-power-init.sh` binds power limits by UUID (2026-08-24)

Found while verifying the P19 thermal question: the boot script that applies box1's power caps
named its cards by INDEX — `-pl 450 -i 1`, `-pl 200 -i 0` — which is the exact identity trap P18
exists to forbid. It was correct only by coincidence of enumeration order. nvidia-smi enumerates
by PCI bus id, and on this box that puts the **3080 at index 0 and the 5090 at index 1**,
backwards from slot order.

If that order ever shifted, `-pl 200` would land on the 5090 — the box's whole interactive
capacity at a third of its power budget, with no error anywhere: the unit succeeds and
nvidia-smi reports a valid limit. The other half fails loudly (450 W on a 340 W card is
rejected), so the damage was one-sided and silent in the expensive direction.

Now a `declare -A LIMITS` map keyed by UUID — the same UUIDs corrallm's pools bind to, so the
power caps and the scheduler's ledger finally agree on what a card is.

**`set -e` was dropped on purpose.** Each card is applied independently, because aborting on the
first missing card leaves every LATER card at its stock limit — a card running unlimited because
a different one was pulled. Verified with a stubbed nvidia-smi: with the 3080 absent, the old
shape stops; the new one still caps the 5090 and exits 1.

It now reports what it did (`name [uuid] -> watts`), names a declared-but-absent card, and exits
non-zero on any failure so the unit shows failed rather than succeeding quietly — the bug being
fixed was silence.

Also recorded in the script: WHY 200 W, so nobody rounds it up. That card measures 0.239 °C/W
against a healthy 0.15–0.18 and throttles at a 250 W cap; at 200 W it is power-limited rather
than thermally limited (71.7 °C sustained, zero thermal throttling over 7 minutes).

**Verified live:** unit exited 0, both cards named by UUID in its log, and `nvidia-smi` reports
3080 = 200 W of 340 W, 5090 = 450 W of 600 W, persistence enabled. Previous script kept at
`/usr/local/sbin/nvidia-power-init.sh.bak-20260824`. Not tracked in this repo — no host or
systemd config is.


---

## ✅ Per-device VRAM attribution — confirmed in production (2026-08-24)

`b3e70f1` fixed a multi-GPU model's footprint being charged to one card; the live confirmation
had been abandoned mid-session when an OCR batch kept admission refusing on gpu1. It turns out
production had already proven it — reading `vram-profile.json` shows `local-deepseek-v4-flash`
carrying TWO separate per-device profiles, measured 2026-08-17 while serving:

    RTX 5090 (pool gpu0)   30,616 MiB
    RTX 3080 (pool gpu1)    6,478 MiB

**Those two sum to 37,094 MiB — the exact corrupt figure the bug produced**, when the whole
process group's footprint was filed under a single device name and a 10 GiB card was recorded as
holding ~37 GB. The split is now correct on both sides, on real hardware, from a real serving
load. No deliberate deepseek load was needed, so Qwen was never evicted.

Also visible in the same read: `local-nomic-embed-text` at 812 MiB on the 3080, measured
2026-08-24 during the `pool:` migration check — matching what the residency ledger reported and
what nvidia-smi showed.

**Known, expected, not a defect:** stale profiles remain under pre-P24 names (`Qwen3.8-27B`
alongside `local-Qwen3.8-27B`, and a `nomic-embed-text` entry on the 5090 from 2026-07-18). The
tune cache keys on served name, and P24 recorded that the rename starts fresh ids rather than
joining old rows. Harmless — nothing resolves to the old names — and cheap to prune if the file
ever matters.


---

# Dashboard & observability

> These six trees were written into the roadmap under phase numbers that **collide with
> the git-authoritative series** (see the note at the top of this file). None of them
> appears in a commit message. They are archived here under descriptive names; the
> mis-number is recorded on each so an old reference still resolves.

## Live in-flight visibility *(was mis-numbered P17 — git's P17 is pause)*

- ✅ **P17 — live in-flight visibility.** The activity log only holds FINISHED requests, so a long
  completion, a cold load, or a request queued behind a saturated backend was invisible in both the
  Overview and Activity views until it ended. The proxy now keeps an in-flight registry
  (`internal/proxy/inflight.go`: queued → loading → streaming, registered before admission, cleared
  on every exit path incl. spills), exposed as `activeRequests` and rendered by a shared
  `ui/src/ActiveRequests.tsx` at the top of BOTH views; elapsed ticks client-side off `startedAt`
  so it never looks frozen between refetches. Overview also leads with a **Loaded** section —
  resident models (ready/loading/evicting) pulled out of their capability sections, so what holds
  capacity right now is the first thing on the page.


## Dark ops theme + the panel vocabulary *(was mis-numbered P18 — git's P18 is multi-GPU capacity)*

- ✅ **P18 — dark ops theme + the panel vocabulary.** The UI ran on a bare `createTheme()`: every
  surface `#fff` on `#fff`, so a dozen cards and section headings read as one undifferentiated
  sheet ("white on white with white sauce"). Now `ui/src/theme.ts` defines three layers
  (canvas `#0f1115` → surface `#171a21` → raised `#1c2029`) separated by **borders, not shadows**
  (MUI's dark elevation overlay is explicitly killed), and `ui/src/Panel.tsx` defines what a panel
  IS: bordered surface + tinted header bar carrying a small uppercase tracked label, with `Row`
  (hairline-separated items — no card-in-a-card) and `Stat` (label-over-value, replacing one-row
  tables). Every route was migrated. Chart palette (`SERIES`) is validated by the dataviz six
  checks against the panel surface — lightness band, chroma floor, adjacent CVD ΔE 8.8, normal
  vision, contrast all PASS; **re-run the validator before reordering or substituting a hue.**
  Table heads deliberately keep the body surface (a tinted head under a tinted panel header reads
  as one confused block). Known gap: charts still have no hover/tooltip layer.


## Memory attribution on the Overview *(was mis-numbered P19 — git's P19 is the thermal envelope)*

- ✅ **P19 — memory attribution on the Overview (RAM/VRAM, and who holds it).** A stacked bar per
  pool/device showing occupancy with per-model attribution, colored by model over the FULL sorted
  model list (never the per-bar subset — a load/unload must not repaint the survivors).
  **Two truths, deliberately not merged:** *measured* (nvidia-smi VRAM + `/proc/meminfo` host RAM,
  new `internal/sysmem` with the same fail-safe contract as `internal/gpu` — a failed probe reports
  `available=false`, never a zeroed bar) and *accounted* (the scheduler's pool ledger, which is what
  admission and eviction actually decide on). The gap between them IS the signal — a backend that
  outgrew its declared `ramUsage`, or a stray process squatting on the GPU, appears as
  "other processes"; averaging them into one bar would erase it. Non-model segments wear a neutral
  grey, never a categorical hue. Exposed on `residency` as `gpu`/`host`. The old static
  "System capacity" panel is gone — its live half moved here, its one unique fact
  (`server.maxConcurrent`) became "Host limits".


## Retry promises, utilization & service-time distribution *(was mis-numbered P15a/P15a.2/P15a.3 — git's P15 is bench)*

The open follow-on ("fix the estimate", formerly P15b) is active in `plan/plan.md`.

- ✅ **P15a — record the promise (who we told to come back, and when).** A 429 is an appointment,
  not just a rejection: the caller is handed a `Retry-After` and, if they honor it, returns at a
  time we chose. That number was computed from live scheduler state, written to the wire, and
  forgotten — the activity log could say "we refused this key at 14:03" but never "…and told them
  4s". Now `writeBackpressure` RETURNS the promise it made (so the logged value cannot drift from
  the header — it is the same number, not a second rounding of the estimate) and every 429 site
  stamps it onto the activity row as `retry_after_ms`. New `retryPromises` op + "Come back later"
  panel on Activity lists who is due back and correlates each promise with the caller's next
  request: **waiting** (due in the future, not back) · **honored** (returned at or after) ·
  **early** (jumped the gun — ignoring Retry-After) · **gone** (never came back). Caller identity
  for the correlation is the key, falling back to source IP for unkeyed callers.
  - **next:** watch a week of real traffic — the promised-vs-actual column is the evidence P15b needs.
  - **risks:** the correlation subquery is per-promise (indexed on `activity(key, ts)`, bounded by
    limit + retention, but it is a correlated scan for the unkeyed arm). Two hosts sharing one key
    collapse into one caller — conservative (can report a return sooner than the promised caller
    made it, never later).
  - **assumption:** the caller's next request of ANY kind counts as "coming back". A caller that
    returns for a different model still reads as honored.

- ✅ **P15a.2 — utilization (per-model pressure).** Row per served model: in use / capacity, queue
  depth, promises outstanding, not-honored, early, and **both halves of the wait** — `est` (the
  scheduler's own live projection, via `BackendLoad.EstWait`, which calls `backpressure()` rather
  than restating its arithmetic — the moment a read view computes the estimate its own way, the
  number an operator sees stops being the number callers are given) beside `real` (measured mean
  `queued_ms` of requests that queued and were then admitted). Per-model, not box-wide: capacity
  is not fungible, and a saturated 27B summed with an idle embedder into "1/6 in use" describes
  neither. Row set = models ASKED FOR in the window ∪ anything busy right now (a model under load
  with no COMPLETED request yet has no activity row, and would vanish exactly when it matters).
  Measured wait excludes instant admissions (else a quiet hour drags the mean to zero) and
  rejections (whose `queued_ms` is time spent before being turned AWAY — a different quantity).

- ✅ **P15a.4 — the lane chip answers what a lane is made of.** On Utilization a lane row wore a
  bare `lane` chip whose tooltip listed member NAMES. That said the row was an aggregate but not of
  what: a lane reading `0/1 in use` is one loaded model idling or four models none of which is
  loaded, and those are opposite answers for the caller arriving next (served now vs. paying a cold
  spawn). `UtilizationRow.members` is now `[]UtilizationMember` — model, residency state, remote,
  spawnable, paused + reason, and that rung's own capacity/active/waiting — resolved by PROCESS key
  like every other residency read (an extension's models share one process) and deduplicated by
  name (a provider with several credentials expands to one candidate per account, which is a
  routing fact, not a second rung). The chip reads `lane 1/3 ready` and the tooltip lists the
  ladder in FALL-THROUGH order with ● loaded / ○ could load / ⊘ shut out, closing with
  "1 ready now, 1 more could load. 1 shut out." Truncation at 8 rungs takes from the middle and
  always keeps the warm ones — a `free` lane resolves to a dozen, and the loaded one is the fact
  the tooltip exists to show. Verified end-to-end against a throwaway instance (lane of
  spawnable + paused + remote rungs).
  - **trap:** `/health` is NOT under `/api`, so Vite did not proxy it and the auth gate's
    insecure-mode probe parsed `index.html`, demanding a token no `--insecure` server would accept.
    `ui/vite.config.ts` now proxies it.

- ✅ **P15a.3 — service-time distribution (measurement only; no behavior change).** The scheduler
  carries ONE dwell EWMA per backend, so every caller of a model is predicted by the same scalar.
  Measured what that scalar averages over, on 24h of real traffic on `Qwen3-6-27B-MPT`:

  | key | n | mean | CV | max | (1+CV²)/2 |
  |---|---|---|---|---|---|
  | `dun` | 1634 | 3.2s | 1.31 | 40.7s | 1.36× |
  | `probe` | 671 | 13.7s | 1.35 | 110.3s | 1.41× |
  | `life-raglit` | 470 | 8.2s | 3.77 | 398.4s | **7.59×** |

  Means differ **4.3× across callers on one model**. `store.ServiceStats` computes mean/stddev over
  SERVICE time (`dwell − queued − load`; feeding queue delay back into a wait estimate would be
  circular). `utilization` gained CV, ρ, and a Pollaczek–Khinchine `pkWaitMs` —
  `ρ/(1−ρ)·E[S]·(1+CV²)/2` — as a third opinion beside est and real, derived from the distribution
  rather than from either mechanism. New `serviceProfiles` op + panel breaks it down per caller,
  with statistics **blended toward the model prior** (pseudo-count 30, raw and blended both shown so
  the adjustment is auditable) — `life-raglit`'s CV of 3.77 is driven largely by one 398s request,
  and a handful of samples must not set policy.
  - **Dead config found and surfaced:** `maxQueueDepth: 8` against `maxWait: 15s` at
    `maxConcurrent: 1` and ~8s mean service allows **1–2** reachable waiters. The depth bound can
    never bind — which is exactly why 18/18 observed rejections were `queue-timeout` and none was
    `rejected`. The row now flags `depthUnreachable` rather than leaving the two settings to
    contradict each other silently.
  - **risks:** ρ over a window assumes steady state, which a bursty hour is not — `pkWaitMs` is an
    order-of-magnitude read, not a promise. Per-key stats are recomputed from the log per request;
    fine at this row count, not free forever.
  - **⚠ `realWaitMs` is CENSORED, and this is structural, not a bug to fix in the query.** It
    averages requests that queued *and were then admitted* — but anyone who waited past `maxWait`
    became a `queue-timeout` and is excluded. The longest waits are precisely the ones removed from
    the sample, so the measured mean is biased LOW and can never exceed `maxWait` (15s) no matter
    how bad the queue gets. First live reading: est 2.0s · real 4.6s · theory 3m49s. The truth sits
    above `real`; how far above is not observable while `maxWait` truncates the distribution.
    **`maxWait` does not only bound the wait, it bounds what can be MEASURED about the wait** —
    which is its own argument for deriving the setting rather than fixing it, and a trap for anyone
    who later "validates" a new estimator against this column.


## Keys-page charts *(was mis-numbered P20 — git's P20 is token counting)*

- ✅ **P20 — keys-page charts (who spent it, and on what).** Two stacked areas over a shared time
  axis: requests/cost/**dwell** by caller, and the same by model — neither derives from the other
  ("dun is 60% of cost" and "the 27B is 90% of cost" are both true and answer different questions).
  New `usageSeriesByModel` op (+ `store.RollupSeriesByModel`, optional key filter) is the "on what"
  axis to `usageSeries`'s "by whom"; the key-detail page reuses it scoped to one caller. One metric
  selector drives both charts so they are always read on the same measure — **separate charts, never
  a dual axis**. `seriesAxis` is shared by both series ops so the two charts cannot land on subtly
  different axes.
  - **Chart primitives extracted** from `usage.tsx` into `ui/src/Charts.tsx` (one implementation;
    a second copy is how two charts of the same data start disagreeing about gaps and stacking).
    Added a **crosshair + hover tooltip** (timestamp, per-band values, stack total) — bands below the
    top one are read as thicknesses, which the eye cannot measure, so exact values must be reachable
    some other way. That tooltip is also what lets the legend collapse without identity becoming
    color-only. Legend is **collapsed by default** (as requested) and absent entirely for one series.
  - **Unbounded series are folded, not cycled:** top 6 by window total keep the validated `SERIES`
    hues; the rest sum into a neutral grey "other" band — grey because "other" is an aggregate, not
    an entity. Palette re-validated unchanged against the panel surface (all six checks PASS).
  - **Window control (6h/24h/7d), not an axis trick.** A single 6,524-request embedding burst
    flattened the other 23 hours to a hairline. Log/clipping/broken scales all misrepresent a
    stacked area, which is additive by construction — the bands would stop summing to the total the
    reader sees. Narrowing the window leaves every mark honestly scaled to its own period (6h peak
    127 shows real structure where 24h showed a flat line).
  - **known gap:** no x-axis tick labels — time is carried by the hover tooltip only, matching the
    existing sparkline-style panels. Fine for "when did this spike" via hover; not for reading a
    time off the page.


## Cache visibility *(was mis-numbered P21 — git's P21 is provider credentials)*

- ✅ **P21 — cache visibility (is the prompt cache actually working?).** `cached_tokens` was captured
  per request but never summed anywhere, so the one number that says whether prompt caching earns its
  keep did not exist. Added `cachedTokens` + `cacheHitRate` to `usageRollup` and `usageByKey`
  (per model AND per caller — hit rate is a property of how a caller PROMPTS: a stable system prefix
  reuses cache, a shuffled one never does), plus `cachedSecondsSaved`, an estimate of the
  prompt-processing time avoided at the observed tokens/sec. Live at 7d: **72.3% box-wide,
  Qwen3-6-27B-MPT at 79.1%, 231.6M tokens served from cache ≈ 52h of prompt processing avoided.**
  - **The correctness question here is `cacheReports`, not the rate.** A zero in `cached_tokens` has
    two incompatible meanings — a backend with no prompt cache (every embedding model, every remote
    provider) versus one that genuinely missed. Reporting both as "0%" invents a caching problem for
    half the catalog. `cacheReports` counts requests that reported ANY hit; at zero the UI shows
    **`n/r`**, never a measured-looking 0%. Pinned by `internal/api/cache_test.go`.
  - **Still ambiguous, deliberately not hidden:** `cacheReports == 0` cannot separate "does not
    report" from "never hits". The tooltip says so. Separating them would need the capture to record
    whether the backend emitted the field at all, not just its value.
  - The grand-total rate is recomputed from summed tokens, NOT averaged across model rows — a model
    with 12 requests must not weigh the same as one with 14,000.
  - **known gap:** `chat` sits at ~19–22% against Qwen's 79–88%. Unexplained; it is a lane, so the
    member it lands on may be resetting cache between calls. Worth a look, not yet looked at.


## Agent self-update actually fires

6. ✅ **P17: heartbeat-driven self-update actually fires** — `maybeSelfUpdate` always ran from the
  heartbeat, but could not DECIDE: it compared version strings, and `make agents` without a tag
  stamps "dev" on both sides, so `ack.Version == "dev" && own == "dev"` skipped every update.
  That is the normal state while iterating on agent code — the exact case the feature exists for.
  Now both sides exchange a **build id** (short sha256 of the binary; `agent.HashFile` is the one
  definition, `agentdist.BuildID` caches by size+mtime since every agent asks every beat). Bytes
  differ → update; bytes match → don't, which is also what terminates the loop. Version strings
  survive only as the fallback when either side lacks an id, keeping their old dev/dev rule.
  Post-download the installed FILE is hashed (not the `X-Corrallm-Version` header) before the
  rename, so the restart guard compares like with like. 5 tests.

---

# Resolved decisions

## Engine & schema decisions (P1–P8, P14)
- ✅ **Lane vocabulary** (P14) — "lane" now means the config's named fallback list over models
  (the user's term); the priority group keeps its proper name and its API/UI surfaces were
  renamed lanes→groups. Persisted analytics naming (`lane_samples`, `LaneDepthSeries`) and
  sched/reservation internals keep the old word — schema stability over vocabulary purity.
- ✅ **Model = one serving path** (P14) — `cmd` XOR standalone `proxy`; residency knobs
  (sticky/persistent/ramUsage/swap/server) are rejected on proxy models at validation. The
  same capability via a paid remote = its own named model composed into a lane, which keeps
  cost accounting per path honest and makes degrade-vs-spill pure member order.
- ✅ **Module path & repo location** — `github.com/iodesystems/corrallm` at
  `iodesystems/services/corrallm`, its own git repo (sibling to redline2/ragtag).
- ✅ **Binary name** — `corrallm` (not the `corral` alias).
- ✅ **Capacity unit** — **per-backend slots** (`maxConcurrent`, default 1), chosen over
  per-server total concurrency. `server.maxConcurrent` layers on as a host ceiling with P4.
  (Capacity-declaration question — declared budget canonical + optional `CapacityProbe` — stands.)
- ✅ **Load coalescing** (P1) — concurrent requests for an unspawned backend wait behind one
  in-flight load (`proc.Manager`, `ready` channel). Queue-behind-load *backoff signaling* still TBD.
- ✅ **Swap-vs-spill default** (P4) — **evict-then-spill**: try eviction to make the preferred
  backend fit; spill only if eviction can't free enough. Configurable later; cost-minimizing
  weighing waits for P6.
- ✅ **Eviction policy** (P4) — evictCost + recency (LRU) + ttl-expiry scoring, constrained to the
  binding pool, all-or-nothing greedy, min-residency hysteresis. Vector bin-packing is greedy
  (small N).
- ✅ **Preempt-vs-spill default ordering** (P5) — **preempt first**: a `preempt` stage reclaims an
  eligible victim before considering spill; only when no victim exists does the stage's `then`/spill
  (else queue/reject) apply. Victim is the lowest-weight `interruptible` slot strictly below the
  preemptor. Per-type `onSaturated` can still pin behavior explicitly via `then`.
- ✅ **`limits` window semantics** (P6) — **sliding window** (trailing per-dimension event log,
  pruned on access), reading `$20/hr`/`600s/min`/`100/min`. **Both** per-group and per-(group×type)
  caps apply (a request charges against both). Over-budget → **spill if the stage allows, else back
  off** (reason `over-budget`, Retry-After = longest binding window); queue/preempt don't apply to a
  budget. Requests charge at admit (incl. the queue/promote path), dwell/cost at release.
- ✅ **Share-currency granularity** (P6) — **per-group** (`requests|dwell|cost`), request-count the
  default. `dwell`/`cost` use a per-group accumulator decayed with a 30s half-life (cost is
  retrospective; dwell measured at release). A backend whose queued groups disagree on currency
  falls back to request-count for that comparison — coherent and starvation-free. (Per-key not done.)
- ✅ **Quality-degrade model** (P7) — **variant-in-list** (one ordered backend list, quality-ranked),
  not a separate fallback map. Degrade is **per-group opt-in**: `acceptDegrade` + `qualityFloor`
  decide which quality tiers a group accepts; a non-degrading group sees only the model's top tier
  and backs off per its stage rather than spilling onto a worse model. Degrade transform = per-backend
  `maxTokens` clamp on the outgoing request (context-window clamp deferred — needs tokenization).
- ✅ **Slot reservations** (interactive headroom) — a keyed caller can lease K slots on a model's
  primary backend (`model#0`) for **its own lane**, so saturating batch backs off and interactive
  work finds an already-free slot. Proactive (holds capacity free), distinct from reactive preempt.
  Gates BOTH direct-admit AND promote via `effCapLocked = capacity − Σ(slots reserved by OTHER
  lanes)` — a freed batch slot won't refill a reserved one; preempt waiters bypass (they swap, not
  fill). Lease ≤ **5m** (`--reservation-max-ttl`), renewed by heartbeat re-POST, auto-expired by a
  2s reaper (`StartReaper`). API: `POST /v1/reservations {model,slots?,ttl?}` create/renew,
  `DELETE ?model=` release, `GET` list. Keyed by (backend, lane), no id. Any keyed caller may
  reserve for their lane. `internal/sched/reservation.go` + `internal/proxy/reservation.go`.
  **Dashboard**: a "Reservations" panel on the Lanes page (model/lane/slots/live-countdown),
  fed by a `reservations` gat op (`corrallm.reservations` in GraphQL). Verified live end-to-end:
  reserved nomic → interactive got its slot in 0.02s while batch queued → release drained batch.

## Audio scoping decisions — settled (P9)
- ✅ **Audio cost basis** — **file bytes** for v1 (deterministic, no extra dependency): STT $ by
  uploaded-audio bytes, TTS $ by `input` chars / output bytes. True-duration costing (parse
  `verbose_json`/SRT or add ffprobe) deferred to Optional extensions.
- ✅ **TTS scope** — **STT now, TTS endpoint stub**: land transcriptions/translations + parakeet
  fully (P9a/c/d); mount `/v1/audio/speech` wired to a configured remote/future TTS backend, optional
  and untested until one is chosen (P9b). No TTS engine selection blocks the phase.
- ✅ **Modality source** (P9d) — **inferred from cost class**: a backend `type` that declares audio
  coeffs (`audioWhPerMiB`/`audioUSDPerMiB`) is an audio type; a model is `audio` iff any backend uses
  one (`cost.IsAudioType`). Zero new config field. Known limitation: an audio model left **unpriced**
  won't be flagged — pricing it (which production should) flags it. Revisit with an explicit optional
  `modality` override only if an unpriced-audio case appears.

## Audio backend decisions — all settled (P9)
- ✅ **Multipart buffering strategy** (P9a) — **bounded in-memory buffer** (matches the JSON path,
  which already buffers the whole body at `proxy.go:85`); bound = 64 MiB × concurrent audio slots,
  fine on the 5090 box. Revisit (temp-file spool / stream-tee) only if audio concurrency grows.
- ✅ **Concrete TTS backend** (P9b) — **Kokoro** (`remsky/Kokoro-FastAPI`, Apache-2.0, CPU,
  native `/v1/audio/speech`, ~35–100× realtime on CPU, <2 GB). Picked over VibeVoice (CUDA-only on a
  full GPU, no turnkey OpenAI server, watermark/disclaimer, MS deprioritized it). **Chatterbox** (MIT,
  cloning, 4–8 GB) is the parked "quality" option for when GPU headroom exists.
- ✅ **Realtime ASR contract + backend** (P9e) — **standardize `/v1/realtime` on the OpenAI Realtime
  *transcription* schema** (de-facto standard; every OpenAI SDK speaks it). **Backend RESOLVED →
  sherpa-onnx via a native adapter** (`examples/sherpa-realtime-adapter`). Speaches was the first pick
  but its realtime *transcription* mode is **broken** (fires response-generation per utterance and 500s;
  ignores `create_response:false` — it's a speech-to-speech server). Parakeet-TDT is **batch-only**.
  The adapter speaks the OpenAI Realtime schema **natively** (corrallm passes through unchanged) and runs
  **sherpa-onnx streaming zipformer** inside — TRUE streaming (live `delta`s + silence endpointing, CPU
  int8). Validated full-stack: client → corrallm → adapter → live partials + finals, metered. Diarization
  NOT included (sherpa diarization is offline-only). *(Original Speaches plan retained below for history.)*
  Default backend:
  **Speaches** (ex faster-whisper-server, MIT, CPU, native `/v1/realtime?intent=transcription`) →
  true byte-passthrough, corrallm's transparent design holds. Custom-protocol backends (sherpa-onnx,
  WhisperLive) would need a thin adapter (base64-JSON↔binary-PCM transcode, auth, interim→`delta`/
  stable→`completed`, synth-VAD). **The installed batch Parakeet-TDT does NOT stream** (full-attention
  FastConformer) — realtime can't reuse it; Speaches (Whisper) or sherpa-onnx (both CPU) are the fits.
- ✅ **Realtime slot model** (P9e) — **one fairshare slot per live session, held for its duration,
  `dwell` currency, preemptible, and parkable in the background** (on preempt: park + resume when a
  slot frees, don't hard-kill). Idle/max-session timeout replaces the 130s request cap.


---

# Closed gaps in shipped code

Struck items are resolved; the ones still live were moved to `plan/plan.md` §7.

- ✅ ~~P1 first-backend-only~~ — resolved in **P3** (ordered fall-through; rr-within-type).
  ✅ ~~`Stage.Then` follow-up verb~~ — resolved in **P5** (preempt's no-victim fallback honors
  `then: fallThrough|spill|queue`). ✅ ~~`quality` inert~~ — resolved in **P7** (routing key +
  per-group degrade opt-in + `maxTokens` clamp).
- ✅ ~~No `limits`/cost metering~~ — resolved in **P6** (`internal/cost`; energy/paid/swap → $;
  per-request dwell/tokens/$ metered + persisted; sliding-window limits; `requests|dwell|cost`
  share currency). Remaining P6 gaps below.
- ✅ ~~No residency accounting~~ — resolved in **P4** (`pools`/`reserve`/`ramUsage`/`sticky`/
  `persistent` gate spawns + eviction). `swap.loadSeconds`/`loadWatts` now priced (**P6**). Still
  inert: affinity, `server.maxConcurrent` host cap.
- ✅ ~~Activity log only / no rollups/UI feed~~ — resolved in **P8**: `recentActivity`/`residency`/
  `usageRollup`/`lanes` ops + activity/usage/lanes views; live SSE events drive updates (15s
  fallback poll). Store carries dwell/tokens/$ per request + a per-model rollup query.
- ✅ ~~VRAM accounting assumed one server~~ — resolved 2026-07-28, before a second host exists,
  because both failures are silent. (1) `vramBudget` summed every resident process and every
  pinned model with no server filter, so two loaded servers each under-counted their own budget by
  the other's footprint and tuned themselves down to fewer slots. (2) The measured footprint was
  written to a hardcoded `"gpu0"` (`const vramPool`), so a unified-memory server declaring only
  `system:` charged it to a pool with no budget — every measured model there goes PERMANENTLY
  unschedulable, surfacing as a 503 that reads like a backend fault on a model that worked right
  up until it was first measured. Now `Server.DevicePool` (default `gpu0`, validated to name a
  pool the server actually declares) and every `vramBudget` term scoped to the model's own server.
  Regression tests: `TestEffectiveUsage_ChargesMeasuredToServerDevicePool`,
  `TestVRAMBudget_IgnoresOtherServersPinnedModels`.
- ✅ ~~Remote models reported as loaded; extension siblings disagreed about one process~~ —
  resolved 2026-07-28. Two bugs with one root: residency was keyed by served NAME and had no
  concept of "not ours". A pure-proxy backend latches `StateReady` on first request (manager.go:446
  — correct, there is nothing to health-check) and is never an eviction victim (`server == ""`), so
  groq/cerebras sat in the dashboard's "Loaded" panel forever having loaded nothing; meanwhile
  oidio's four models are ONE process, and whichever sibling spawned it read `ready` while the
  other three read `absent` off that same live process. Now: `Model.LocalProcess()`/`Model.Remote()`
  (proxytarget.go) are the predicates, `Process.remote` + `ResidentModel.ProcKey` carry them out of
  the manager, `/v1/models` reports `remote: true` + `state: "proxy"` (kind stays `model` — clients
  filter kind to separate models from lanes), and every consumer resolves state through ProcKey.
  `ModelDef.Spawnable` was `m.Cmd != ""`, which called every extension-hosted model a proxy.
- ✅ ~~Transient capacity misses reported as 503~~ — resolved: `ErrNoCapacity` now returns a
  `*proc.CapacityError` splitting **permanent** (won't fit even fully evicted → stays 503, a real
  operator fault) from **transient** (a resident is inside its `activeUse`/`minResidency` window →
  429 + `Retry-After` = when that blocker becomes a legal victim). The proxy walk keeps the
  *soonest* backpressure across candidates (`keepSoonest`) instead of the last one, so a lane
  answers "when could ANYTHING here serve", including a saturated-but-live member's dwell EWMA.
  Applied to both the inference and realtime paths. **Why it mattered:** agentkit-style clients
  retry 429 against their whole budget but cap 5xx at ~5 attempts (1+2+4+8s = 15s), which is
  shorter than these models' 30s cold load — so every mid-swap load was unretryable. Found via
  crucible, where pinned model names give a 1-element candidate list: any capacity miss was an
  instant, unretryable 503 that silently scored the task zero.
  *Deliberately NOT added: a queued-load intent marker.* It would re-create the load/evict thrash
  `activeUse` exists to damp (see the 107-spill note at `manager.go`), and the computed
  `Retry-After` already lands the client's retry at the moment eviction becomes legal. Revisit only
  with measurements showing spills persist.
- **P8-beyond known gaps / OSS pre-reqs:**
  (1) ✅ ~~`/api` unauthenticated~~ — resolved (`3e83001`): admin token (`<home>/admin.token`) gates
  `/api/*` incl. load/unload, via Bearer or cookie; `/v1`/`/upstream`/`/health` stay open.
  *(Single shared admin token — no per-user accounts/roles/rotation yet; fine for one operator.)*
  (2) ✅ ~~Cost coefficients are placeholders~~ — calibrated (ml-kit config): split into `chat`
  (Qwen: ~400W ÷ 83 gen, ÷2300 prompt tok/s → gen 0.0013 / proc 0.00005 Wh/tok) and `embed`
  (nomic: single pass → proc 0.000002, gen 0). Verified live: chat ≈ $0.0000068, embed ≈ $0.00000007.
  Re-measure if hardware/models change. *(Field name still says "WattsPerToken" but is Wh/token —
  cosmetic rename deferred.)*

---

# UX verification

## ✅ Lenny — the constant user, wired up and run once (2026-09-04)

The house UX method (`~/doc/patterns/lenny.md`, canon LEN-1..LEN-10) now has a harness here.
Reference implementation is caselit; this is the port.

| what | where |
|---|---|
| the constant — capability, familiarity, temperament | `ux/lenny.md` (byte-identical to caselit's; **never edited to suit a run**) |
| what varies — role and an external goal | `ux/scenarios/is-my-box-working.md` |
| the walk, each step named for the promise it keeps | `ux/walks/is-my-box-working.json` |
| this project's standard, cite-able by a finding | `ux/obviousness.md` (OB-1..OB-10) |
| the runner | `bin/ux` + `bin/ux.mjs` → `tmp/ux-run-<walk>-<stamp>/` |
| the scoped evaluator | `.claude/agents/lenny.md` (`tools: Read`) |

**It walks the LIVE daemon, read-only, and that is the deliberate trade.** The dashboard is empty
without models, attached hosts and traffic, and a person only ever opens it on a box that has all
three (LEN-4). A seeded copy would be reproducible and would also be an install nobody has used.
The price: a run photographs the box as it was, so `ACTIONS.md` records base, daemon version and
timestamp. **A walk carrying a `click` is REFUSED against a live base** — a press reaches requests
somebody is waiting on — and the guard is in `bin/ux`, not in the walk author's memory.

**Two traps paid for while building it:**

- `waitUntil: 'networkidle'` times out on every signed-in page. The live event stream never idles,
  so the run would have reported blank screens the person never saw. `domcontentloaded` + a fixed
  settle is what actually waits for React.
- The capture must NOT read `aria-label`. Doing so puts a word on Lenny's screen that his screen
  does not have — the nav rail's labels live in tooltips and he does not hover. It is captured
  separately and marked unread, which is how OB-10 became the most repeated finding of run 1.

**Run 1 (`1fb3df6-dirty`, 9 steps, live box1):** verdict **"No. Not on my own, anyway."** Report
and fix queue in `plan.md` §6. Composition, so run 2 is comparable: 8 zeros/ones on *what is being
asked of me*, one screen scoring 3s (the sign-in), and three sentences named as good —
"Nobody has been turned away in the last 60 minutes", "Nobody has had to work for an answer in the
last 60 minutes", and the quota page's "Counts are a snapshot from the last call — observed N ago
— not a live tick."


---

## ✅ Lenny — seven runs, two scenarios, and P30 (2026-09-04/05)

The UX method arrived here on 2026-09-04 with no harness (`chore: scaffold the Lenny UX
harness`). Two days later: seven runs, two scenarios, an information architecture rebuilt
around them, and two rules the runs asked for that the standard did not have. Design doc:
[`p30-information-architecture.md`](p30-information-architecture.md). What is still OPEN is in
`plan.md` §6 — this is the archive.

### The composition, which is the part that compares

| run | what it walked | Q1 | Q2 | Q3 | Q4 | total | verdict |
|---|---|---|---|---|---|---|---|
| 1 | before any fix | 10 | 12 | 10 | 10 | 42 | "No. Not on my own, anyway." |
| 2 | phase A | 17 | 12 | 18 | 13 | 60 | no |
| 3 | phase B | 15 | 11 | 12 | 4 | 42 | no |
| 4 | phase C + consequences | 20 | 22 | 23 | 21 | 86 | **yes**, "only as far as Now and Traffic" |
| 5 | phase D | 23 | 18 | 24 | 19 | 84 | **yes**, unqualified |
| 6 | after the vocabulary sweep | 22 | 14 | 15 | 10 | 61 | yes, "only as far as one screen" |
| W | **the caller** — a different scenario | 18 | 12 | 13 | 10 | 53 | no |

The walk changed between 2→3 and 3→4 and again at 6, so those are run totals against the four
questions, never step-by-step. **Run 3's dip and run 6's are both real and both instructive**:
run 3 found two defects in what phase B had just shipped, and run 6 found one that this session
had caused an hour earlier — a config edit with no author, on the afternoon the box felt slow.

### What each phase actually changed

- **A — the home screen answers its question.** A computed state sentence (the chip it replaced
  was hardcoded `color="success"` AND its text was a tautology — the schema says status is
  "always ok when the process is serving"), a **What needs looking at** panel collecting faults
  from three pages, the memory ledger moved to Machines, in-flight deduped to one home.
- **B — Traffic: one page, one time axis.** `/activity` + `/usage` merged; `store.Window` on
  every activity read (exclusive upper bound, so adjacent windows tile); `from`/`to` on eight
  endpoints; the window in the URL so a span can be sent to somebody.
- **C — seven pages, each named for a question.** Ten nav entries named after data became Now ·
  Models · Traffic · Callers · Machines · Setup · Bench. Ten old addresses redirect.
- **D — consequence and vocabulary.** Seven controls that reach live work now say what they cost,
  above the group, in text (a tooltip reaches nobody who does not hover; a confirm dialog is
  behind the press he will not make). The caller side says "group", the model side "model lane",
  matching a schema that had settled it at P14.

### The two rules the runs asked for, which no existing rule reached

- **OB-11 — the answer comes before the explanation.** Asked for on a page whose one useful
  sentence sat below four paragraphs of implementation notes.
- **OB-12 — a property that reaches live traffic explains itself, the same way a control does.**
  Asked for on finding his own team's key in the cheapest, interruptible group with nothing
  saying what either word costs a request.

### Traps, each paid for once

- **The harness lied and the product got blamed.** The scenario asserted "twice it gave up
  entirely"; the log for that hour said 384 of 385 served, nobody turned away, dwell no worse
  than the week's. Lenny dutifully reported the product — canon LEN-4. Ray's complaint is
  hearsay from colleagues now, which is what he would really have.
- **A frozen window drew live numbers.** `In use`, `Queue` and `Est. wait` come from the
  scheduler's current snapshot; under a fixed span they showed an est. wait identical to the live
  page's, on a page headed "these numbers will not change". Found only by reading two screens
  against each other.
- **"Nobody was turned away" was false.** It counted 429s alone, so a window holding eight
  `503 no backend available` said nobody was turned away. Found by the SECOND scenario — the
  first never thought to ask.
- **A fix of ours created a defect within the hour.** `--cache-reuse` was applied through the
  dashboard API, which recorded "edited through the dashboard" and nothing else; the next run
  found an unexplained change timed the same afternoon the box felt slow and could not tell
  whose it was. Revisions now record what changed and the address it came from.
- **`waitUntil: 'networkidle'` times out on every signed-in page** (the live event stream never
  idles), and **the capture must not read `aria-label`** — doing so puts a word on his screen
  that his screen does not have.

### The second scenario earned its keep immediately

Five runs used one goal and one role, so "Lenny is the constant" was an assertion. Wren — handed
a key a year ago, never opened the dashboard, does not administer the box — walked the same
screens and failed DIFFERENTLY: the front door scored 3/1/3/1 for Ray and **2/0/0/0** for her;
the model page cost Ray "how do I proceed" and Wren "is this even addressed to me"; the same two
controls were refused for opposite reasons. And she found the false statement above.

---

## ✅ The 2026-09-05 slowdown: prompt-cache prefix thrash on a one-slot backend

**Diagnosed, not fixed — the fix is a capacity decision that is yours (§9 territory).**
Found by following up the only real evidence of "slow" in five Lenny runs: he compared two
screens and noticed cache reuse had fallen.

**What happened, measured three ways.** Between roughly 10:00 and 14:00, each request to
`local-Qwen3.8-27B` reprocessed **8,000–17,000 prompt tokens** instead of the usual ~1,000–2,000,
and mean time answering tracked it exactly:

| hour | requests | mean | prompt tokens cached | tokens reprocessed / request |
|---|---|---|---|---|
| 09:00 | 384 | 4.5 s | 96.5% | 2,200 |
| 10:00 | 406 | 6.5 s | 82.6% | 8,506 |
| 12:00 | 292 | 10.3 s | 68.5% | 14,830 |
| 13:00 | 270 | 10.8 s | 57.2% | 17,148 |
| 14:00 | 106 | 2.0 s | 98.5% | 999 |

Yesterday's same hours: 88–97% cached, 860–1,660 tokens, 2.3–3.3 s. So this was a departure,
and it ended on its own.

**The mechanism is in llama.cpp's own log.** It picks a slot by longest-common-prefix
similarity, and logs the score: `selected slot by LCP similarity, f_sim_best = 0.999` in the
good hours. The share of requests that could NOT find a near-exact cached prefix, per hour:

    09:00  30%   10:00  49%   11:00  56%   12:00  62%   13:00  68%   14:00  9%

At 10:06:43 it gave up entirely once — `selected slot by LRU`, no similar slot at all — and at
10:09:14 the backend reloaded cold (7.9 s load, a 93,318-token prompt reprocessed, 64.5 s dwell).

**What it is NOT** — checked, because these are what an operator would suspect: no rejections
(zero all day), no queueing (`queued_ms` zero), no configuration change (nothing since
2026-09-03), no second model competing for the card (only Qwen served all day, one placement),
and nothing to do with `carlsmacbookpro`, which has been unreachable far longer than this window.

**The cause is the traffic's shape, not the box.** `local-Qwen3.8-27B` runs with **one slot**, so
one conversation's prefix is cached at a time. Interleaving two conversations with different
prefixes makes each request reprocess the divergent tail — which is exactly an f_sim of 0.5–0.9.

**The remedy, and the reason it is not applied here:** more slots would give each conversation its
own cache, and slots cost KV memory. gpu0 is at **96%** (30 GB of 31 GB), so raising `nSlots`
means lowering per-slot context or moving the model — a capacity trade only you can make.

**The product gap this exposes, which IS ours:** corrallm records `cached_tokens` per request and
can therefore see this collapse, but no screen says it, and the activity log records that a
backend loaded without recording WHY. Five runs asked "will it come right on its own" and the
answer was in the data the whole time. A "prefix reuse" figure beside time-per-request, and a
reason on every load, would have answered it. Filed as the strongest candidate for the next
slice — it is the first thing in this whole exercise that would have told Ray *why*.

---

## ✅ The Mac, measured: capacity that cannot serve this workload (2026-09-06)

`carlsmacbookpro` was dark from July until 2026-09-05 and is now up, heartbeating and
self-updating (`plan.md` §6 for how, and for the routing workaround it needs). What it can
actually do was then measured rather than hoped at, on `Qwen3.6-35B-A3B-MTP Q6_K_XL`, a 35B MoE
with ~3B active — the shape that is supposed to suit Apple Silicon:

| M1 Max, 69 GB, cold prompt of 8,813 tokens | |
|---|---|
| prefill | **71 tok/s** |
| generation | **10.7 tok/s** |

Against box1's measured **731 tok/s** prefill over the same day. One of aw4's ordinary 60k-token
prompts is ~30 seconds of prefill on box1 and **~14 minutes** on the Mac.

**So it is not overflow capacity for this box's traffic**, and it never was: prefill, not
generation, is what aw4's 40–120k-token prompts spend their time on. A dense 27B there would be
worse still (~22 GB of weights against ~400 GB/s).

**It is also not routed anywhere** — the `chat` lane has exactly one rung, `local-Qwen3.8-27B`,
so nothing would have fallen through to the Mac even if it were fast. Two independent reasons it
sat idle, and only one of them was the agent being down.

**What it could still be worth**, all short-prompt work where 71 tok/s prefill does not matter and
which currently competes with chat for box1's single slot: `local-nomic-embed-text`, and the oidio
audio models. Filed in `icebox.md`.

**Why this is worth keeping.** It converts "the Mac never realised" into two numbers, and those
numbers are the argument for a second card: the machine that exists is 10x slower at the thing
this workload is made of.

## Lane and alias traffic became visible, 2026-09-06

Started from "this is why we have openrouter as well" and ended somewhere else.

**The activity log recorded the name asked for, not the model that answered.** `served` was
the request's own `model` field, never reassigned to the winning candidate, while Prometheus
had the right name all along (`proxy.go:685`, `metrics.Request(prov, name, …)`) — one request,
two accounts, disagreeing whenever a caller addressed anything indirect. It cost the callers
doing it the intended way their whole history: 4,249 rows said the model was `chat` and 48
said `free`, carrying 19.2M prompt tokens belonging to no model. `yscr` ran 4,070 of them over
17 days.

Now `served` is the candidate that answered and `requested` is what the caller wrote — its own
column, not parsed back out of `req_body`, which is absent or truncated on a large fraction of
rows (4,482 of one caller's 12,294 in a day). A request nobody served repeats the requested
name rather than crediting a backend that refused; a rejection is not work. History is not
backfilled, because the answering model was never recorded and reconstructing it from config
revisions would be inference wearing the costume of measurement — expect a discontinuity in
per-model charts at the cutover. `abfa815`, verified live: `served=local-Qwen3.8-27B,
requested=chat`.

**It found an indirection nobody was looking for.** `life-raglit` addresses models by alias
(`nomic-embed-text` → `local-nomic-embed-text`), mis-recorded exactly like lanes.

**And "no backend available" was a lie about the box.** Probing the free lane found
`cerebras-gpt-oss-120b` answering **402 Payment Required** since at least 2026-09-01 — an
unpaid account. corrallm spills past a hard-failing free backend, correctly; but when it was
the last candidate the row said `no backend available`, a statement about corrallm, while the
reason lived in one log line. Eight rows over five days said it. The row now says
`cerebras-gpt-oss-120b refused with 402`. The wire response is unchanged: callers parse it,
and an upstream's status is the operator's business. `23195f3`, verified live.

**And the dead rung was pulled.** Probing all twelve free-lane rungs found exactly one
casualty: cerebras. The other eleven answered 200, bar one 429 that is the free tier working
as sold. Cerebras's provider definition and lane membership are gone (config revision 33,
rollback at 32), with the reason written into the groq notes beside the sentence it
invalidated — that note had claimed the duplication was deliberate, which stopped being true
the moment the second quota stopped paying out. The free lane now serves from
`groq-gpt-oss-120b` plus ten pool models; verified after the reload with
`served=groq-gpt-oss-120b, requested=free`.

**`PLACEMENT` stopped being a label** (OB-3): *Ran on*, *Ways to run this*, *N boxes*, with the
config's word kept in each gloss. `0eb90c2`.

**What this did NOT settle.** `chat` being a one-rung ladder is a design — the indirection you
retarget on upgrade, meaning "the best we can do" — not a gap. Whether aw4 should stop pinning
a model and ride it is now genuinely open and unblocked (`open-questions.md` §4).

## A dead rung now says so, 2026-09-06

Follow-on from the cerebras removal, and an answer to "do we have a way of
auto-detecting free lanes from various providers".

**We do, and it was already running — for one provider.** `internal/freeroster` refetches a
provider's `/v1/models`, records which ids are free, and marks a backend stale when its model
churns out of that set, so the selector routes around it before a request lands. Its package
doc describes the cerebras case exactly. The virtual extension does the enrolment half — the
ten OpenRouter rungs were never typed by anyone.

**But `RefreshRoster` skips any model without `freeTier.refresh: true`,** and groq and cerebras
were declared statically without it. That is the mechanical reason a 402 sat unnoticed for five
days: the proactive check written for this was never switched on for that rung.

**And it could not have been switched on usefully.** Both catalogues, pulled through corrallm:

| provider | rows | flagged free | priced | modality reported |
|---|---|---|---|---|
| groq | 14 | **0** | 9 | 0 |
| openrouter | 430 | 21 | 409 | 430 |

The free test is a `:free` id suffix or `pricing.prompt == "0" && pricing.completion == "0"`.
Groq's free tier is a **rate limit on normally-priced models**, not a zero price, so there is
nothing in its catalogue to detect; cerebras is the same shape. Auto-detection generalises to
providers that price their catalogue OpenRouter-style and to no others. Enabling refresh on
groq would be inert rather than harmful — `Roster.Has` returns `known=false` on an empty set,
so a provider with no free rows cannot be falsely stranded.

**So the gap was never detection — it was that nothing accumulated.** A hard failure was
spilled past per request and forgotten. The ledger's `hardFails` count could not help and was
not meant to: it lives in memory, clears on any 2xx, and reset to zero on every restart, so it
answers "how hard should I back off right now", never "has this been dead a week". Nor is the
activity log any use — a spill writes no row naming the backend it spilled PAST, since one
request is one row, so only a rung failing as the LAST candidate leaves a trace.

Now a small durable table holds one row per refusing backend, deleted the moment it serves
again. `since` is fixed at the first refusal and never advanced: the question is how long it
has been broken, and when it last failed is already in the log. Surfaced above the ledger on
Provider budgets, naming the problem and the fix rather than the status — 402 reads "the
account is unpaid — fund it, or drop this rung from its lane" (OB-3, OB-6). `1bd0052`.

**Verified by test, not by observation.** The end-to-end test drives a real 402 through the
real proxy stack, including a restart, and asserts the streak survives it and clears on the
first success after. It could not be watched live: the one backend that produced 402s was
removed an hour earlier, and the live check confirms only the plumbing — table created,
endpoint carrying an empty `refusing` list.

## The queue clash got a button, 2026-09-06

"Lame. why not a fix it button?" — about a note Lenny run 8 had just called the best writing
in the product.

**The prose was worse than lame: it misdirected.** It said "raise the wait it permits or lower
the queue it advertises, on Setup **under the model**". Both are `Config.Scheduler` — box-wide.
A model carried `maxConcurrent` and no queue knob at all, so anyone who followed that sentence
would open the model and find nothing. It scored 3/3/3/3 only because he never went.

**And the reason there was no button was arithmetic, not effort.** A queue's reachable depth is
capacity × maxWait / meanService — one model's numbers — so on a box with one slow model and
several fast ones there is no correct global depth: the value that stops the slow one
advertising a slot it cannot honour starts turning away callers of the fast ones. A button
writing a box-wide value from a per-model diagnosis would have been a worse defect than the
prose.

So the bound became per model. `Model.Scheduler` is an optional override resolved
**field-by-field** over the box-wide one — an override means "this model's queue is different",
not "this model opts out of every bound", and replacing the struct would make setting a depth
silently drop the wait. A zero field means INHERIT, not unbounded, for the same reason.
Resolved once per admission and used by both paths that can queue, the preempt waiter included.

The button sets one field on one model, shaped like `UpdateNotes` rather than the full upsert:
a control that can rewrite a proxy target while claiming to adjust a queue is worse than the
problem it fixes. It states the cost before you press it — nobody waits longer, no other model
changes, requests already running are unaffected. `maxWait` deliberately stays a sentence: it is
box-wide and llm-bench's stall guard is derived from it, so one click is the wrong shape.
`da5ef3d`, `c1fe48c`.

### Two traps, both worth keeping

**The first cut wrote to a derived view.** `c.Models` is a flat INDEX built at load from
`providers.<p>.models`, `extensions.<e>.providers.<p>.provides` and the top-level `models:`
block. Writing only there survives until the next save and is then re-derived away — and on
this box every model is authored under a provider, so the button did nothing at all. Caught by
exporting the config after pressing it, not by a test: the configdb round-trip test passed
throughout, because it used a flat `Models` map, which is not the shape a real box has.
`editAuthoredModel` now writes where the model is written down. The tests assert on the
**authored** entry, which is the assertion the first cut would have failed.

**Then a stale binary faked the same bug a second time.** After the fix, `corrallm config
export` still showed no override — while the daemon resolved it correctly and survived a
restart on it. `bin/corrallm` is a gitignored build artifact, and the one on disk predated the
`Scheduler` field, so `fromMap` dropped the unknown key on export. The store had been correct
the whole time; the tool used to check it was not. **`bin/deploy` rebuilt and installed the
daemon and never refreshed `bin/corrallm`,** so the CLI silently lagged the running server —
and every `config` subcommand reads through it. Fixed the same day (`ed045c1`).

### The guard existed and missed on two counts

`cmd/corrallm/staleness.go` was already written for exactly this, well argued, with tests. It
ran in `serve` only, and it disabled itself unless the Makefile had stamped `srcDir` — and the
two conditions that actually bit were both outside it: a CLI subcommand, from a hand-built
`go build -o bin/corrallm`.

So: `PersistentPreRun` on the root command checks every subcommand (stderr, so
`config export > file` is unaffected — verified); an unstamped binary falls back to the module
root above the executable, which distinguishes a hand-built binary in `./bin` from a release in
`/usr/local/bin` that still correctly stays silent; and the message says what staleness costs
for the command being run, because "restart the service" is not the fix when the problem is a
misread export. `bin/deploy` now rebuilds the CLI with `make build` so it is stamped and can
warn on its own.

**A regression made while fixing it, worth keeping:** rewriting `bin/deploy` through a
write-temp-then-rename helper dropped its execute bit, and the commit recorded mode 100644.
Caught by the next deploy refusing to run. An atomic write does not inherit the mode of the
file it replaces.

## OB-13, and a measurement that was lying to us, 2026-09-06

### The rule run 8 asked for

`COME BACK LATER` read "Everybody got an answer between 09:00 AM and 10:00 AM on 9/6/2026 —
nobody was told to come back, nothing was refused, and no answer was cut short." Every word
true, of an hour in which sixteen requests ran past 11 s and the slowest took 40.0 s against a
3.8 s mean. The 40.0 s was on screen — four panels down, beside the mean, with nothing saying
which of the two was the news. He read the verdict, believed the hour was fine, and closed the
lid: *"a true average can hide a true incident from someone who reads three things and stops."*

**OB-13**: a summary of a span carries its worst moment, not only its average.

`store.SlowSpell` finds it — how many ran past three times the mean, when it started and ended,
the worst one and when. Three because it has to clear ordinary spread rather than mark it: this
box runs a CV near 1.5, so 2× would flag a normal hour and train the reader to scroll past the
one sentence written to stop them scrolling past it. The mean is over SERVED requests only; a
429 is refused in milliseconds and would drag the average down, then make the threshold it
defines too easy to clear.

The verdict keeps its claim — the claim was never wrong — and gains what qualifies it, in the
panel where the reader stops. It reports and does not judge: whether sixteen slow requests are
bad depends on what the box is for, which the reader knows and the screen does not. Validated
against the hour that produced it: 428 rows, 3.8 s mean, 16 over 11.4 s between 09:12 and
09:47, worst 40.0 s at 09:41. `1b88cae`.

### And then the harness turned out to be lying

The next item was going to be "split Setup — 480 of its 510 lines are notes stacked under the
change log". **That finding was false, and looking at the screenshot before building anything
is the only reason it did not get built.**

`bin/ux` promises its text file is "every word visible on that screen", and its own comment
says a collapsed panel must not reach the evaluator. `innerText` honours `display:none` and
`visibility:hidden` — it does **not** honour overflow clipping, and this dashboard clamps every
notes field to one line with `-webkit-line-clamp`. So the full note stayed in the DOM and
arrived looking like something a person had read. On screen it is one line:
"P16 free-tier aggregator (plan/p16-free-aggregator.md). ONE integration…". The engineer's
diary of LDFLAGS, cgroup ceilings and an out-of-memory story was never visible to anybody.

That is the worst kind of false finding for a method whose entire claim is that it measures
what a person could see. Fixed: leaf elements the page clips now carry their visible beginning
plus `[clipped on screen — the rest was not visible]`, rather than a guess at which words
survived. Step 09 drops 510 → 436 lines and the diary is gone. `c956932`.

**Run 8's defect #6 is withdrawn**, and the part of its Q1 score that rested on "buried under a
wall I'd never read" with it. Runs before `c956932` scored some pages against text that was in
the DOM and not on the screen; the effect is largest wherever notes are dense, which is Setup.

**Still open, and a real question this exposed:** the full notes exist ONLY on hover, which
OB-10 says means they were never written. An operator with 12 KB of notes on a model can read
them by hovering and no other way. Not acted on — it needs a decision about where notes belong,
not a clamp adjustment.

## Run 9 — the same total, a different shape, 2026-09-06

| run | walked | Q1 | Q2 | Q3 | Q4 | total | verdict |
|---|---|---|---|---|---|---|---|
| 8 | before OB-13 | 21 | 18 | 26 | 13 | 78 | yes, "only as far as one screen" |
| 9 | after OB-13 + the capture fix | 22 | 18 | 21 | 17 | 78 | yes for the read-only part |

**Q4 rose 13 → 17, which is where OB-13 aimed.** He quoted the new sentence as the thing he
came for: *"16 requests were much slower than the rest between 09:00 AM and 10:00 AM... The
slowest took 40.0 s, at 09:41 AM. ... I could put this in the team chat word for word."* Step 05
went 3/3/3/1 → 3/3/3/3.

**Q3 fell 26 → 21 on a single 0**, and it is not a regression — it is a finding that was
previously masked. See below.

**Two of run 9's defects were my own harness artifact.** The first cut of the clipping fix
truncated at a flat 60 characters, so subtitles a person reads ~150 characters of arrived as
stubs and he reported explanations as "cut off exactly where it would matter" that are in fact
readable — while quoting the fuller text off the screenshot, so the text file and the image
disagreed about the same page. Fixed to clip proportionally to the visible fraction of the box;
the capture now ends "and only while there is co…" against the screen's "and only while there
is …". Defects 4 and 6 of run 9 are discounted accordingly, and step 03's Q1 = 1 with them.

### The Q3 = 0, which is real and now has a factual answer

`sk-aw4` — 35,646 requests, "just now", unmistakably his team — sits in group `batch`, weight
`1`, and the Priority Groups table says `batch` is **interruptible: yes** while `default` and
`interactive` show `—`. Nothing on the page says what that word costs. He connected it to a
teammate saying the assistant "gave up on him" and could not rule it in or out:

> "Until that word is explained somewhere on the page it's attached to, I'm not confident enough
> to tell the team 'it's fine' — only 'it answered everyone, eventually'."

This is OB-12 exactly — the rule run 5 asked for, still violated on the table that most needs it.

**Checked against the log: it has never happened.** The proxy records a preempted request as
status 499 with error `preempted`, and there are ZERO such rows in the entire history. aw4's
eight 499s are client cancellations. So the honest answer to his question is "that word has
never cost you a request on this box" — which is exactly the sort of thing the screen could say
and does not.

## The home screen answers two questions now, 2026-09-06

It carried a panel headed **"Nothing else on this page, on purpose"**, explaining that Models is
what can be called, Machines is what the hardware holds, and Traffic is what it has been doing.

That was a defensible answer to "is it broken" and the wrong answer to the question an owner
actually opens a dashboard with, which is **two** questions: is it working, and is it doing
anything. A page that can only say "nothing needs you" cannot tell a healthy busy box from a
healthy idle one — and those are not the same news to somebody paying for the hardware.

**What it is getting done**: machines, models loaded, and the last hour's requests, tokens in
and out, and cost. Live: `2 machines · 1 of 12 loaded · 619 requests · 47.0M in · 89.0k out ·
$0.0246`.

- The window is stated once on the panel rather than five times (OB-4).
- Idle says "none" in words; a strip of zeroes reads as broken rather than quiet.
- Compact headline numbers — "46,840,478 in" is exact and unreadable at a glance, which is the
  only way a strip like this is read. Traffic keeps the exact figures, where somebody is looking
  rather than glancing.
- Deliberately not a chart: six points of one hour invites reading a trend off noise, and this
  answers "how much", not "which way". Traffic owns the shape and the panel says so (OB-8).

Nearly free — the page already fetched this hour's rollup to compare against the day before it,
and used it only to decide whether to say "slower than usual". Two fields added to that query,
nothing new called. `1f0fdd9`.

**Known duplication, left in deliberately:** the arrival sentence already says "1 of 12 models
loaded across 2 machines", so machines and models appear twice — once as prose, once as
scannable stats. Both were asked for. The sentence is the OB-5 arrival line and has scored 3
in every run, so it was not touched to remove the overlap.

## Thinking back on, with a budget the proxy computes, 2026-09-07

**Sampling was already exactly right.** Both profiles match the Qwen3.8-27B card field-for-field
— thinking 1.0/0.95/20/0/0/1.0, instruct 0.7/0.80/20/0/1.5/1.0 — verified at the backend via
`/slots` rather than in config. `presence_penalty: 1.5` looked aggressive from Qwen3 habit and
is this model's own instruct recommendation.

**Thinking turned back on** (config revision 37): `--reasoning off` → `on`, and
`sampling.default: instruct` → `thinking`. Both together, because the config's own note already
carried the invariant — the flag picks the DEFAULT mode and does not lock it, so the sampler
default must agree or the model thinks while sampled to instruct, the silent degradation
`sampling.go` exists to prevent. `--reasoning-format deepseek` pinned so thoughts land in
`message.reasoning_content` and not in `message.content`, because aw4 had never seen thinking
output. Pinned rather than left on `auto` since what `auto` resolves to is a property of the
template.

**Measured cost, 27 requests against 1,176 of baseline:**

| | avg out | max out | avg dwell | max dwell |
|---|---|---|---|---|
| instruct | 102 tok | 1,532 | 3.7 s | 154.8 s |
| thinking | 1,525 tok | 6,970 | 22.6 s | 88.6 s |

15× the output, 6× the latency. On a ONE-slot backend with `maxWait` at 15 s, that is everybody
else's queue.

### The budget, and why a fixed one could not work

At the measured 99.8 tok/s, 20% of a near-empty 188k window is 37,400 thinking tokens — **6¼
minutes before the answer starts**. A single number is wrong at both ends of the same model's
traffic anyway: large enough for a short question is most of the window on a 150k-token one.

`budget = (context − prompt) × fractionOfRemaining`, computed per request. The share is of what
is LEFT, so the answer is reserved by construction — at 0.2, four fifths of the remaining window
stays to answer with, and there is no separate reserve to keep in step. Live: 0.2, min 2048,
max 8192.

Prompt length is estimated at 4 bytes/token rather than tokenised: the tokenizer is in the
backend, so an exact count costs a round trip to the process the request is queued for. It errs
safe — code and JSON run nearer 3, so dividing by 4 under-counts the prompt and makes the cap
generous rather than tight.

**Below `min` it goes unrestricted rather than clamping up, and the probe proved why.** A
direct request with `reasoning_budget_tokens: 16` cut reasoning from 1,315 to 66 characters —
and grew the ANSWER from 143 to 762. An over-tight budget does not save tokens; it moves the
reasoning into the content, where nothing is labelled as reasoning at all.

### Verified live, not just tested

- llama.cpp honours `reasoning_budget_tokens` per request — the truncation above.
- `/slots` reports `reasoning_format` and no budget field at all, so an earlier "reasoning_budget:
  None" reading proved nothing in either direction. Worth knowing before it is read as evidence.
- corrallm's own injection was confirmed by temporarily setting `max: 48`, sending one request
  through the proxy, and watching reasoning come back at 188 characters. Settings restored
  (revision 40).
