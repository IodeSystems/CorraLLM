# P36 — OpenAI compatibility audit

Answers §9.0c's second sentence: "Ensure that is the case." Scope is every serving
surface, not audio. Rules applied: §9.0b (accept generously, produce and document
strictly) and §9.0c (the published shape is OpenAI's, wherever OpenAI published one).

**No code was changed by this audit.** It is a findings document.

## Verdict

The compatibility claim holds on the happy path and fails on the error path. A
request that succeeds gets a body corrallm never touches, so chat, completions,
embeddings and audio are as compatible as the backend behind them. Everything a
client reads when something goes wrong is where the claim thins out: **corrallm's
404 body is not valid JSON** (an unescaped model name breaks the envelope, and a
caller-supplied name can inject into it), **every error is served as
`text/plain`**, **none carries OpenAI's required `type`/`param`/`code`**, and
**every `/v1/` path corrallm does not mount answers 200 with the dashboard's
HTML** — including `GET /v1/models/{model}`, which OpenAI does define. The
second-thinnest place is the manifest: it is normative under §9.0b and it
currently advertises models the proxy refuses 404, while the README still carries
the exact `num_speakers` claim the manifest was corrected for.

---

## Bucket 1 — accepts less than OpenAI does

### 1.1 `GET /v1/models/{model}` is not implemented; it answers 200 `text/html`
- **Where:** no route mounted (`internal/proxy/proxy.go:535` mounts only
  `/v1/models`); the request falls to the SPA catch-all at
  `cmd/corrallm/main.go:844`.
- **OpenAI:** `retrieveModel` — `GET /v1/models/{model}` returns the bare `Model`
  object; `DELETE /v1/models/{model}` also exists.
- **corrallm:** `200 Content-Type: text/html` with the dashboard's `index.html`.
- **Breaks:** `client.models.retrieve(...)` in any OpenAI SDK — it gets HTML with
  a success status and fails in the JSON decoder, not in error handling.
- **Verified:** probed `:8111` — `/v1/models/local-Swift-Qwen3.8-27B` →
  `200 text/html`; OpenAI side read from `openai-openapi` `openapi.yaml` (v2.3.0).

### 1.2 Any unmounted `/v1/…` path answers 200 `text/html`
- **Where:** same catch-all, `cmd/corrallm/main.go:844`. The hazard is already
  known in this file — `main.go:765` explains that `/healthz` is answered inline
  "so it can't fall through to the SPA catch-all (which would 200 with HTML and
  mask an unhealthy process)". The same reasoning was not applied to `/v1`.
- **corrallm (probed, all `200 text/html`):** `/v1/responses`, `/v1/moderations`,
  `/v1/files`, `/v1/nonexistent`, `/v1/models/`, `/v1/chat/completions/`
  (trailing slash), `//v1/chat/completions` (base URL given as `…/v1/`).
- **OpenAI:** 404 with the JSON error envelope.
- **Breaks:** capability probing ("does this server do `/v1/responses`?" answers
  yes-shaped); any client whose `base_url` ends in `/v1/`; `OpenAI-Beta` realtime
  helpers that POST `/v1/realtime/client_secrets` or `/v1/realtime/calls`.
- **Verified:** probed `:8111`, six paths.

### 1.3 A body over the cap is silently truncated instead of refused
- **Where:** `internal/proxy/proxy.go:569-578` — `io.ReadAll(io.LimitReader(r.Body,
  maxBody))` with no check that the limit was reached. 32 MiB for JSON routes,
  64 MiB for STT uploads.
- **corrallm:** a 33 MiB chat body is cut at 32 MiB, no longer parses, and is
  reported as `400 {"error":{"message":"missing \"model\""}}` — pointing the
  caller at a field that was present and first in their body. An oversize
  multipart upload is cut mid-file and forwarded to the backend if the `model`
  part survived the truncation.
- **OpenAI:** 413 / an explicit size error.
- **Verified:** probed `:8111` with a 33 MiB body — got exactly the
  `missing "model"` 400 above.

