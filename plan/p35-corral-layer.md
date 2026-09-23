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
> **contract 2 (`stream=true` on STT) is BUILT and verified live — see §9.1.** The
> rest is design only; each remaining contract carries a policy call that is the
> owner's, named in §9. **§12 is the canonical schema, written and awaiting
> ratification — it is the gate on phases 3 and 4.**

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

## 8. Coming out: five contracts, each verified, none implemented

**Deliberately not built.** Every one of these needs corrallm to *decide* something, and
two of them need it to DO something the backend does not — which is not renaming, and
not a call to make inside an implementation commit.

1. **Diarization shape differs per backend.** oidio returns
   `segments[]{id,start,end,text,speaker:UUID,overlap?}` plus top-level
   `speakers[]{uuid,embedding[512],similarity{uuid→cosine},clean_seconds,total_seconds,blended?}`
   and `words[]{word,start,speaker}` with **no `end`** — and **the UUID is minted per
   request**: three calls on the same audio gave three different ones. NeMo-Speech.cpp
   instead puts an integer `speaker` on `words[]` (verbose_json only; 400 otherwise),
   or returns textless `{"segments":[{start,end,speaker:int}]}`, and never serialises
   `num_speakers` at all. **Normalising NeMo onto oidio's shape means joining words
   into speaker turns — computing something the backend never returned.**
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
- **§12.10.2 — `data: [DONE]` — REOPENED, now IN SCOPE.** Filed as "outside §9.1", which
  was right at the time. Under produce-strictly it is not: corrallm's output terminates
  the same way whichever backend answered, so oidio's real stream gets normalised too. A
  client must be able to use `[DONE]` as its end-of-stream test on this route. This is
  the principle's first real cost — it means touching a backend's own stream, which §9.1
  deliberately did not.

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
sloppiness — the manifest is now what a client is entitled to rely on.

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

**A real stream is untouched**, keyed on the backend's own `Content-Type`. The live
box has one — `oidio-stt` (§12.10) — and its events pass through byte for byte,
including its own missing `[DONE]`. Synthesis is the fallback for a finished body,
never a rewrite of every reply. An error stays a JSON error with its own status.

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
   unimplemented, or keep transcribing and document the lie. Corrallm could also
   translate the transcript with a chat model — a real option, and one that changes what
   the endpoint costs.
3. **Diarization: whose shape is canonical?** ~~OpenAI has no diarization schema~~ —
   **that premise expired; see §12.0.** OpenAI now publishes `diarized_json` and a
   `transcript.text.segment` event, which makes "the OpenAI shape" an available answer
   and the third column §9.0 asked for. Folded into 3a rather than answered on its own.
3a. **The canonical schema itself — the gating question. DESIGNED, awaiting
   ratification: §12.** It fixes the speaker label's type and scope (string, `"A"`/`"B"`
   by order of first appearance, stable within one response and explicitly NOT across
   calls — §12.4); where a backend's extras go (a `backend` sidecar, measured at ~5.2 KB
   per speaker — §12.5); `words[]` (in the contract, with `end` optional because oidio
   does not report it — §12.6); and what a NON-diarizing model returns (absent fields,
   never null, never a placeholder; `diarized_json` to one is a 400 — §12.7). **Eight
   sub-choices are left for the owner in §12.9**, and (a) is the one phases 3 and 4 wait
   on.

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
synthesis, no decisions beyond §9.5 and §9.6. The smallest shippable slice of the other
half.

**Phase 4 — remap out, the synthesis.** Contracts 1 and 3; **contract 2 is done** (§9.1).
**Blocked on §12.9(a)** — the canonical schema is written but not ratified — and on
§9.2/§9.4. Do not start it before those are answered; this is the part where a
compatibility layer starts lying if it guesses.

---

## 12. The canonical audio schema (§9.3a) — DESIGN ONLY, the owner ratifies it

**Nothing here is implemented, and nothing here should be implemented before it is
ratified.** §9.0 mandates a canonical shape; this is the proposal for what it is,
written so it can be argued with rather than discovered in a diff. Phases 3 and 4
have no target until §12.9 is ruled on.

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
| `language` | string | always |
| `duration` | number (s) | always |
| `text` | string | always |
| `segments[]` | `{id, start, end, text}` + `speaker?` | when the backend segments |
| `words[]` | `{word, start}` + `end?` + `speaker?` | when the backend reports words |
| `usage` | `{type:"duration", seconds}` | always |

