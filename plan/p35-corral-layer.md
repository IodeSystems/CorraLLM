# P35 — the corral layer: transcode in, remap out

> Opened 2026-09-23, from the owner: *"we also need a transcode layer into it, so
> that we can maintain compatibility, and we need a remapping layer out for the same
> reason."*
>
> The framing, his words, earlier: *"speech, like image generation, has odd,
> non-standard backends, and we intend to 'corral' them into a standard OpenAI
> interface."*
>
> **Phase 1 (transcode in) is BUILT and verified live — see §7.** Of the remap out,
> **contract 2 (`stream=true` on STT) is BUILT — §9.1 — and contract 1 (the canonical
> transcription body) is BUILT — §12, ratified by §9.0c and shipped 2026-09-23. What
> actually ships, with its measurements and its gaps, is §12.11.**
>
> **Still open and still design only: contract 3** (`/v1/audio/translations` does not
> translate), **contract 4** (`response_format: url` on images) and **contract 5**
> (`/v1/realtime`'s hardcoded audio format). Each carries a policy call that is the
> owner's — `open-questions.md` §5, items 2, 5 and 6 — and §12.9(a) did NOT unblock
> them: they were never waiting on the schema.

## 0. The problem in one sentence

corrallm publishes OpenAI's shape and forwards a backend's own, so on both edges the
caller is talking to the backend and not to corrallm: **what a client sends may be
unreadable going in, and what a backend answers may be unrecognisable coming out.**

## 1. Going in: a format the backend cannot read

**The API's input set is not a suggestion.** `/v1/audio/transcriptions` documents flac,
mp3, mp4, mpeg, mpga, m4a, ogg, wav and webm, and the official client uploads whatever
the user handed it. A browser recording arrives as webm/opus; a phone's voice memo
arrives as m4a.

**The next backend refuses most of that before reading a sample.** NVIDIA's
NeMo-Speech.cpp — the likely next audio backend, currently on an unmerged PR — rejects
anything that is not RIFF/WAVE (`src/common/audio_file.cpp:38`), and then only PCM
format 1 at 16-bit or format 3 at 32-bit float, mono or stereo, 8–96 kHz (`:89`).
Point the official client at it with an mp3 and the request fails inside somebody
else's program, with somebody else's message, about an encoding the caller never chose.

**oidio accepts all of it.** Verified on the wire against the live `:8111`,
2026-09-23: wav, mp3, m4a, flac, webm and a 48 kHz stereo 24-bit wav each returned 200
with the correct text from `oidio-stt`. So this is **per-backend capability, not a
blanket transform** — normalising for oidio would spend CPU to make a request no
better, and would hand a resampled file to a model that was happy with the original.

**The consequence for the design:** corrallm must know what a backend ingests *before*
it touches the bytes, and the answer has to be able to be "everything".

## 2. Where that knowledge comes from — an existing field, one level down

**`config.ModalitySpec.Formats`, and nothing new.** The field exists
(`internal/config/config.go:1038`), its own comment already reads *"image/audio:
accepted encodings"*, it is declarable per model under `modalities:`, it is already
overridden by probe results through `config.ProbedModalities` (`config.go:1063`), and
it is already published: `/v1/models` on the live box serves
`"image":{"formats":["png","jpeg"]}` today.

`config.IO` (`internal/config/iomodality.go`) names input modalities coarsely — a model
`Accepts(ModalityAudio)`. **Audio format is the finer grain UNDER that modality**, and
it is the same field one level down rather than a parallel concept. Three things were
considered and rejected:

- **A new `accepts:`/`transcode:` block per model.** Rejected: it would duplicate a
  field that exists, and the two could disagree with nothing to say which wins.
- **Format names inside `IO.In`** (`in: [audio/mp3, text]`). Rejected: it breaks
  `Accepts(ModalityAudio)` for every existing caller, and makes the modality vocabulary
  ambiguous — `containsMod` would need prefix matching, and then "audio" both is and is
  not a member of the list.
- **Inferring the restriction from the cost `type`.** Rejected: oidio and NeMo are both
  `type: stt` and they are the two ends of this problem. The type cannot carry it.

**An empty list means no constraint is known**, which is the honest reading of silence
and is what keeps the layer invisible until an operator describes a picky backend.

**The gap this leaves, named rather than hidden.** The declaration is per CONTAINER.
NeMo's second check is on the PCM parameters, and a 48 kHz stereo 24-bit wav passes a
`formats: [wav]` declaration untouched and would still be refused by the real backend.
Proven in §7's last row. Closing it means `ModalitySpec` growing sample-rate/bit-depth
bounds — a phase 2 item, deliberately not guessed at now.

## 3. When it converts, and what that costs

**Only when the backend cannot take what arrived.** Always-normalise is simpler and
wrong twice: it spends the CPU on every oidio request that never needed it, and it
re-encodes a file the backend would have decoded better itself.

**What arrived is decided by the magic number, never the filename.** A client picks the
filename; the backend's reader looks at the bytes. An unidentified container counts as
"not one of the declared names" — converting something that turns out to have been
acceptable costs CPU, while forwarding something unidentified costs the caller their
request.

**Target: 16 kHz mono PCM s16 WAV.** Every ASR frontend in this class (NeMo,
whisper.cpp, parakeet) resamples to 16 kHz internally, so more is bytes the backend
discards; WAV is the one container a format-restricted STT backend is restricted *to*;
and mono halves the payload. It also makes the size arithmetic rather than a guess:
**32,000 bytes per second of audio, whatever arrived.**

**Measured on box1, 2026-09-23:**

| input | what it costs |
|---|---|
| 2.56 s wav → mp3/m4a/flac/webm, end to end through the proxy | **112–135 ms** (§7) |
| 30 min mp3 (7.2 MB) → 16 kHz mono wav | **4.24 s**, producing **57.6 MB** — 425x realtime |
| 2 h m4a (29.9 MB) | **refused in 0.2 s**, never decoded |

**The bound is the one that already exists.** 1,800 s at 16 kHz is 57.6 MB, just inside
the 64 MiB cap `handleInference` already puts on an STT upload. Over that is **413 with
the number in it**, never a truncation — and the refusal is priced off `ffprobe`, so a
2-hour upload is turned away before any decoding happens. A file whose duration ffprobe
will not state is bounded by ffmpeg's own `-fs` on the output, which is the same limit
measured on the artifact instead of on the claim.

## 4. Where it sits: before admission

**Beside the PDF rewrite** (`internal/proxy/proxy.go`, immediately after
`convertChatPDFs`), which is **before** `beginInflight` and before the `sched.Admit`
that takes a slot. `internal/proxy/pdf.go` already chose this placement and it is
followed rather than re-argued, with one reason added that is specific to audio:

**the three refusals in §5 are validation.** A caller whose file was never going to be
servable must not queue behind a saturated backend first in order to find that out.

**The cost of that placement, said out loud:** a request later refused for capacity has
already spent its transcode, and will spend it again when the caller retries. At 4.24 s
for a 30-minute file that is real. The alternative — hold a slot while shelling out to
ffmpeg — spends a *scarcer* resource (a local STT backend has 4 slots; the GPU chat
models have 1) and delays the validation. Taken deliberately; reversible.

**Resolved against a lane's FIRST member**, exactly as `Config.ConvertFor` does, because
the rewrite happens once rather than per candidate in the fall-through walk. The
mismatch that leaves is narrow: since the target is always WAV, converting for a strict
first member produces something every other candidate also accepts. The uncovered case
is the reverse — a permissive first member and a strict fallback — where the fallback
still refuses. Filed as a known gap, not fixed by guessing.

## 5. What happens when it cannot convert

`pdf.go` leaves an unreadable PDF in the request, and that is right *there*: the model
reads the rest of the message and says it could not open the attachment. **An STT
request has no rest.** A silent pass-through that then 400s inside the backend is the
worst of the available outcomes, so this layer diverges from its sibling and answers
itself:

| condition | status | what the caller reads |
|---|---|---|
| ffmpeg absent and a conversion is needed | **415** | *"audio is mp3 and this model accepts only wav; corrallm can convert it but ffmpeg is not installed on this host"* |
| longer than the cap | **413** | *"audio is 7200s long and corrallm converts at most 1800s of audio per request; send a shorter file, or split it"* |
| ffmpeg cannot decode it | **400** | the format it was sniffed as, plus the tail of ffmpeg's own stderr |
| ffmpeg wedged past 2 minutes | **504** | the timeout |

All four are live-verified (§7). The 415 in particular names a **configuration** fault
— the operator declared a restriction and did not install the tool that honours it —
which is a thing a 400 from inside the backend could never say.

## 6. What it is observable as

- **`X-Corrallm-Transcode-Ms` on every proxied response**, zero when nothing happened.
  Its own header rather than a share of `loadMS`, for the reason the timing set exists:
  a transcode is neither queueing nor loading nor execution, it scales with the length
  of the audio and not with the model, and folded into `loadMS` it would make
  `llm-bench` read a 4-second ffmpeg as a slow backend. Same argument as P34 §8.4.
- **One `slog.Info` per actual transcode**, with in/out bytes and ms. CPU spent on a
  caller's latency, and an unexplained pause is the mystery the headers exist to stop
  creating.
- **One `slog.Warn` per refusal**, with the status.

## 7. Phase 1, as built — and the live before/after

**Built:** `internal/proxy/audio.go`, `ConvertConfig.Audio*`, `--transcode-audio` /
`--transcode-audio-max-seconds`, `X-Corrallm-Transcode-Ms`.

**The rig.** The live `:8111` was not touched. A second corrallm ran on `:8199` against
a stub that reproduces exactly one NeMo behaviour — refuse anything that is not
RIFF/WAVE, `audio_file.cpp:38` — and forwards whatever it accepts to the REAL
`oidio-stt` on `:8111`, so an accepted request produces a real transcription and not a
canned string. Two models point at that same stub: `nemo-sim` declares
`modalities: {audio: {formats: [wav]}}`, `nemo-undeclared` declares nothing.

**Before** (`--transcode-audio=false`), against `nemo-sim`:

```
speech.mp3   400  {"error":{"message":"failed to open audio file: not a RIFF/WAVE file (magic 'ID3\x04')"}}
speech.m4a   400  … (magic '\x00\x00\x00\x1c')
speech.flac  400  … (magic 'fLaC')
speech.webm  400  … (magic '\x1aE\xdf\xa3')
```

**After** (the layer on, its default):

```
speech.mp3   200  X-Corrallm-Transcode-Ms: 135  {"text":"The quick brown fox jumps over the lazy dog."}
speech.m4a   200  X-Corrallm-Transcode-Ms: 122  {"text":"The quick brown fox jumps over the lazy dog."}
speech.flac  200  X-Corrallm-Transcode-Ms: 112  {"text":"The quick brown fox jumps over the lazy dog."}
speech.webm  200  X-Corrallm-Transcode-Ms: 123  {"text":"The quick brown fox jumps over the lazy dog."}
```

**And the three things that prove it is not a blanket transform:**

```
mp3  → nemo-undeclared   400, Transcode-Ms: 0   same stub, same file: only the declaration differs
2h m4a (29.9 MB)         413 in 0.205 s          refused off ffprobe, never decoded
mp3, ffmpeg off PATH     415                     "…corrallm can convert it but ffmpeg is not installed on this host"
48 kHz stereo 24-bit wav 200, Transcode-Ms: 0    the §2 gap, visible: the container matched, the PCM was not checked
```

## 8. Coming out: five contracts, each verified — TWO now built, three still open

~~none implemented~~. **1 and 2 are BUILT** (§12.11 and §9.1). **3, 4 and 5 are
deliberately not built**, and each still needs corrallm to *decide* something that is
the owner's call — `open-questions.md` §5 items 2, 5 and 6.

1. ✅ **BUILT 2026-09-23.** **Diarization shape differs per backend.** oidio returns
   `segments[]{id,start,end,text,speaker:UUID,overlap?}` plus top-level
   `speakers[]{uuid,embedding[512],similarity{uuid→cosine},clean_seconds,total_seconds,blended?}`
   and `words[]{word,start,speaker}` with **no `end`** — and **the UUID is minted per
   request**: three calls on the same audio gave three different ones. NeMo-Speech.cpp
   instead puts an integer `speaker` on `words[]` (verbose_json only; 400 otherwise),
   or returns textless `{"segments":[{start,end,speaker:int}]}`, and never serialises
   `num_speakers` at all. ~~Normalising NeMo onto oidio's shape means joining words
   into speaker turns~~ — and the premise there was wrong twice over: neither shape is
   canonical (§9.0), and the join is arithmetic on returned data rather than an
   invention (§9.0's own boundary). **Both are now mapped onto OpenAI's
   `diarized_json`; the join is RULE 2 in §12.9(e) and the NeMo half of it is
   unit-covered only, because nothing on this box runs NeMo-Speech.cpp.**
2. ✅ **BUILT.** **`stream=true` is silently ignored on oidio's diarize path.**
   `transcriptions.go:45` dispatches to `handleDiarize` before the stream check, and
   `handleDiarize` never reads it. **Re-verified on the wire 2026-09-23:** `stream=true`
   to `oidio-stt-diarize` returned `200` with `Content-Type: application/json` and a
   complete non-SSE body. Three options and the current one was the worst.
   **The DIARIZE path only** — `oidio-stt` streams for real, which §12.10 records.
   corrallm now synthesises the events; see §9.1 for what shipped and what it costs.
3. **`/v1/audio/translations` does not translate.** oidio maps it straight onto
   `handleTranscriptions`; it transcribes in the source language. The shape claim is
   true and the behaviour claim is false.
4. **`/v1/images/generations` only ever emits `b64_json`.** OpenAI defaults to `url` and
   honours `response_format`.
5. **`/v1/realtime` hardcodes "PCM16 mono @ 24 kHz".** That is oidio's default
   (`engine/realtime.go:56`) but is per-model configurable, and its WebRTC transport
   decodes Opus at 48 kHz regardless.

**Where the remap would live, if it lives.** The symmetric position to §4: a response
transform beside `ModifyResponse`, keyed on the same per-model capability record that
`ModalitySpec` already is. The hard part is not the plumbing — it is that (2) and (3)
require synthesis, and synthesis needs a decision.

## 9. Decisions the owner owns

### 9.0 The governing principle — answered 2026-09-23

> "We aim to take the mish mash of non standard audio tools and corrall them into a
> common, portable API."

**No backend's shape is canonical. corrallm publishes one, and every backend is mapped
onto it.** This is the project's purpose, stated by the owner, and it is not a
per-question preference — it decides the FRAMING of every question below rather than
any one of their answers.

What it settles, and what it does not:

- **§9.3 was posed wrongly.** "oidio's shape or NeMo's?" has no third column in it, and
  the third column is the answer. Both are mapped onto corrallm's. The lossiness the
  question described is real and does not go away — it moves. Mapping oidio onto a
  canonical shape still drops or relocates the embeddings; mapping NeMo onto it still
  joins words into turns. What changes is that neither backend gets to impose its
  accidents on the other, and a third backend costs one mapping instead of N.
- **It does NOT decide the schema.** "corrallm's shape" is a mandate to define one, not
  a definition. That is design work, and it is §9.3a below.
- **It raises the bar on synthesis, it does not lower it.** A portable API may compute
  what a backend did not return — that is the job. It may not INVENT what no backend
  knows. Joining words into turns is arithmetic on data NeMo returned. Stabilising a
  speaker across calls (§9.4) is speaker recognition, and no amount of portability
  makes that a remap.

### 9.0b The robustness principle — answered 2026-09-23

> "I've found that these various implementations have differences. I prefer the
> Berkeley method: accept generously, produce and document strictly."

The robustness principle, applied to a corral layer. §9.0 said corrallm publishes one
shape; this says what publishing owes, and it is sharper than §9.0 because it is
ASYMMETRIC — the tolerance is on the way in only.

**In: accept every form of the same request.** Any container a backend cannot read
(phase 1). `stream=true` where the backend ignores it (§9.1). A backend's `id` as int
or as string. A parameter OpenAI defines that our backend spells differently.
Tolerance here costs one mapping and buys every client that was written against
someone else.

**Out: emit exactly one shape, and let the manifest be normative.** Not "usually this
shape". Not "this shape, plus whatever the backend added". Where two published specs
disagree, corrallm picks one, emits it everywhere, and says which — mirroring an
upstream's inconsistency is not compatibility, it is forwarding a bug with a new
hostname on it.

**Where it does NOT reach, and this is where it will be misapplied.** Accept generously
governs the FORM of a request, never a capability the model does not have. Accepting a
request for data no backend produced and answering it with something plausible is not
generosity — it is §9.0's "may not invent" wearing a friendlier word. Phase 1's 415 and
413 are not violations: they accept every format that CAN be converted and refuse, early
and by name, only what cannot be served at all. Refusing at the door beats accepting and
failing after the queue.

**What it settles outright:**

- **(f) `words[].end` when unreported → OMIT.** Computing it from the next word's start
  emits a number nobody measured. Produce-strictly forbids it.
- **(d) the `backend` sidecar → `include[]=backend`, opt-in.** The DEFAULT response is
  the documented canonical shape and nothing else. Extras are available, never ambient.
- **(c) `segments[].id` → ONE type, everywhere, documented.** OpenAI contradicts itself
  (string in the `transcript.text.segment` event, integer in `whisper-1`'s
  `verbose_json`); corrallm may not. Accept either from a backend, emit one. **String**,
  as §9.1 shipped, because the diarized event schema is the one actually specifying
  diarized output.
- **(g) `diarized_json` to a non-diarizing model → 400 stands.** This is the
  misapplication case above, and it is why that boundary is written down.
- **§12.10.2 — `data: [DONE]` — REOPENED, and ✅ SHIPPED 2026-09-23.** Filed as "outside
  §9.1", which was right at the time. Under produce-strictly it is not: corrallm's output
  terminates the same way whichever backend answered, so oidio's real stream gets
  normalised too. A client must be able to use `[DONE]` as its end-of-stream test on this
  route. This was the principle's first real cost — it meant touching a backend's own
  stream, which §9.1 deliberately did not. **What it actually cost, measured first:
  TWO divergences, not the one that was filed** — the missing terminator, and a
  `transcript.text.done` with no usage block. The second was answered by NOT
  normalising it: a real stream measured no duration, and computing one is what (f)
  already forbade, so `usage` becomes an explicitly optional field of the published
  contract. The shape the principle takes on a backend's own stream is COMPLETION —
  append what is missing, rewrite nothing, double nothing. Details in §12.10.2.

**What it sharpens but does not answer:**

- **(a)** one live answer remains rather than a decision made. Produce-strictly plus a
  portable API points at adopting a published spec over inventing one — but adopting
  `diarized_json` is still the owner's ratification, and still gates phases 3–4.
- **(b)** letters vs `speaker_0` stays open. What the principle adds: "what happens at
  27" belongs IN the schema, not discovered at runtime.
- **(e)** the join rule must be written and ratified, not observed. Restated, unchanged.
- **(h)** accept-generously leans toward taking `known_speaker_names[]` under OpenAI's
  name even where the mechanism beneath differs. Reserve the field now.

**A standing obligation, not a one-off.** "Document strictly" promotes the capabilities
manifest from description to specification. It has twice shipped claims that were false
(`speech.wav`, `num_speakers`). Under this principle that is a broken contract, not
sloppiness — the manifest is now what a client is entitled to rely on. **A THIRD was
found the moment the obligation was applied**: the `streaming` note claimed "no STT
backend on this box streams", which §12.10.1 had already disproved. Three false claims
in one manifest is a pattern, not three accidents — the note is now written as a
specification (§12.10.3), and the next one should be checked against the wire before it
ships rather than after.

### 9.0c The mission is OpenAI compatibility — ANSWERED 2026-09-23, and it closes §12.9(a)

> "Yes, project mission statement is openai compatible. Ensure that is the case."

**§12.9(a) is ratified: OpenAI's `diarized_json` IS the canonical shape.** Phases 3 and 4
are unblocked. This is the third and last of the framing decisions, and it removes §9.3
rather than answering it — corrallm does not invent a schema, it implements a published
one and maps every backend onto it.

Read together, the three now say one thing:

- **§9.0** — no backend's shape is canonical; corrallm publishes one.
- **§9.0b** — accept generously; produce and document strictly.
- **§9.0c** — and the one it publishes is OpenAI's, wherever OpenAI has published one.

**What ratification settles beyond (a), because the answer is now "whatever OpenAI does":**

- **(b) speaker labels → `"A"`, `"B"`, …** OpenAI's. The 27-speaker rollover still needs a
  written rule (§9.0b put it in the schema, not in a runtime surprise), but the label
  FORM is no longer a choice.
- **(h) `known_speaker_names[]` → accept it.** OpenAI defines it; accept-generously plus
  OpenAI-compatibility both point the same way. Mapping it onto oidio's UUID-based
  `known_speakers` is a separate piece of work and may be refused for now — but the
  parameter name is OpenAI's and corrallm must not spell it differently.

**Still a real rule to write, not a choice to make:** **(e)** the NeMo word→segment join.
OpenAI says what the OUTPUT looks like; it says nothing about how to build a segment from
a backend that only returns words. The rule is ours, it must be written into the schema,
and it must be ratified as a rule rather than discovered as behaviour.

**"Ensure that is the case" is the larger half of this instruction.** It is not scoped to
diarization or to audio. Every surface corrallm serves is a compatibility claim, and the
manifest is now normative (§9.0b), so an unaudited claim is a liability rather than a
gap. Tracked as §13.

### 9.1 `stream=true` on a path that cannot stream — ANSWERED 2026-09-23, and BUILT

**Synthesise SSE from the complete body**, on `/v1/audio/transcriptions` and
`/v1/audio/translations`. The client set a flag the spec defines; a corral layer that
drops it is the silently-wrong option, and refusing 400 punishes a caller for defensive
correctness. The latency benefit is a fiction and must be documented as one — the body
is complete before the first event goes out.

**As built:** `internal/proxy/sttstream.go`. The event sequence is OpenAI's, not
invented — `transcript.text.delta` (with `segment_id` when the response is
speaker-labelled), `transcript.text.segment` per diarized segment, then
`transcript.text.done` with the whole transcript and a `{type:"duration",seconds}`
usage block, terminated by `data: [DONE]`.

**The fiction, measured.** Against the real `oidio-stt-diarize` through a second
corrallm on `:6510`, `examples/audio/speech.wav`, three runs:

```
stream=true   ttfb 0.296 s  total 0.296 s   442 bytes
stream=false  ttfb 0.301 s  total 0.302 s  5640 bytes
stream=true   ttfb 0.340 s  total 0.340 s   442 bytes
stream=false  ttfb 0.330 s  total 0.331 s  5640 bytes
stream=true   ttfb 0.392 s  total 0.392 s   442 bytes
stream=false  ttfb 0.298 s  total 0.299 s  5640 bytes
```

**Time-to-first-event equals time-to-whole-answer, every run, the widest gap 0.9 ms.**
That is the fiction in one number: there is no progressive delivery and no
time-to-first-word. What it buys is one client code path. The sentence is in the code,
in `/v1/capabilities`, and in §12.8.

**And it is LOSSY: 5,640 bytes become 442.** The event schema has no carrier for
`language`, `task`, `words[]` or `speakers[]`. The manifest tells a caller to set
`stream=false` to keep them.

**A real stream's EVENTS are untouched**, keyed on the backend's own `Content-Type`.
The live box has one — `oidio-stt` (§12.10) — and its events pass through byte for
byte, unbuffered. ~~including its own missing `[DONE]`~~ — that half expired: §9.0b
reopened it and §12.10.2 shipped the fix, so a real stream is now COMPLETED with the
terminator it omits. Nothing it did send is altered. Synthesis is still the fallback
for a finished body, never a rewrite of every reply. An error stays a JSON error with
its own status.

**Metering is unchanged by construction.** The writer sits OUTSIDE `statusCapture`, so
the activity row still records the backend's body: verified live at 200, 123,388 audio
bytes, ttfb 298 ms, response payload = oidio's JSON.

**`/v1/audio/speech` is still open, and was NOT touched.** The question is per endpoint
by construction: TTS streams AUDIO chunks, not SSE events, and chunking a finished mp3
is a different mechanism with a different honesty problem. Do not let the transcription
answer travel there by momentum. `audioUploadRoute` excludes it structurally, and a
live `{"stream":true}` to `/v1/audio/speech` still returns `audio/mpeg`.

**Phase 1 (built) took three, all reversible and all argued in the commits:** convert
only on a declared mismatch · convert before admission · refuse rather than pass
through. **Still open, and phase 4 must not start on these before they are answered:**
2. **`/v1/audio/translations`.** Route it to a translating backend, refuse it as
   unimplemented, or keep transcribing and document the lie. ~~Corrallm could also
   translate the transcript with a chat model — a real option, and one that changes what
   the endpoint costs.~~ **CORRECTED 2026-09-23, by the owner: the chat-model option is
   dominated and should not have been listed as real.** NeMo-Speech.cpp translates
   NATIVELY — `src/nmt/translator.h` wraps llama.cpp to run the Riva Translate 4B
   Instruct v2 decoder, `src/speech/speech_translator.h` does streaming speech
   translation, and its README lists composed ASR→NMT→TTS as a first-class application.
   Routing audio through a general chat model for a job a purpose-built NMT does inside
   the backend we are already adopting costs chat tokens for a worse answer. **This does
   not close the question** — oidio is what serves the route TODAY and it does not
   translate, so "refuse" and "document the lie" are still live for the present. It
   removes one option and makes the answer conditional on NeMo landing (`~/inflight`,
   PR unmerged).
3. **Diarization: whose shape is canonical?** ~~OpenAI has no diarization schema~~ —
   **that premise expired; see §12.0.** OpenAI now publishes `diarized_json` and a
   `transcript.text.segment` event, which makes "the OpenAI shape" an available answer
   and the third column §9.0 asked for. Folded into 3a rather than answered on its own.
3a. ✅ **The canonical schema itself — RATIFIED §9.0c and BUILT 2026-09-23: §12,
   as-built in §12.11.** All eight sub-choices in §12.9 are closed, and the two rules
   ratification left open are written: the 27-speaker rollover (§12.4, bijective
   base-26) and the NeMo word→segment join (§12.9(e), six clauses plus a 2b for a
   backend with no segments at all). The label is a string, `"A"`/`"B"` by order of
   first appearance, stable within one response and NOT across calls; the extras ride
   an opt-in `backend` sidecar (`include[]=backend`, 665 → 6,097 bytes measured);
   `words[].end` is omitted when unreported; a non-diarizing model's fields are absent
   and `diarized_json` to one is a 400.

4. **Is a per-request speaker UUID a bug to fix or a fact to expose?** It cannot be
   correlated across calls, so any client that treats it as an identity is already
   wrong. Corrallm could stabilise it from the embeddings — that is speaker
   recognition, a feature, not a remap.
5. **`response_format: url` on images.** Serving a URL means corrallm hosts the bytes:
   a store, a lifetime, and a retention policy. Worth it, or is `b64_json`-only
   documented as the limit?
6. **`/v1/realtime` audio format.** Report what the model is actually configured with
   (requires asking oidio), or keep the constant and document it?

## 10. What phase 1 did NOT build, and why

- **No remap out.** §8, §9 — it needs the decisions above.
- **No PCM-parameter checking.** §2's named gap. It needs `ModalitySpec` to grow
  sample-rate/bit-depth bounds, and no backend on this box has stated any.
- **No transcode for `/v1/audio/speech`.** JSON in, audio OUT — nothing arrives that
  needs converting. Converting what comes BACK is a remap-out question.
- **No probing of audio formats.** `ProbedModalities` already overrides declarations,
  so when something can be asked, this reads the answer with no change here. Nothing
  corrallm talks to can be asked today.
- **No per-candidate conversion.** §4. One rewrite, against the first lane member,
  matching `ConvertFor`.
- **No `ffmpeg` timeout flag.** The duration cap is the knob that changes what a
  transcode costs; a second timer bounding the same thing is P34 §1a's mistake.

## 11. Phases

**Phase 1 — transcode in. ✅ BUILT, live-verified (§7).**

**Phase 2 — the input capability told honestly.** PCM bounds on `ModalitySpec` so a
24-bit wav is caught by corrallm rather than by the backend (§2); the strict-fallback
case from §4; a route probe that asks a backend what it ingests instead of reading a
declaration. Independent of phase 3, and worth doing whether or not the remap happens.

**Phase 3 — remap out, the renames.** Contracts 4 and 5 (§8): field-for-field, no
synthesis, no decisions beyond §9.5 and §9.6. ❓ **STILL BLOCKED, and §9.0c did not
unblock it.** Ratifying `diarized_json` settled the AUDIO schema; contract 4 is
`response_format: url` on images (which means corrallm hosts bytes — a store, a
lifetime, a retention policy) and contract 5 is `/v1/realtime`'s advertised audio
format. Both are `open-questions.md` §5 items 5 and 6, both still open, and neither
was ever waiting on §12.

**Phase 4 — remap out, the synthesis.** ✅ **Contract 1 (the canonical transcription
body) is DONE** — §12, built 2026-09-23, as-built record in §12.11. **Contract 2 is
done** (§9.1). ❓ **Contract 3 — `/v1/audio/translations` does not translate — is
NOT done and is not started.** It is `open-questions.md` §5 item 2 and it is the one
place left on this route where the shape claim is true and the behaviour claim is
false: the canonical body now comes back from `/v1/audio/translations` field for
field, still reporting `"task":"transcribe"`, verified live 2026-09-23. **§9.4
(stabilising a speaker across calls) stays out of scope by §9.0's own rule.**

---

## 12. The canonical audio schema (§9.3a) — RATIFIED §9.0c, and ✅ BUILT 2026-09-23

~~DESIGN ONLY, the owner ratifies it~~ — ratified, then built. §12.9 is ruled on in
full (every sub-choice now carries its answer), the two rules the ratification left
open are WRITTEN (§12.4's rollover and §12.9(e)'s join), and the code is
`internal/proxy/sttremap.go`. **What actually shipped, what was measured on the wire,
and what is unit-covered only: §12.11.** Read that before trusting a number here —
§12.1–§12.9 are the design as argued, and §12.11 is the design as built.

Everything in §12.1–§12.3 was read off the wire on **2026-09-23** against the live
`oidio-stt` and `oidio-stt-diarize` (through a second corrallm on `:6510`; the
production `:8111` was not restarted or reconfigured). NeMo-Speech.cpp is read off
its source, because the PR is still unmerged and **nothing has been run against it**.

### 12.0 The premise §9.3 was written on is now FALSE

> §8.1 and §9.3: *"OpenAI has no diarization schema, so 'the OpenAI shape' does not
> settle it."*

**It does now.** OpenAI ships `gpt-4o-transcribe-diarize` with a
`response_format: diarized_json`, a `segments[]` of `{id, speaker, text, start,
end}`, speaker labels that are capital letters `A`, `B`, … unless the caller
supplied `known_speaker_names[]`, and a streaming event `transcript.text.segment`
carrying the same five fields. Read from OpenAI's published reference, not from the
wire — there is no OpenAI key on this box, so this is a documentation claim and it
is marked as one.

That changes the answer rather than the question. §9.0 says corrallm publishes one
shape and maps every backend onto it; it does **not** say corrallm invents one.
Where the API corrallm is compatible WITH has a name for a thing, that name is the
canonical name, and a third spelling would be the exact mish-mash §9.0 exists to
end.

**So the proposal is: the canonical shape IS OpenAI's, extended only where OpenAI
is silent, and every extension lives under one clearly-marked `backend` object.**

### 12.1 The shape

Three response formats, the first two unchanged from today.

**`response_format: json`** (the default) — untouched, forever:

```json
{"text": "The quick brown fox jumps over the lazy dog."}
```

A plain OpenAI client reads `.text` and is finished. No `speaker`, no `segments`, no
sidecar, on any model. This is the floor and it is already true on the box today
(`oidio-stt` returns exactly those 56 bytes).

**`response_format: verbose_json`**:

| field | type | when |
|---|---|---|
| `task` | string | always |
| `language` | string | ~~always~~ **when the backend IDENTIFIED one, or the caller named one** |
| `duration` | number (s) | always |
| `text` | string | always |
| `segments[]` | `{id, start, end, text}` + `speaker?` | when the backend segments |
| `words[]` | `{word, start}` + `end?` + `speaker?` | when the backend reports words |
| `usage` | `{type:"duration", seconds}` | ~~always~~ **when the backend MEASURED a duration** |

**Correction, made while building:** `usage` was written "always" here and that was
wrong by this document's own rule. §12.10.2 already made it optional in the streaming
form, for the reason §12.6 gives — a backend that measured no duration would force
corrallm to compute one. The non-streaming form must agree with the streaming form, so
`usage` is optional in both. oidio always reports a duration, so nothing on this box
sees the difference; a backend that does not would have made the manifest lie.

**Second correction, made after running a second backend (`p35-nemo-live.md` §5.2).**
`language` was written "always" and a backend that does not identify one now exists on
this box. NeMo-Speech.cpp echoes `""` when the caller named no language;
`canonTranscription.Language` is `omitempty`, so the key vanishes — and a client
reading `.language` got a string from oidio and `undefined` from NeMo on the same route
and the same format. This is the call `usage` already made, for the reason §9.0b(f)
gave for `words[].end`: emitting `"language": ""` would publish a language nobody
identified. So **`language` is OPTIONAL — present when the backend identified one or
the caller named one in the `language` form field, absent otherwise, and never guessed
by corrallm.** **Who reports it:** oidio, on every transcription; NeMo-Speech.cpp, only
back to a caller who sent `language=`. The remap is unchanged — the manifest was what
was wrong.

**`response_format: diarized_json`** — OpenAI's name, new to corrallm:

| field | type | when |
|---|---|---|
| `task`, `duration`, `text` | as above | always |
| `language`, `usage` | as above | **optional, exactly as above** |
| `segments[]` | `{id, speaker, text, start, end}` — `speaker` REQUIRED | always |
| `words[]` | as above, `speaker` present | when the backend reports words |
| `backend` | `{name, …}` | when the backend returned data the canonical has no home for |

### 12.2 oidio → canonical, field by field

Measured: `oidio-stt-diarize`, `response_format=verbose_json`, top-level keys
`duration, language, segments, speakers, task, text, words`.

| oidio | type on the wire | canonical | note |
|---|---|---|---|
| `text` | string | `text` | identity |
| `duration` | number | `duration`, and `usage.seconds` | identity |
| `language` | string | `language` | identity |
| `task` | string | `task` | identity |
| `segments[].id` | int (`0`) | `segments[].id` | **type change — see §12.9(c)** |
| `segments[].start` | number | `segments[].start` | identity |
| `segments[].end` | number | `segments[].end` | identity |
| `segments[].text` | string | `segments[].text` | identity |
| `segments[].speaker` | string, a **per-request UUID** | `segments[].speaker` = `"A"`, `"B"`, … | **relabelled — §12.4** |
| `segments[].overlap` | documented; absent from the sample | `backend.segments[].overlap` | no canonical home |
| `words[].word` | string | `words[].word` | identity |
| `words[].start` | number | `words[].start` | identity |
| *(no `words[].end`)* | — | **omitted** | oidio reports when a word STARTS, nothing more — §12.6 |
| `words[].speaker` | UUID string | `words[].speaker` = the same label | relabelled with segments |
| `speakers[].uuid` | string | `backend.speakers[].uuid` | the label's back-reference |
| `speakers[].embedding` | 512 floats | `backend.speakers[].embedding` | **§12.5** |
| `speakers[].similarity` | object UUID→cosine (`{}` in a one-speaker sample) | `backend.speakers[].similarity` | keys are UUIDs, so it travels with them |
| `speakers[].clean_seconds` | number (`2.5`) | `backend.speakers[].clean_seconds` | |
| `speakers[].total_seconds` | number (`2.5`) | `backend.speakers[].total_seconds` | |
| `speakers[].blended` | bool (`true`) | `backend.speakers[].blended` | |
| *(no `num_speakers`)* | — | not published | derived: count the distinct labels — §12.8 |

`backend.name` is `"oidio"`. `backend.speakers[]` carries the canonical `label`
alongside the `uuid`, so a caller that wants the voiceprint for speaker `"A"` has
one lookup and not a join on ordering.

### 12.3 NeMo-Speech.cpp → canonical, field by field

From the source, not the wire. Two shapes, because it has two.

| NeMo | canonical | note |
|---|---|---|
| `words[].word`, `.start`, `.end` | `words[]` same | identity; NeMo DOES report `end` |
| `words[].speaker` (int) | `words[].speaker` = `"A"`, `"B"`, … | int → label by order of first appearance |
| `{"segments":[{start,end,speaker:int}]}` — **textless** | `segments[]` with `text` | **§12.9(e): corrallm must fill `text`** |
| verbose_json ONLY (400 otherwise) | `json` must work | corrallm requests verbose_json upstream and projects down |
| *(never serialises `num_speakers`)* | not published | same derivation as oidio |

Two things NeMo forces that oidio does not:

1. **`json` has to be synthesised from `verbose_json`.** NeMo 400s on anything else,
   and `{"text": …}` is the floor every OpenAI client stands on. Projecting a
   verbose body down to its `text` is a field read, not an invention. corrallm
   sending `verbose_json` upstream regardless of what the caller asked is a
   request-side rewrite — the first one the remap layer needs, and it belongs in
   phase 3, not phase 4.
2. **A textless segment is not a segment.** The canonical requires `text` on
   `segments[]` because OpenAI's does. Joining the words whose `start` falls in
   `[segment.start, segment.end)` is arithmetic on data NeMo returned, which §9.0
   permits. Emitting `"text": ""` is the alternative and it is honest but useless.
   The join rule has to be WRITTEN DOWN or two implementations will disagree at a
   boundary; that is §12.9(e).

### 12.4 The speaker label: `string`, ordinal, stable within one response only

**Type: `string`.** OpenAI's is a string, and a string can later hold a
`known_speaker_names[]` value; an integer cannot without a breaking type change.

**Value: `"A"`, `"B"`, `"C"`, … by order of first appearance in the transcript.**
"Order of first appearance" is the BACKEND'S OWN DOCUMENT ORDER — every `segments[]`
entry in array order, then every `words[]` entry in array order. Nothing is sorted by
time: re-ordering a backend's arrays would be corrallm asserting a chronology the
backend did not.

**RULE 1 — THE 27-SPEAKER ROLLOVER. Bijective base-26: after `"Z"` come `"AA"`,
`"AB"`, … `"AZ"`, `"BA"`, … `"ZZ"`, `"AAA"`.** Spreadsheet columns. §9.0b said this
belongs IN the schema rather than in a runtime surprise, and this is it.

Why this one:

- It agrees with OpenAI **exactly** over the whole range OpenAI specifies (1–26). A
  scheme that diverges before 27 would be a different label form wearing OpenAI's
  clothes.
- One character class, no separator, unbounded. A client's parser does not need a
  second branch at 27.
- Every value is distinct. Unit-covered to 1,000 speakers, because the failure that
  matters is not "ugly at 27" but "two speakers share a label".

Rejected: **`speaker_27` after `Z`** — two label forms in one response, so every
client parses both forever. **Wrapping back to `"A"`** — two speakers, one label; the
schema lying, which is the specific failure §9.0's last paragraph names. **Refusing
the 27th speaker** — a refusal for a transcript the backend produced correctly.

**Not measured, and it cannot be honestly claimed as measured:** no 27-speaker
recording exists on this box. The rule is unit-tested and nothing more.

**Scope, stated in the manifest and not only here: STABLE WITHIN ONE RESPONSE,
EXPLICITLY NOT ACROSS CALLS.** `"A"` in two responses is two different assertions.
Nothing in this schema makes a speaker identity persist, and nothing should pretend
to: that is speaker recognition (§9.4), a feature, not a remap.

**What was rejected, and why:**

- **Pass oidio's UUID through as the label.** It is a 36-character opaque token that
  LOOKS like an identity and is not one — three calls on the same audio gave three
  different UUIDs when §8 was written, and this session saw three more
  (`72ca5ab1…`, `b05e81a3…`, `a30b0cc4…`) across three requests on the same 2.56 s
  file. A shape that invites a client to key a database on a value that changes
  every call is a shape that manufactures bugs. The UUID is not lost — it is
  `backend.speakers[].uuid`, where its scope is visible from where it sits.
- **NeMo's integer index.** Diverges from OpenAI for nothing, and closes the door on
  names.
- **`speaker_0` / `speaker_1`.** Same information as the letters, more characters,
  and not what the reference API prints. The only reason to prefer it is §12.9(b).

### 12.5 Backend extras: a `backend` sidecar, not a drop and not a top-level merge

oidio's `speakers[]` is **real data a caller may want**: 512 floats per speaker is a
voiceprint, and it is the only thing on the wire that could ever make a speaker
identity survive a second call. Dropping it forecloses §9.4 at the API level.

**It goes in `backend`, verbatim, under the backend's own name.** Not merged into the
top level, for one reason that decides it: a caller cannot tell a corrallm field
from a backend field once they are mixed, and the NEXT backend's `speakers[]` will
not mean the same thing as this one's. A namespace makes "this is oidio's, and it
may be gone tomorrow" a property of where the data sits rather than of a paragraph
someone has to have read.

**What it costs, measured.** The design wrote this as "5,640 bytes with the voiceprint
and 442 without" — which compared oidio's raw body against a SYNTHESISED STREAM, two
things that differ in more than the sidecar. **Re-measured against what actually
ships, same 2.56 s single-speaker file, `diarized_json` both times: 665 bytes without
the sidecar, 6,097 with it.** ~5.4 KB per speaker, scaling with speaker count and not
with audio length. The conclusion is unchanged and the number is now the right one.

**`backend.name` is the MODEL that served the request** (`oidio-stt-diarize`), not a
vendor string. §12.2 wrote `"oidio"`; corrallm has no vendor field to read, and
inventing one would be this manifest's fourth false claim. The served model name is a
fact corrallm knows for certain and a caller can correlate with `/v1/models`, and it
namespaces just as well — better, for a fleet running two builds of the same vendor.

**Rejected: drop the extras.** It is the cheapest option and it destroys the only
data that answers §9.4. A corral layer that narrows what a backend can say, with no
way to ask for it back, is a worse API than the mish-mash.

### 12.6 `words[]` is in the contract; `end` inside it is not

**In.** It is OpenAI's own field (`verbose_json` carries `words[{word,start,end}]`),
and both backends produce it — oidio on `verbose_json`, NeMo on `verbose_json`. The
question as posed in §9.3a ("only one backend produces it") was wrong on the facts.
Dropping a field the reference API publishes, to keep the contract narrow, is the
wrong direction for a compatibility layer.

**`end` is optional within it.** oidio reports only when a word STARTS — verified:
`{"word":"The","start":0,"speaker":"…"}`, no `end`, on every word of the sample.
A contract that REQUIRES `end` forces corrallm to invent a measurement.

**Rejected: compute `end` from the next word's `start`.** It reads like a
measurement and is a guess, and it is wrong exactly where it matters — a pause, a
breath, the end of a turn — because it silently attributes the silence to the
preceding word. §9.0 permits computing what a backend did not RETURN; it does not
permit inventing what no backend KNOWS, and oidio does not know when the word
ended.

**Rejected: emit `end: null`.** Same cost as omitting it, plus a null a client has
to branch on. Absent is the honest spelling of unknown.

### 12.7 What a plain, NON-diarizing STT model returns

Verified live against `oidio-stt` on the same file:

| request | today, on the wire | under the canonical |
|---|---|---|
| `json` | `{"text":"…"}` — 56 bytes, nothing else | **unchanged** |
| `verbose_json` | `{duration, language, segments:[{id,start,end,text}], task, text}` — 213 bytes | **unchanged**, plus `usage` |
| `diarized_json` | (does not exist) | **400**, naming the model and the formats it does serve |

**The rule: a field a model cannot answer is ABSENT, never null and never a
placeholder.** `segments[]` on a non-diarizing model has no `speaker` key at all.
`words[]` has no `speaker` key at all.

**Rejected: label the whole file `"A"` on a non-diarizing model.** It asserts "one
speaker", which nothing measured — a two-person interview transcribed by `oidio-stt`
would come back claiming one. That is the schema lying, and it is the specific
failure §9.0's last paragraph names.

**Rejected: degrade `diarized_json` to `verbose_json` silently.** The caller asked
for speaker labels. Returning a body with none, at 200, is the `stream=true` bug
this plan already has one instance of.

The streaming form agrees, and this part is BUILT: §9.1's synthesis emits
`transcript.text.segment` only for a segment that carries a speaker, so a plain
model's stream is deltas and a done, with no segment events. Verified on the wire.

### 12.8 Lossiness, in both directions, plainly

**Backend → canonical, if the sidecar ships:** the speaker UUID stops being the
label (recoverable from `backend.speakers[].uuid`), and `segments[].id` changes type
(§12.9(c)). Nothing else. **If the sidecar does NOT ship:** every voiceprint, the
similarity matrix, `clean_seconds`, `total_seconds`, `blended` and `overlap` are
destroyed — ~5.2 KB per speaker — and §9.4 becomes unanswerable from corrallm's API.

**Backend → canonical, for NeMo:** its textless segments gain a `text` corrallm
computed (§12.9(e)), and its integer speaker becomes a letter. The integer survives
in `backend.speakers[].index`. If the join rule is wrong at a boundary, a word lands
in the wrong turn and nothing in the response says so.

**Canonical → the wire on `stream=true`, and this is SHIPPED, not proposed:** the
event schema has no carrier for `language`, `task`, `words[]` or `speakers[]`, so a
synthesised stream drops all of them. ~~Measured: 5,640 bytes of JSON become 442
bytes of events~~ — that pair compared oidio's RAW body against the stream, and half
of it stopped being true when the canonical shipped. **Re-measured against what
ships, same request both ways: 665 bytes of canonical `diarized_json` against 407
bytes of events.** The loss is the same loss; only the numbers moved.
The manifest tells a caller to set `stream=false` to keep them.
This is the largest loss in the whole layer and the one a caller is most likely to
trip over, because they asked for a stream and got a smaller answer.

**Caller → backend:** `diarized_json` and `verbose_json` both have to be requested
upstream as whatever that backend accepts — NeMo takes only `verbose_json`, oidio
takes both. A caller asking for `json` from a NeMo-backed model makes corrallm ask
for MORE than the caller wanted and throw the excess away. That is bandwidth on a
local socket and it is not worth a second mechanism.

**`num_speakers` is not published.** Neither backend serialises it. It is
`len(distinct labels)` — arithmetic on returned data, which the caller can do as
easily as corrallm can, and publishing a derived count as though it were measured is
the kind of small lie this section exists to catch. ~~The manifest already says "take
`speakers.length`"~~ — shipped: the manifest now says "count the distinct
`segments[].speaker`".

### 12.9 Open sub-choices — ✅ ALL EIGHT CLOSED 2026-09-23

Nothing here is open. (a) was the gate and the owner ratified it; (c), (d), (f) and
(g) were settled by §9.0b; (b) and (h) by §9.0c; (e) is ruled on above and its rule is
written. Kept rather than deleted because each records WHY, and the code points here.

**(a) Is OpenAI's `diarized_json` the canonical shape?** ✅ **YES** — *"Yes, project
mission statement is openai compatible."* (§9.0c). It removes §9.3 entirely rather
than answering it. The cost of yes, paid: corrallm carries a third `response_format`
and relabels oidio's UUIDs on every diarized request.

**(b) `"A"`/`"B"` or `speaker_0`/`speaker_1`?** ✅ **Letters** — OpenAI's, per §9.0c,
so the label FORM was never a choice. What was: what happens at 27. **Answered as
RULE 1 in §12.4** — bijective base-26, `"AA"` after `"Z"`.

**(c) `segments[].id` — string or integer?** OpenAI is inconsistent with itself: the
`transcript.text.segment` EVENT carries a string `id`, `whisper-1`'s `verbose_json`
carries an integer. oidio carries an integer. They cannot both be canonical, and the
streaming and non-streaming forms must agree with each other. ✅ **STRING**, settled
by §9.0b: accept either from a backend, emit one, and the event schema wins because it
is the one actually specifying diarized output. Built: an integer `0` from oidio comes
out as `"0"` in both forms, verified live.

**(d) The `backend` sidecar: does it ship, and if so always or on request?** ✅
**`include[]=backend`, opt-in**, settled by §9.0b: the default response is the
documented canonical shape and nothing else. It does add a request-side parameter
corrallm owns and no backend knows about — which is why the parameter is STRIPPED
before the request goes upstream, and why an `include[]` value corrallm cannot serve
is a 400 at the door rather than a silent drop.

**(e) NeMo's textless segments: join the words, or `text: ""`?** ✅ **ANSWERED —
JOIN, and here is the rule.** The proposal was right and underspecified: it left
three cases without an answer, and two implementations would have disagreed at
exactly those. **RULE 2, as built and as published:**

1. A word belongs to the **FIRST** segment, in `segments[]` **document order**,
   whose **half-open** `[start, end)` contains the word's `start`. Half-open, so a
   word starting exactly on a boundary belongs to the segment that is STARTING, not
   the one that is ending. First-in-document-order, so overlapping segments resolve
   deterministically rather than by whichever the loop reached last.
2. A segment with **no `end`** runs to the **next segment's `start`**, or to
   infinity when it is the last. A backend that reports where a turn begins and not
   where it ends has still said where the next one starts.
3. A word matching **no** segment is appended to the nearest segment **starting at
   or before** it. A word starting **before every** segment goes to the **first** —
   "nearest preceding" has no referent there, and dropping the word would lose
   transcript text to a gap in the backend's own segmentation.
4. A word with **no `start`** cannot be placed and is not placed. It still appears
   in `words[]`; it is absent from every segment's text.
5. A segment's text is its words in the order they were **RETURNED**, not re-sorted.
   A single space joins two only when the left does not already end with whitespace
   and the right does not begin with it — oidio writes `"The"` and the same
   recogniser's stream writes `" The"`, and a caller must not be able to tell which
   by counting spaces. The result is trimmed.
6. A segment that gathers **no** words keeps `text: ""`. That is the honest
   rendering of "no word landed here", and it is the only case where a canonical
   segment's text is empty.

**RULE 2b — the case the question did not cover.** A backend can answer "who spoke
when" entirely in `words[]`, with no `segments[]` at all; NeMo's verbose_json does.
The canonical requires `segments[]`, so they are DERIVED: each **maximal run of
consecutive words sharing a speaker** becomes one segment — `start` from the run's
first word, `end` from its last word **when the backend reported one and OMITTED
otherwise** (never the next run's start, which nobody measured), `text` by rule 5,
`id` the run's zero-based index as a string. Consecutive in RETURNED order, not in
time, for rule 5's reason.

**Nothing here is invented.** Every character emitted was returned by the backend;
this is arithmetic on returned data, which §9.0 permits, and it is the whole of what
§9.0 permits. ~~**Untested against the backend that motivates it**~~ — **RULE 2 IS
NOW EXERCISED ON REAL DATA, and it took a second upstream call to get there.** See
§12.12; the join's exposure is unchanged in kind (a wrong boundary moves a word and
nothing in the response says so) and is now measurable rather than hypothetical.

**(f) `words[].end` when the backend does not report it.** ✅ **OMIT** (§9.0b(f),
§12.6). Verified live: a canonical `words[]` from oidio carries no `end` KEY at all,
not a null.

**(g) `diarized_json` to a non-diarizing model: 400, or degrade?** ✅ **400** (§9.0b).
Built — with one compromise that is named rather than hidden, because it was not
visible until the code was written: **the refusal happens at RESPONSE time, not at the
door.** Nothing in corrallm's model record declares that a model diarizes. `capability`
is derived from the cost `type` string (`config.TypeIO`), and a substring check for
"diarize" would 400 a NeMo-backed model that diarizes under a different type name —
worse than a late refusal. So the only honest signal available is what the backend
actually returned. The consequence, measured live: the caller gets 400 while the
activity row records the backend's 200 and its 213-byte body, because the work was
really done and really cost GPU time. Moving this to the door needs a model to be able
to DECLARE diarization — a `config` change, tracked in `open-questions.md` §5.

**(h) Does corrallm accept OpenAI's `known_speaker_names[]`?** ✅ **The NAME is
reserved; a request that USES it is refused 400 at the door.** §9.0c settled the
spelling — OpenAI defines it, so corrallm must not spell it differently, and the
parameter is recognised by name rather than falling through as an unknown form field.
The mechanism is not implemented, and that is the half this had to decide: OpenAI
pairs names with reference audio, oidio takes `known_speakers` UUIDs a caller kept
from a previous call, and mapping one onto the other is speaker recognition (§9.4),
not a rename.

**Why 400 rather than accept-and-ignore.** Accepting names and answering `"A"` is
accept-generously misapplied to a capability — the exact boundary §9.0b drew, and
the same failure as §12.7's silent degrade. Refusing at the door costs the caller one
error message and no GPU time; accepting costs them a transcript they think is
labelled with names. **An EMPTY `known_speaker_names[]` is a no-op, not a refusal** —
reserving a field must not break a client that sends it empty by construction. Both
halves verified live: 400 in 0.6 ms with no backend call, and an empty value 200.

oidio's own `known_speakers` is untouched and still forwarded, under its own name.

### 12.10 Findings from building §9.1 — two filed, and both now closed by §9.0b

1. **`oidio-stt` — the NON-diarize path — DOES stream for real.** §8.2 says
   "`stream=true` is silently ignored on oidio's diarize path", which is precise and
   is easy to read as broader than it is. Verified 2026-09-23: `oidio-stt` with
   `stream=true` returns `text/event-stream` and ten real `transcript.text.delta`
   events. corrallm forwards them byte for byte and unbuffered, which is the shipped
   behaviour — and, since 12.10.2 below, terminates them.
2. **oidio's real stream did NOT send `data: [DONE]`. ✅ FIXED 2026-09-23** —
   reopened by §9.0b, which made it a broken contract rather than a filed quirk.
   Shipped: `internal/proxy/sttstream.go`, the `passing` mode.

   **Measured before anything was written**, against the live `oidio-stt` with
   `examples/audio/speech.wav`. The real stream is:

   ```
   10 × data: {"delta":" The"|" quick"|…|".","type":"transcript.text.delta"}
    1 × data: {"text":"The quick brown fox jumps over the lazy dog.",
               "type":"transcript.text.done"}
   (nothing else)
   ```

   **TWO divergences from the synthesised shape, not one.** The missing
   terminator, and a `transcript.text.done` with **no usage block**. The event
   TYPES and FIELD names already matched what `sttstream.go` synthesises —
   `type`+`delta`, `type`+`text` — so nothing had to be renamed or reordered.

   **What was normalised, and what deliberately was not.** The terminator is
   appended. The usage block is NOT: a real stream reports no duration, and
   computing one from the upload emits a number nobody measured — the ruling
   §9.0b already made for `words[].end` (f). `usage` is therefore an explicitly
   OPTIONAL field of the published contract, present only when the backend
   measured the audio, and the manifest now says so rather than promising it.

   **Completed, never rewritten.** A passed-through stream's bytes go downstream
   as they arrive, unbuffered and unaltered; corrallm observes them on the way
   past and appends at the end only what the backend left out. Accept generously
   is what keeps that from doubling anything: a backend that already sent
   `[DONE]`, or its own `transcript.text.done`, gets no second one. A stream that
   sends deltas and no done gets one built from those deltas — concatenation of
   returned data, not a guess — and **that case is unit-covered only; no backend
   on this box behaves that way.**

   **§9.1's three properties survive.** Headers are still held for a JSON body,
   the mode is still decided from status + `Content-Type`, and the writer still
   sits OUTSIDE `statusCapture`. Verified live: the activity row records oidio's
   648 bytes while the client receives 662, so the 14-byte terminator is
   corrallm's and is not metered.

   **Verified on a second daemon on `:6510`** proxying the already-running oidio
   on `:5806` — no `cmd`, so nothing was spawned, loaded or evicted, and
   production was not touched. `oidio-stt` ends `data: [DONE]` with its ten
   deltas and one done intact; `oidio-stt-diarize` still synthesises the same 442
   bytes §9.1 recorded. Both halves of the route now terminate identically.

3. **The capabilities manifest's `streaming` note is now NORMATIVE** (§9.0b's
   standing obligation), and was rewritten as a specification: the full event
   sequence, `[DONE]` named as always-last and usable as the end-of-stream test,
   `usage` named as optional, `logprobs`/`languages` named as never emitted, and
   the two producers behind the one contract described as differing in timing
   only. The old text also carried a THIRD false claim of the same family as
   `speech.wav` and `num_speakers` — "no STT backend on this box streams", which
   §12.10.1 had already disproved. It is gone.

**✅ FIXED 2026-09-23, after the merge** — `modelFromMultipart` read the `stream`
form field as `strings.TrimSpace(v) == "true"`, so a client sending `True`, `TRUE`
or `1` silently got a non-streaming reply: the flag was read, found wanting and
dropped without a word. It now uses `strconv.ParseBool`, which is the documented
Go spelling of the same tolerance a JSON body already got for free — the
asymmetry was never intended, it was the difference between a real boolean and a
text field nobody re-read.

Filed as out of scope by the agent that found it, and that was the right call at
the time. It is in scope now because §9.0b is exactly this: accept generously on
the way IN. An unparseable value (`yes`, `on`) still resolves false, as before —
generosity is not guessing.

**It does reach `/v1/audio/speech`, and that is not a §9.1 reversal.** On the
speech route `streaming` only labels the activity row; the SSE gate is
`audioUploadRoute`, which excludes speech structurally. So a `stream=True` speech
request is metered correctly now and still returns binary audio. What §9.1 left
open — whether TTS should stream audio chunks at all — is untouched.

### 12.11 As BUILT — what shipped, what was measured, what is unit-covered only

`internal/proxy/sttremap.go`, wired in `proxy.go`. Covers
`/v1/audio/transcriptions` and `/v1/audio/translations`; `/v1/audio/speech` is
excluded structurally by `audioUploadRoute`, unchanged.

**Where it sits, and why that order.** The response path is
`w ← sttStreamWriter ← sttRemapWriter ← statusCapture ← ReverseProxy`. INSIDE the
SSE writer, so events are synthesised from corrallm's canonical body and carry
canonical speaker labels — the streaming and non-streaming forms cannot drift
because one is built from the other. OUTSIDE `statusCapture`, so the activity row
still records the BACKEND's body and metering is untouched, exactly as §9.1
arranged. Both writers hold headers until the body is complete, so a body that
does not parse leaves with its original headers and status.

**Request side, before admission** (beside the transcode, for the same three
reasons): read `response_format` / `include[]` / `known_speaker_names[]` once,
refuse what cannot be served, rewrite the upstream `response_format` to
`verbose_json`, strip `include[]`.

#### Measured live, 2026-09-23

A second daemon on `:6510` with its own home and DB, oidio configured as a pure
proxy onto the already-running `:5806` with no `cmd` — nothing spawned, loaded or
evicted. Production `:8111` was asked for transcriptions (which is what keeps
oidio resident) and was never restarted, stopped or reconfigured.
`examples/audio/speech.wav`, 2.56 s, one speaker.

| request | before the remap | after |
|---|---|---|
| `oidio-stt-diarize` `json` | 5,640 B, segments+speakers+voiceprints | **55 B** — `{"text":…}` and nothing else |
| `oidio-stt-diarize` `verbose_json` | 6,348 B | **665 B** — labels `"A"`, ids `"0"`, words with no `end` |
| `oidio-stt-diarize` `diarized_json` | (did not exist; oidio ignored it) | **665 B**, `speaker` required |
| … `+ include[]=backend` | — | **6,097 B** — 512-float voiceprint + `label:"A"` |
| `oidio-stt` `json` / `verbose_json` | 56 B / 213 B | **55 B / 261 B** (`usage` added) |
| `oidio-stt` `diarized_json` | 200, silently plain | **400**, naming the model |
| `text` / `srt` | forwarded | **forwarded, byte for byte** (44 B / 89 B) |
| `diarized_json` + `stream=true` | 442 B of events, speaker = a UUID | **407 B, speaker `"A"`** |
| `json` + `stream=true`, `oidio-stt` | 662 B passed through + `[DONE]` | **662 B, unchanged** |
| `known_speaker_names[]=Alice` | forwarded and ignored | **400 in 0.6 ms, no backend call** |
| `include[]=logprobs` | forwarded and ignored | **400 in 0.6 ms, no backend call** |

Also verified: `/v1/audio/translations` returns the same canonical body (665 B,
`diarized_json`) — and still reports `"task":"transcribe"`, which is contract 3,
still open and untouched. oidio's own request args (`num_clusters`,
`speaker_min_duration`, `known_speakers`) still reach it and still 200.