### 1.4 POST-only routes do not check the method
- **Where:** `handleInference` (`proxy.go:557`) never reads `r.Method`;
  `handleModels` takes `_ *http.Request` (`proxy.go:2128`), so POST/DELETE
  `/v1/models` returns the catalog.
- **corrallm:** `GET /v1/chat/completions` → `400 missing "model"`.
- **OpenAI:** 405.
- **Severity:** low — no real client does this. It is listed because the wrong
  message sends a caller hunting for a body problem.
- **Verified:** probed `:8111`.

### 1.5 Malformed JSON is reported as a missing `model`
- **Where:** `modelFromBody` (`proxy.go:2821-2827`) discards the unmarshal error;
  the empty result then hits `proxy.go:583-586`.
- **corrallm:** `-d 'not json'` → `400 missing "model"`.
- **OpenAI:** 400 `invalid_request_error` naming the parse failure.
- **Verified:** probed `:8111`.

### 1.6 The audio file part's declared Content-Type is discarded on model rewrite
- **Where:** `rewriteModelMultipart` (`proxy.go:1901-1950`) rebuilds every
  non-`model` part with `mw.CreateFormFile`, which writes
  `Content-Type: application/octet-stream` unconditionally. This runs on every
  audio request whose backend has its own model id — i.e. all `oidio-*`.
- **Why it matters here:** the codebase already knows backends sniff the part
  header — `replaceFilePart` (`audio.go:405-412`) sets `audio/wav` explicitly with
  the comment "A backend that sniffs the part header rather than the payload would
  otherwise be told the wav is still an mp3." The model rewrite undoes that care
  for every other request.
- **Verified:** read the code; confirmed `CreateFormFile`'s header empirically by
  running a 12-line Go program against `mime/multipart` (output:
  `Content-Type: application/octet-stream`).

---

## Bucket 2 — emits something OpenAI does not

### 2.1 The 404 body is not valid JSON, and the model name is injected raw
- **Where:** `noSuchModel` (`proxy.go:2013-2018`) returns a message containing
  literal `"` quotes; it is concatenated into a JSON string literal at
  `proxy.go:589` (inference), `proxy.go:1295` (realtime), `proxy.go:2031`
  (upstream) and `proxy.go:2085` (count_tokens). `proxy.go:780` does the same with
  an `fmt.Sprintf("%q")` message.
- **corrallm, on the wire:**
  ```
  {"error":{"message":"unknown model "nope-xyz""}}
  {"error":{"message":"model "groq-gpt-oss-120b" is disabled in this configuration"}}
  ```
  Both fail `JSON.parse`. A model name containing `"` injects structure:
  `model: "a\"},\"injected\":{\"x\":1,\"y\":\""` produced
  `{"error":{"message":"unknown model "a"},"injected":{"x":1,"y":"""}}`.
- **OpenAI:** a parseable `ErrorResponse`.
- **The fix already exists in the package** — `jsonString()` (`audio.go:459`) is
  used correctly at `proxy.go:639` and nowhere else.
- **Ranked first** because it is the only finding that hands a client a body it
  cannot parse at all, on the most common failure a new integration hits.
- **Verified:** probed `:8111` three times (unknown model, disabled model,
  crafted name).

### 2.2 Errors are served as `text/plain`, and several are not JSON at all
- **Where:** every `http.Error` call in the package. `http.Error` sets
  `Content-Type: text/plain; charset=utf-8` and `X-Content-Type-Options: nosniff`
  even when the body is JSON.
- **Plain-text bodies (no envelope at all):** `proxy.go:576` `read body`;
  `proxy.go:708` `unrecognized caller key: …` (401); `proxy.go:793` + `1687`
  `model is paused` (503, inference path — the realtime path at `proxy.go:1312`
  passes JSON for the same condition); `proxy.go:801` `no privacy-safe backend
  available…`; `proxy.go:812` `no permitted credential for this key`;
  `proxy.go:1480/1485/1497/1503` `streaming unsupported` / `backend unavailable` /
  `backend write` / `backend read`; `proxy.go:2036`; `proxy.go:2068-2112` (all five
  count_tokens refusals).
