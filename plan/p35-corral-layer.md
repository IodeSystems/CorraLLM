# P35 — the corral layer: transcode in, remap out

> Opened 2026-09-23, from the owner: *"we also need a transcode layer into it, so
> that we can maintain compatibility, and we need a remapping layer out for the same
> reason."*
>
> The framing, his words, earlier: *"speech, like image generation, has odd,
> non-standard backends, and we intend to 'corral' them into a standard OpenAI
> interface."*
>
> **Phase 1 (transcode in) is BUILT and verified live — see §7.** Everything about the
> remap out is design only; each of its five contracts carries a policy call that is
> the owner's, named in §9.

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
2. **`stream=true` is silently ignored on oidio's diarize path.**
   `transcriptions.go:45` dispatches to `handleDiarize` before the stream check, and
   `handleDiarize` never reads it. **Re-verified on the wire 2026-09-23:** `stream=true`
   to `oidio-stt-diarize` returned `200` with `Content-Type: application/json` and a
   complete non-SSE body. Three options and the current one is the worst.
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

**Phase 1 (built) took three, all reversible and all argued in the commits:** convert
only on a declared mismatch · convert before admission · refuse rather than pass
through. **These are the open ones, and phase 2 must not start before they are answered:**

1. **`stream=true` on a path that cannot stream.** Synthesise SSE from the complete body
   (the client's contract is honoured, the latency benefit is a fiction), refuse 400
   (honest, breaks a caller who set the flag defensively), or keep ignoring it
   (today's behaviour, and the only one that is silently wrong). **Per endpoint, not
   globally** — the same question has a different answer on `/v1/audio/speech`.
2. **`/v1/audio/translations`.** Route it to a translating backend, refuse it as
   unimplemented, or keep transcribing and document the lie. Corrallm could also
   translate the transcript with a chat model — a real option, and one that changes what
   the endpoint costs.
3. **Diarization: whose shape is canonical?** OpenAI has no diarization schema, so
   "the OpenAI shape" does not settle it. If oidio's is canonical, corrallm must
   compute speaker turns for NeMo. If NeMo's is, oidio's per-request UUIDs must be
   renumbered and its embeddings dropped. Both are lossy, in opposite directions.
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

**Phase 4 — remap out, the synthesis.** Contracts 1, 2 and 3. **Blocked on §9.1–§9.4.**
Do not start it before those are answered; this is the part where a compatibility layer
starts lying if it guesses.