**`response_format: diarized_json`** — OpenAI's name, new to corrallm:

| field | type | when |
|---|---|---|
| `task`, `language`, `duration`, `text`, `usage` | as above | always |
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

**What it costs, measured:** the one-speaker sample is **5,640 bytes with the
voiceprint and 442 bytes without it** — ~5.2 KB per speaker, and it scales with
speaker count, not with audio length. That is why `json` never carries it.

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
synthesised stream drops all of them. **Measured: 5,640 bytes of JSON become 442
bytes of events.** The manifest tells a caller to set `stream=false` to keep them.
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
the kind of small lie this section exists to catch. The manifest already says "take
`speakers.length`"; under the canonical it becomes "count the distinct
`segments[].speaker`".

### 12.9 Open sub-choices — the owner rules on these

Phases 3 and 4 stay blocked until (a) is answered. The rest can be answered in any
order, but all of them before phase 4 writes a line.

**(a) Is OpenAI's `diarized_json` the canonical shape?** §12.0 says the premise that
it does not exist is out of date. Recommendation: **yes** — it removes §9.3 entirely
rather than answering it. The cost of yes: corrallm carries a third
`response_format` and relabels oidio's UUIDs on every diarized request.

**(b) `"A"`/`"B"` or `speaker_0`/`speaker_1`?** Letters are OpenAI's and run out at
26. Twenty-seven speakers in one recording is not a real case for this box, but
"what happens at 27" needs an answer in the schema rather than in a panic later.

**(c) `segments[].id` — string or integer?** OpenAI is inconsistent with itself: the
`transcript.text.segment` EVENT carries a string `id`, `whisper-1`'s `verbose_json`
carries an integer. oidio carries an integer. They cannot both be canonical, and the
streaming and non-streaming forms must agree with each other. The shipped §9.1 code
renders it as a **string** in the event, matching the event schema; if the answer is
integer, that is a one-line change and a test.

**(d) The `backend` sidecar: does it ship, and if so always or on request?** Three
options: always present in `verbose_json`/`diarized_json`; only when the caller
passes `include[]=backend` (OpenAI's own opt-in mechanism, used for `logprobs`); or
not at all. Recommendation: `include[]=backend`, because 5.2 KB per speaker on every
response is a real cost for data most callers will not read — but that adds a
request-side parameter corrallm owns and no backend knows about, which is a new kind
of thing in this layer.

**(e) NeMo's textless segments: join the words, or `text: ""`?** If join, the rule
must be written into the schema: *a word belongs to the segment whose
`[start, end)` contains the word's `start`; a word matching no segment is appended
to the nearest preceding one.* That rule is a guess at a boundary and should be
ratified as a rule, not discovered as behaviour.

**(f) `words[].end` when the backend does not report it.** Recommendation: omit
(§12.6). The alternative is to compute it from the next word's start and say so in
the manifest.

**(g) `diarized_json` to a non-diarizing model: 400, or degrade?** Recommendation:
400. It is the same class of decision as §9.1 and should not be answered differently
without a reason.

**(h) Does corrallm accept OpenAI's `known_speaker_names[]`?** OpenAI takes names
plus reference audio. oidio takes `known_speakers` — UUIDs, from a previous call the
caller kept. They are different mechanisms with the same purpose, and mapping one
onto the other is not a rename. Out of scope for the schema; in scope for whether
the schema should reserve the field name now.

### 12.10 Two findings from building §9.1, recorded here because they touch this

1. **`oidio-stt` — the NON-diarize path — DOES stream for real.** §8.2 says
   "`stream=true` is silently ignored on oidio's diarize path", which is precise and
   is easy to read as broader than it is. Verified 2026-09-23: `oidio-stt` with
   `stream=true` returns `text/event-stream` and ten real `transcript.text.delta`
   events. corrallm passes them through untouched, which is the shipped behaviour.
2. **oidio's real stream does NOT send `data: [DONE]`.** corrallm's synthesised
   stream does. So the two forms of the same route terminate differently, and a
   client reading corrallm cannot use `[DONE]` as its end-of-stream test unless
   corrallm normalises a real stream too — which is rewriting a backend's own
   stream, deliberately out of scope for §9.1. Filed rather than fixed. It is small
   and it is the kind of thing that costs someone an afternoon.