- **OpenAI:** `Content-Type: application/json`, body `{"error":{…}}` always.
- **Verified:** probed `:8111` (headers shown above); read every call site.

### 2.3 The error object is missing `type`, `param` and `code`
- **OpenAI:** the `Error` schema marks **all four** of `type`, `message`, `param`,
  `code` as required (`openapi.yaml`, `Error` / `ErrorResponse`).
- **corrallm:** emits `message` alone at `proxy.go:584/589/780/1267/1290/1295/1312/
  1454`; `type` appears only on the transcode refusals (`proxy.go:639`,
  `"invalid_request_error"`) and on the 429.
- **Breaks:** any client that branches on `error.code` (e.g. retry on
  `rate_limit_exceeded`, surface `model_not_found` differently) gets `undefined`.
- **Verified:** read the code; OpenAI side from `openapi.yaml`.

### 2.4 The 429 body uses a non-OpenAI `type` and non-OpenAI keys
- **Where:** `writeBackpressure` (`proxy.go:2587-2617`).
- **corrallm (probed):**
  ```
  HTTP/1.1 429   Content-Type: application/json   Retry-After: 24
  {"error":{"capacity":1,"in_flight":1,"message":"backend at capacity; retry after
   backoff","reason":"queue-timeout","retry_after":24,"ticket":"…",
   "type":"backpressure","waiting":0}}
  ```
- **OpenAI:** `{"error":{"message":…,"type":"rate_limit_error","param":null,
  "code":"slow_down"}}` plus `Retry-After`.
- **Assessment:** the headers and `Retry-After` are right and the extra keys are
  useful. The divergence that costs something is `type: "backpressure"` where a
  client expects a rate-limit type, and the absent `code`. This is the one place a
  corrallm-specific extension sits **inside** the OpenAI-typed field instead of
  beside it.
- **Verified:** probed `:8111` (the live box was saturated, which produced the
  response above unprompted).

### 2.5 `data: [DONE]` on the STT stream is corrallm's, not OpenAI's
- **Where:** `sttstream.go:224` and `:409`; asserted as normative in the manifest
  (`proxy.go:2383`): "`[DONE]` IS ALWAYS THE LAST FRAME — use it as your
  end-of-stream test."