**Metering.** `audio_bytes` bills the caller's upload, not corrallm's rewrite —
the upstream form gains a `response_format` field the caller did not send, and
charging for it would bill a caller for corrallm's compatibility layer. The
activity row's captured response is still oidio's (4,120 B of a 6,348 B body at the
capture cap) while the client receives 665, so the meter reads the backend.

#### Unit-covered only — say so rather than implying otherwise

- **Both join rules (§12.9(e), rules 2 and 2b).** NeMo-Speech.cpp's PR is unmerged
  and nothing on this box runs it. Every NeMo column in §12.3 is source-read, and
  every line of code written for it is exercised by tests and by no backend.
- **The 27-speaker rollover (§12.4, rule 1).** No 27-speaker recording exists here.
  Unit-tested to 1,000 labels for distinctness.
- **An integer speaker key** (NeMo's shape) reaching the canonical as a letter.
- **A backend that reports `words[].end`.** oidio does not; NeMo is said to.
- **A backend returning `segments[]` with extra keys** (oidio's documented
  `overlap` never appeared in any sample), so `backend.segments[]` has never been
  populated on the wire.

#### Known gaps, all named rather than fixed

1. **The `diarized_json` refusal is late** — §12.9(g). The caller gets 400; the
   activity row records the backend's 200. It needs a model to be able to declare
   that it diarizes, which is a `config` change.
2. **NeMo's `json` still asks upstream for `verbose_json`** and projects down, which
   is what §12.8 ruled — but that ruling is now load-bearing for a backend nobody
   has run. If a third backend 400s on `verbose_json`, this is where it breaks.
3. **`text`/`srt`/`vtt` are not normalised.** oidio writes `Speaker 1:` into an SRT
   cue — its spelling, not corrallm's ordinal label. Documented in the manifest as a
   backend's text rather than quietly rewritten, because rewriting inside a subtitle
   cue is editing a transcript.
4. **`include[]=logprobs` is now a 400 where it was silently ignored.** That is the
   intended direction (§9.0b) and it is a behaviour change for any caller that was
   sending it.
5. **A diarizing client reading oidio's extras out of a plain `json` body will
   break.** That is the whole point of §12.1's floor, and it is the largest
   compatibility change in this commit. The manifest says so in its first sentence.

---

### 12.12 The two-call diarized join — ✅ BUILT 2026-09-25, and it is what makes RULE 2 fire

> The owner, 2026-09-25: *"As for the incompatibility, do the two call variant.
> Remember, we are supposed to be an adapter on these wild West audio systems into a
> reliable OpenAI API."*

**It settles a tension §9.0 left open, and the answer is that there was no tension.**
"May compute what a backend did not return, may not invent what no backend knows"
reads, at first glance, as forbidding a second upstream call. It does not. Joining a
backend's own words to that same backend's own speaker segments is arithmetic on
returned data whether they arrive in one response or two. Which endpoint the data
came through is not a moral fact about it.

#### What was broken, and it is measurable on PUBLIC audio

NeMo-Speech.cpp answers "who spoke when" twice and the two answers disagree.

| endpoint | what it returns | verdict |
|---|---|---|
| `/v1/audio/diarizations` | `{"segments":[{start,end,speaker:int}]}`, textless, overlapping | **correct at every length tried** |
| `/v1/audio/transcriptions?diarization=true` | the same fused onto `words[]` | **wrong on long audio** |

The failure was first seen on a private 1451 s call (first 634 s / 1,387 words on one
speaker). **It reproduces on CC-BY audio, which is what makes it reportable**: NeMo's
own `test_files/diar/ami_en2002d_2132.wav` — 60 s of AMI meeting EN2002d, three
speakers, real overlap — concatenated to 1440 s.

```
fused, per 120 s block, 1440 s:        the diarizations endpoint, same file:
   0- 120s {3: 277}                       0- 120s {1: 11, 2: 18, 3:  8}
 120- 240s {3: 275}                     120- 240s {1: 11, 2: 18, 3: 10}
 240- 360s {3: 281}                     240- 360s {1: 10, 2: 19, 3: 10}
 360- 480s {3: 278}                     360- 480s {1: 13, 2: 19, 3: 11}
 480- 600s {3: 261, 2: 8}               480- 600s {1: 12, 2: 19, 3: 12}
 600- 720s {1: 134, 2: 103, 3: 35}      600- 720s {1: 13, 2: 18, 3: 10}   …uniform to 1440
```

**The first 597.6 s — 1,372 words — on one speaker**, then plausible behaviour, against
a recording that is literally the same minute twelve times over. The two endpoints
agree on **97.1% of words at 720 s and 62.1% at 1440 s**, so this is a LENGTH failure
in the fusion, not a bad diarizer. Remapping the fused body faithfully publishes that
in OpenAI's shape, where nothing downstream can see it — worse than an ugly shape.

#### The rule that fires, and what overlap does

**RULE 2, unchanged.** It was written for exactly this shape (§12.3 read it off the
source) and then recorded as unexercised because the FUSED endpoint returns no
`segments[]`. The second call supplies them; RULE 2b stays for backends that answer
entirely in speaker-bearing words.

**Overlap is a TIE-BREAK, and clause 1 is it: the first candidate in the backend's own
document order.** Simultaneous speech is real and this diarizer reports it — 80 words
of 3,284 (2.4%) at 1440 s start inside two segments at once. For those the backend has
said two people were talking and has not said which produced the word, so every answer
is arbitrary; the requirements are that it be deterministic and written down. For a
diarizer emitting segments in start order (NeMo's are, verified) that means **the
speaker whose turn began earlier keeps the word**.

**Rejected: prefer a candidate that also contains the word's `end`.** Defensible, uses
more returned data, and measured on the same audio it moves **3 words of 3,284**
(0.09%). A second rule every reimplementation must get right, for that, is not a
trade. Recorded because the alternative was considered and priced, not overlooked.

**The segments are the AUTHORITY.** Every word's speaker is re-derived from the segment
that owns it and the fused `words[].speaker` is discarded — publishing both would be
two answers to one question. A word RULE 2 declines to place (clause 4) carries no
speaker rather than a guessed one.

#### Opt-in per backend, declared, not global

`ModalitySpec.DiarizationPath` — `modalities.audio.diarizationPath`, a PATH not a bool
because corrallm has to call it and no two backends spell it alike. Modelled on how
`Formats` and the PCM bounds already carry per-backend audio facts, so it inherits
`EffectiveModalities`'s probed-beats-declared precedence and appears on `/v1/models`
without new plumbing. Refused at LOAD if it does not begin with `/`.

**Empty everywhere by default, and that is the whole off switch.** oidio's fused output
is internally consistent, one call is cheaper than two, and nothing about oidio's path
changes. No model name is hardcoded anywhere.

#### What it costs, measured

NeMo at `97a15af` on the 3080, AMI loops, wall clock:

| audio | ASR only | fused (1 call) | diarizations | two calls | delta |
|---|---|---|---|---|---|
| 720 s | 7.49 s | 16.77 s | 11.51 s | 19.00 s | **+13.3%** |
| 1440 s | 15.31 s | 36.49 s | 25.85 s | 41.16 s | **+12.8%** |
| 1440 s, end to end through corrallm | — | 36.62 s | — | 40.99 s | **+11.9%** |

**+13% and not +100%, because the transcription leg stops asking for diarization at
all.** corrallm strips the caller's `diarization` flag on that leg, so it pays for one
ASR pass and one diarizer pass — the same two the fused call already ran. Kept naive,
the identical work costs **+69%**. The words are unaffected: the transcription bodies
with and without the flag were compared word for word and timestamp for timestamp on
both files and are **identical**.

**A second pass is spent only when the answer can carry a speaker**: `diarized_json`,
or a backend's own diarization flag on `verbose_json`, or either under `stream=true`.
Plain `json` returns `{"text":…}` and makes one call.

#### Two allocations are never live at once

The second call is issued from the RESPONSE path, after the transcription body is read
to completion and BEFORE the residency ref is dropped. Strictly sequential, never
concurrent; the admission slot is still held, so a `maxConcurrent: 1` backend does not
see a neighbour land in the gap. Measured across both 1440 s requests: the 3080 rose to
a **4,772 MiB** plateau and the diarization call added nothing on top — the compute
buffer is retained and reused, which is what §14 of `p35-nemo-live.md` already
established and what makes a concurrent second call the dangerous option on a backend
that ABORTS rather than 500s when it cannot allocate.

#### A failure is a refusal, not a fallback

A diarization call that fails, 404s, or returns nothing readable → **502 naming the
endpoint**. Falling back to the fused labels would serve the known-bad answer under the
good one's name, which is §9.0b's "forwarding a bug with a new hostname on it" with the
bug now documented. Verified live against a model declaring a path that is not there.

#### The compatibility win nobody asked for

`diarized_json` against NeMo used to require the caller to also know NeMo's own
`diarization=true`, or get §12.7's 400. With a declared endpoint corrallm fetches the
speakers itself, so **`response_format=diarized_json` alone now works** — a plain
OpenAI client needs no backend-specific parameter. Verified live.

#### Turn structure, fused vs joined, same 1440 s file through corrallm

| | fused | joined |
|---|---|---|
| segments | 133 | 491 |
| longest segment | **597.4 s, 1,372 words** | 7.7 s, 33 words |
| median segment | 2.24 s | 1.67 s |
| segments in 240–480 s | **0** | 82 |
| empty-text segments | 0 | 49 |
| top-level and item keys | identical | identical |

The 49 empty segments are RULE 2 clause 6 working: a diarizer heard a backchannel the
recogniser did not transcribe, and `text: ""` is the honest rendering of that.

#### What is untested

- **No private audio was used, and none is referenced.** The corpus is NeMo's own
  CC-BY AMI excerpt, looped. A LOOP is not a 24-minute conversation: it repeats one
  minute, so it cannot show drift, topic change, or a speaker who leaves.
- **One backend.** Only NeMo-Speech.cpp has a separate diarization endpoint on this
  box. The `wireSegment` reader accepts a string speaker and a segment `id`, but
  nothing has sent either.
- **Nemotron 3 was not re-run.** The Sortformer v2 diarizer served every measurement
  here. §14's V3 abort-on-OOM is untouched by this work and is not made better by it.
- **No DER.** The AMI RTTM ground truth was not used to score either answer; the
  comparison is structural and an agreement percentage, not an accuracy claim.
- **The 502 refusal is not reachable from `/v1/models`.** A caller cannot tell in
  advance that a model's diarization endpoint is misconfigured — same shape as §12.9(g)'s
  late refusal, same fix (a model that can DECLARE what it does), same open question.
- **`stripDiarizationFlag` assumes the flag's only effect is the diarizer pass.** True
  for NeMo at `97a15af` (verified: identical words). A backend where the flag also
  changes segmentation or punctuation would be silently altered by the strip.

---

## 13. "Ensure that is the case" — the compatibility audit. ◻ UNSTARTED

§9.0c referenced this section and it did not exist. It does now, empty of findings
and honest about that, because a dangling forward reference is how a plan starts
describing work nobody did.

**The instruction.** *"Yes, project mission statement is openai compatible. Ensure
that is the case."* The second sentence is the larger half and it is NOT scoped to
diarization or to audio. Every surface corrallm serves is a compatibility claim, the
manifest is normative under §9.0b, and an unaudited claim is a liability rather than
a gap.

**Why it is a real tree and not a chore.** This manifest has shipped four false
claims that anyone would have caught by trying them: a `speech.wav` that existed
nowhere, a `num_speakers` nothing returns, "no STT backend on this box streams" —
which §12.10.1 had already disproved — and, until 2026-09-23, a diarization note
promising the caller oidio's raw body. Four is a pattern. The audit's product is not
prose; it is **a check per claim, run against the wire**, and the claims that cannot
be checked get deleted rather than softened.

**What is already known to be false or unstated, from the work so far:**

- `/v1/audio/translations` **does not translate** (`open-questions.md` §5 item 2).
  The shape claim is true and the behaviour claim is false. Re-verified live
  2026-09-23 through the canonical remap: `"task":"transcribe"`.
- `/v1/images/generations` **never honours `response_format`** — OpenAI defaults to
  `url` and corrallm only ever emits `b64_json` (item 5). Nothing says so.
- `/v1/realtime` **advertises a hardcoded "PCM16 mono @ 24 kHz"** that is oidio's
  default, per-model configurable, and decoded from Opus at 48 kHz anyway (item 6).

**next** — nothing scheduled; this needs a decision about scope before it needs code.
**risks** — an audit that produces a document instead of tests is the fifth false
claim waiting to happen. **blocking decisions** — none to start, but items 2, 5 and 6
are each a policy call the owner owns, and the audit will surface more of the same
kind rather than resolving them.