- **OpenAI:** the transcription stream is a discriminated union of
  `transcript.text.delta` / `transcript.text.segment` / `transcript.text.done`.
  **No `[DONE]` sentinel is documented for this endpoint** — the spec's own SSE
  example ends at `transcript.text.done`. (`[DONE]` is OpenAI's chat-completions
  convention, not this route's.)
- **Assessment:** harmless to SDK clients (their generic SSE reader already
  breaks on `[DONE]`), but it is a shape OpenAI did not publish, promoted to a
  normative promise. §9.0c says corrallm publishes OpenAI's shape "wherever OpenAI
  has published one", and OpenAI published one here. **§9.1's line "The event
  sequence is OpenAI's, not invented" is true of the three event types and not of
  the terminator.** Filing the divergence, not proposing the removal — §12.10.2
  chose `[DONE]` deliberately and the owner may keep it as a documented extension.
  It should be labelled as one.
- **Verified:** `openapi.yaml` (`CreateTranscriptionResponseStreamEvent`, and the
  SSE example under `/audio/transcriptions`), cross-checked against the rendered
  reference page for `create-transcription`. Not probed on the wire.
- **Related, and NOT a divergence:** the `{type:"duration",seconds}` usage block
  on `transcript.text.done` is conformant — OpenAI's `usage` there accepts both
  the token and the duration shape. §9.0b's decision to make it optional stands.

### 2.6 `segments[].id` has one type on the stream and the backend's type off it
- **Where:** synthesized events stringify the id (`sttstream.go:395`,
  `jsonScalarString` at `:425`); the non-streaming body is forwarded verbatim
  (manifest, `proxy.go:2406`: "corrallm forwards the transcription body verbatim").
- **OpenAI:** integer in `verbose_json`, string in `diarized_json` — the
  contradiction §9.0b(c) resolved by picking string.
- **corrallm:** string when `stream=true`; whatever oidio emits when `stream=false`.
  §9.0b(c)'s "ONE type, everywhere" is implemented on the stream only.
- **Verified:** read the code; OpenAI side from `openapi.yaml`
  (`TranscriptionSegment.id` integer, `TranscriptionDiarizedSegment.id` string).
  Not probed — see "could not verify".

### 2.7 `/v1/models` adds ten fields and omits `shutdown_date`
- **Where:** `proxy.go:2140-2168`.
- **Extra:** `state`, `quality`, `type`, `kind`, `remote`, `members`,
  `persistent`, `context_length`, `slots`, `modalities`, `outputs`, `capability`.
- **Assessment:** not a break — OpenAI clients ignore unknown keys, the four
  required fields are present and correct, and the manifest documents the
  enrichment. Listed for completeness. `shutdown_date` (nullable in the current
  spec) is absent, which is also fine — it is not required.
- **Verified:** probed `:8111`; OpenAI side from `openapi.yaml` (`Model`).

---

## Bucket 3 — claims something untrue

### 3.1 The manifest advertises models the proxy refuses 404
- **Where:** `byCap` is built from `p.config().Models` with **no `Disabled`
  filter** (`proxy.go:2296-2303`), while `/v1/models` uses `AllModels()`, which
  skips disabled models (`internal/config/config.go:312-315`).
- **On the live box:** `groq-gpt-oss-120b` appears in `/v1/capabilities`
  `models_by_capability.chat` and in the `models` list of both
  `/v1/chat/completions` and `/v1/completions`. It is absent from `/v1/models`.
  Sending it returns `404 … is disabled in this configuration`.
- **Why it is a §9.0b breach, not a cosmetic one:** the manifest is the thing a
  client is told to build against ("Point any OpenAI client at this base URL"),
  and the per-endpoint `models` array is the most directly actionable claim in it.
- **The same loop has the opposite bug:** it iterates `Models` only, so a
  *discovered* model is served and listed on `/v1/models` but never appears in the
  manifest's capability lists.
- **Verified:** probed `:8111` (manifest, catalog, and the 404); read both loops.

### 3.2 The README still promises `num_speakers` and an integer `speaker`
- **Where:** `README.md:221-223`:
  ```jsonc
  { "text": "…",
    "segments": [{ "speaker": 0, "start": 0.0, "end": 3.3, "text": "…" }],
    "num_speakers": 2 }
  ```
- **What the code says:** `proxy.go:2406` — "There is no `num_speakers`: take
  `speakers.length`", and `speaker` is "a UUID minted PER REQUEST (not an index)".
- **This is the third false claim of §9.0b's list, still live in a second file.**
  `proxy_test.go:1792-1799` guards the manifest against exactly this string.
  Nothing guards the README, so the correction landed in one of the two places a
  client reads.
- **Verified:** `grep -rn num_speakers` over the repo — `README.md:223` is the only
  remaining promise of it outside the retraction and its test.

### 3.3 The README documents a `modes` config field that does not exist
- **Where:** `README.md:212-215` — "STT models declare a delivery `modes` list of
  `batch`, `realtime`, or both (empty means unrestricted)."
- **What the code says:** `proxy.go:2313` — "No `modes` field; the cost type
  decides." `grep -rn modes internal/config/` finds no such field.
- **Verified:** grep over `internal/config` and the proxy.

### 3.4 `/v1/audio/speech` is advertised as `streaming: true`, and nothing streams it
- **Where:** the endpoint tuple at `proxy.go:2416` sets `Streaming: true`. The
  same manifest's transcription note (`proxy.go:2383`) ends: "Not applicable to
  `/v1/audio/speech`, which returns binary audio rather than events." The manifest
  contradicts itself.
- **OpenAI:** TTS streaming is `stream_format: "sse"` → `text/event-stream` of
  `speech.audio.delta` / `speech.audio.done`. **`stream_format` appears nowhere in
  this repo** (`grep -rn stream_format --include=*.go` → no hits), and
  `handleInference` reads only the JSON `stream` boolean (`proxy.go:2846`), which
  OpenAI's speech request does not define.
- **Status:** the contradiction is proven; whether a client asking for
  `stream_format: sse` gets SSE depends on the backend and was **not probed** (see
  below).
- **Verified:** read the code and the manifest; OpenAI side from `openapi.yaml`
  (`CreateSpeechRequest.stream_format`, and the `200` response's
  `text/event-stream` variant).

### 3.5 The realtime manifest documents the deprecated beta session shape
- **Where:** `proxy.go:2427-2437` — `ws_url` carries `&intent=transcription`, and
  the documented flow is
  `{"type":"session.update","session":{"input_audio_transcription":{…},
  "turn_detection":{"type":"server_vad"}}}`.
- **OpenAI today:** the transcription session is opened by `session.update` with
  `session.type: "transcription"` and the transcription model under
  `session.audio.input.transcription.model`. `?intent=` appears in neither the
  current spec nor the current realtime guides.
- **Honest framing:** corrallm is a byte pipe here (`proxyWebSocket`,
  `proxy.go:1477`) — it rewrites `?model=` and forwards everything else. So
  "OpenAI Realtime transcription schema" is a claim about the backend, and the
  flow block is a claim corrallm makes on the backend's behalf. It currently
  describes the beta shape.
- **Verified:** `openapi.yaml` realtime paths + the `realtime-websocket` and
  `realtime-transcription` guides. Not probed — no realtime session was opened.

### 3.6 `/v1/completions` is advertised for models that do not serve it
- **Where:** `proxy.go:2365` lists `byCap["chat"]`, which includes `claude-*` and
  the remote chat proxies.
- **corrallm's own config says otherwise:** `internal/config/presets.go:201` —
  "Via Anthropic's OpenAI-compatible layer at `/v1/chat/completions`. The native
  Messages API is a different surface and is not what this reaches." A legacy
  completions request to that target has no route to land on.
- **Nothing enforces it:** `routeEmits` (`proxy.go:1708`) gates
  `/v1/images/generations` only, and `proxy.go:770-775` says so explicitly — the
  other routes "have always forwarded whatever model they were handed".
- **Status:** the Anthropic side is **not probed** (it would spend the owner's
  credential). The inconsistency between the manifest and the preset's own note
  is verified; the upstream 404 is inferred from it.

### 3.7 Undocumented behaviour the normative manifest should carry
Each is real behaviour with no claim against it — the gap, not a lie:
- **`max_tokens` is injected when the caller set neither spelling**
  (`proxy.go:1976-1981`), capping a request OpenAI would leave unbounded. Only
  when the model config sets `MaxTokens`. Nothing in the manifest mentions it.
- **415 and 413 on audio** (`audio.go:259`, `:296`, `:333`), including a **30
  minute** conversion cap (`defaultTranscodeMaxSeconds = 1800`, `audio.go:72`).
  §9.0b defends these as "refusing at the door"; "document strictly" means the
  manifest should name them, and it does not.
- **The auth block** (`proxy.go:2471-2474`) says "unkeyed callers use the default
  lane" unconditionally. With the unknown-keys policy on, an unrecognized caller
  gets 401 (`proxy.go:705-711`). The claim is true only in the default config.
- **The manifest omits `/v1/messages/count_tokens`** entirely, though it is
  mounted (`proxy.go:539`) and the manifest presents itself as the endpoint list.

---

## Not findings: the non-OpenAI routes

`/v1/rerank`, `/v1/capabilities`, `/v1/reservations` and
`/v1/messages/count_tokens` are not OpenAI routes. The question for them is
collision, not conformance.

**No collision.** The full path index of `openai-openapi` v2.3.0 (224 entries)
contains no `/rerank`, no `/capabilities`, no `/reservations` and no `/messages`
of any kind. `/v1/messages/count_tokens` is Anthropic's spelling of an Anthropic
route, proxied to an Anthropic target — correct as-is.

Two notes that are not conformance findings but belong beside them:
- `/v1/realtime` **is** an OpenAI path, and OpenAI also defines
  `/v1/realtime/calls`, `/v1/realtime/client_secrets`,
  `/v1/realtime/transcription_sessions` and `/v1/realtime/sessions`. corrallm
  mounts none of those, so they land on the SPA (finding 1.2).
- count_tokens returns plain-text errors (finding 2.2). Anthropic's envelope is
  `{"type":"error","error":{"type":…,"message":…}}`; the route matches neither
  vendor's error shape.

---

## Could not verify, and why

- **A successful `/v1/chat/completions` body.** Two small probes against `:8111`
  both returned 429 — the box's one chat slot was busy for the whole audit window.
  So these remain **code-read only**: the response `model` field is the
  **upstream's** id, not the served name, because the request's `model` is
  rewritten on the way out (`rewriteModelField`, `proxy.go:1872`) and the response
  body is never touched (`statusCapture.Write`, `proxy.go:3034`, passes bytes through
  unmodified). A client that asks for `chat` and echoes `response.model` back into
  its next request would send a name `/v1/models` never listed. Unverified on the
  wire; also unverified: `finish_reason` spellings, `object` values, and the
  `usage` block the backends emit.
- **`/v1/embeddings` on the wire.** `local-nomic-embed-text` is `absent`; probing
  it would cold-load a model on the owner's live box. `encoding_format: "base64"`
  is doubly unverified — OpenAI's own spec does not model the base64 response
  shape, and its embeddings guide never mentions base64.
- **Any live audio call.** Transcription, translation and TTS all spend GPU. So
  the diarization note's field list (`segments[].overlap`, the 512-float
  `embedding`, `similarity`, `clean_seconds`, `blended`), the "no `logprobs` and
  no `languages`" absence claim, the `verbose_json` word shape, and whether
  `stream_format: sse` yields SSE on `/v1/audio/speech` (3.4) are all **read, not
  probed**. The `examples/audio/speech.wav` claim IS verified: the file exists and
  is 16-bit PCM, mono, 24 kHz, 2.56 s — the one manifest claim of that family that
  now checks out.
- **`/v1/images/generations` on the wire.** A generation is tens of seconds of
  GPU. Whether sd-server returns OpenAI's required `created` alongside
  `data[].b64_json` is therefore unknown — `images_test.go:67` asserts against a
  fixture the test itself writes, which is evidence about the test, not about
  sd-server. The manifest's shape claim (`proxy.go:2419`) names only
  `{"data":[{"b64_json":…}]}` and omits `created`.
- **`/v1/realtime` end to end.** No WebSocket session was opened.
- **Anthropic and Groq upstream behaviour** (3.6). Not probed — it spends the
  owner's credentials.
- **openai-python / openai-node failure modes.** The consequences in 1.1, 2.1 and
  2.3 are argued from the SDKs' documented behaviour and from the wire bodies, not
  from running either SDK against `:8111`.
- **Prod is ~18 commits behind main.** Every wire probe above was re-checked
  against main's source, and each finding cites the line in main. Where prod and
  main differ (prod's manifest still ships `num_speakers`; prod marks
  `/v1/audio/translations` non-streaming), the finding is written from **main**.

## Ranking, in one line each

1. **2.1** — the 404 body is unparseable JSON, and caller-controlled.
2. **1.1 / 1.2** — 200 + HTML on every `/v1` path corrallm does not mount.
3. **2.2 / 2.3** — `text/plain` errors with no `type`/`param`/`code`.
4. **1.3** — oversize bodies truncated, then blamed on a field that was there.
5. **3.1** — the normative manifest advertises models that 404.
6. **1.5** — malformed JSON reported as a missing `model`.
7. **3.2 / 3.3** — the README contradicts the corrected manifest, twice.
8. **1.6** — the audio upload's declared Content-Type is dropped.
9. **2.4** — `type: "backpressure"` inside OpenAI's typed field.
10. **2.5 / 2.6 / 3.4 / 3.5 / 3.6 / 3.7** — published shapes and claims that
    diverge from OpenAI's or from this repo's own code.
