# P35 phase 2 — the input capability told honestly

> How this plan works: see the "Planning" rules in `~/CLAUDE.md`. Current state and
> active work only; finished work moves out, deferred work moves to the icebox.
>
> This file is phase 2 of [`p35-corral-layer.md`](p35-corral-layer.md) §11, opened
> from the owner's instruction *"make sure to include a test for validating the nemo
> adapter intake"*. Phase 1 (§7) is unchanged by it.

## 0. The gap this closes, in one sentence

Phase 1 decided whether to convert from a list of CONTAINER names, so a 48 kHz
stereo 24-bit wav satisfied `formats: [wav]` and was handed to a backend that
refuses it — a late failure inside a process the caller has never heard of, which
is the thing this layer exists to prevent.

## 1. Status

**✅ BUILT and live-verified (§5).** Two commits: the conformance test, then the
implementation. Nothing in phase 3 or 4 is touched, and no schema question is
answered here.

## 2. NeMo-Speech.cpp's intake contract — read, cited, and NOT run

**No NeMo backend was ever contacted.** The PR is unmerged, nothing on this box
builds or runs it, and no request in any proof below reached one. What exists is
its SOURCE, read at one named commit, and the rules transcribed into Go with the
line each came from. The test that carries them says this about itself in its own
header, because a conformance test that reads like an integration test is a claim
nobody made.

    source: NVIDIA/NeMo-Speech.cpp @ 302ebc93f096d03395e5d86c643897b8bf0fd9a8
            (2026-09-21), read 2026-09-23

| rule | what it requires | where it is |
|---|---|---|
| R1 | the multipart field is literally named `file` | `server/http/http_server.cpp:195-198` |
| R2 | at least 12 bytes | `src/common/audio_file.cpp:36-37` |
| R3 | `RIFF`…`WAVE` — both words | `src/common/audio_file.cpp:38-39` |
| R4 | no chunk declares more than remains | `src/common/audio_file.cpp:53-54` |
| R5 | a `fmt ` chunk ≥ 16 bytes, and a `data` chunk | `src/common/audio_file.cpp:55-57, 75-76` |
| R6 | 1 or 2 channels | `src/common/audio_file.cpp:77-78` |
| R7 | 8 000 ≤ rate ≤ 96 000 | `src/common/audio_file.cpp:79-80`, again at `src/asr/recognizer.cpp:351` → `src/common/audio_resampler.cpp:15-16,63-65` |
| R8 | format 1 at 16-bit, or format 3 at 32-bit float. **Nothing else** | `src/common/audio_file.cpp:83-89` |
| R9 | `blockAlign == bytesPerSample*channels`, and `dataSize` divides by it | `src/common/audio_file.cpp:90-92` |
| R10 | at least one frame | `src/common/audio_file.cpp:110-111` |

**R8 is the gap.** 8-, 24- and 32-bit integer PCM are all refused, and so is
`WAVE_FORMAT_EXTENSIBLE` (0xFFFE) — which matters twice, because it is also what
ffmpeg emits (§4).

**These citations are the only thing standing between this file and silent
staleness.** Upstream can change the contract without telling us; a rule with a
line number can be re-checked in a minute, a rule without one has to be
rediscovered.

## 3. What the test asserts

`internal/proxy/audio_test.go`, three new blocks:

- **the checker is not vacuous** — thirteen files, each accepted or refused at the
  named rule. Without this, "corrallm's output is accepted" proves nothing.
- **transcode output satisfies the contract** — mp3/m4a/flac/webm/ogg through the
  real converter, then through all ten rules. Sniffing `"wav"` was the old test and
  it only ever checked R3.
- **the 24-bit case is settled at corrallm's door** — converted, or refused by
  corrallm with corrallm's status. Forwarding is the one outcome that fails.
- plus, per bound: silence adds no constraint, an in-bounds wav is untouched, each
  bound catches its own file, a declared target is converted INTO, the size cap
  follows the target, the three refusals name what is wrong, and probed still beats
  declared.

## 4. What the code now refuses, or converts, that it did not before

`ModalitySpec` grew `minSampleRate` / `maxSampleRate` / `maxChannels` / `pcm`. Each
is zero-means-silent, exactly as `formats` already is.

| case | before | now |
|---|---|---|
| 24-bit wav, backend declares its PCM bounds | forwarded → backend's 400 | converted → 200 |
| any wav outside the declared rate/channel/encoding bounds | forwarded | converted into the bounds |
| mp3 to a backend that reads only flac | converted to wav the backend cannot open → 400 | **415 at the door**, naming the format, the accepted list and the tool |
| a `pcm` list with no name corrallm can produce | (did not exist) | 415, naming both lists |
| a rate window with nothing in it | (did not exist) | 415 (probe-only: `Validate` refuses it in config) |
| a `pcm` typo in config | (did not exist) | **fails at load**, not at request time |
| backend declares containers only, or nothing | unchanged | **unchanged** |

**§9.0b, both halves, and both cost something.**

- **Accept generously.** The target is chosen to FIT the declaration rather than to
  be corrallm's favourite: a backend that asks for 44.1 kHz float32 gets 44.1 kHz
  float32. 16 kHz mono s16 is a default, not a demand. The duration cap follows the
  target — f32 is 4 bytes a sample, and a cap computed off s16 would let the 64 MiB
  body limit be crossed with nothing saying so.
- **Produce strictly, and this is where it bit.** ffmpeg's wav muxer writes
  `WAVE_FORMAT_EXTENSIBLE` for anything over 16 bits or over 48 kHz
  (`libavformat/riffenc.c`), and a reader that checks the format tag — R8 — refuses
  it. So honouring a request for float32 would have produced a file the requester
  cannot open: generosity that lands as somebody else's 400. `canonicalWAVHeader`
  rewrites the fmt chunk to the plain form its sub-format GUID already names. It is
  a no-op on the 16 kHz s16 default, which is every request today.

**No second way to answer the question.** The bounds ride the same
`EffectiveModalities` call the containers already did, so `ProbedModalities` still
overrides a declaration, unchanged and unduplicated. This is §2's rejected
"new `accepts:` block" staying rejected.

## 5. Live proof — an isolated daemon, and production never touched

Production `:8111` was not restarted, stopped, installed over or reconfigured; it
is ~18 commits behind and would never have run this code. A second daemon ran on
`127.0.0.1:6511` with its own `--home` and `--web-root`, against an intake stub on
`:6520` that transcribes nothing and reproduces `parse_wav` — all ten rules, not
phase 1's magic-number check. `:8111` answered 200 on `/v1/models` after the run.

```
                                                      HTTP   Transcode-Ms
nemo-sim            s24_48k_stereo.wav (24-bit)        200        110      {"text":"loaded 16000 Hz 1 ch s16"}
nemo-container-only s24_48k_stereo.wav                 400          0      "unsupported WAV encoding; convert with: ffmpeg …"
nemo-undeclared     s24_48k_stereo.wav                 400          0      same — silence still means "not my business"
nemo-sim            s16_16k_mono.wav (in bounds)       200          0      no conversion, 1.3 ms end to end
nemo-sim            speech.mp3                         200        100      phase 1, unchanged
nemo-sim            s16_192k.wav (rate out of bounds)  200        119      resampled to 16 kHz
nemo-float-only     s24_48k_stereo.wav                 200        114      {"text":"loaded 16000 Hz 1 ch f32"}
flac-only           speech.mp3                         415          –      "audio is mp3 and this model accepts only flac;
                                                                            corrallm converts audio with ffmpeg to wav,
                                                                            which this model does not accept"
```

The first two rows are the before/after on ONE file and ONE backend: the only
difference is whether the model declared its PCM bounds. The `nemo-float-only` row
is the EXTENSIBLE rewrite working on the wire — the stub read format tag 3.

Also verified live: the bounds survive SQLite (`config export` round-trips them),
`/v1/models` publishes them, and `corrallm validate` refuses
`pcm: [s16be]` with *"unknown audio pcm encoding … (want f32|f64|s16|s24|s32|u8)"*.

## 6. Active work

**◻ next** — nothing in this slice. It is complete and committed. The remaining
phase 2 items from `p35-corral-layer.md` §11 are below, untouched.

**risks**

1. **No NeMo backend was ever contacted.** The conformance spec is a reading of
   source at one commit. If upstream loosens or tightens `parse_wav`, this file and
   the test go stale silently; the citations are the mitigation, not a guarantee.
2. **R1 is not enforced.** `replaceFilePart` puts the audio back under the field
   name the client used, so a client uploading under something other than `file`
   is forwarded and refused by NeMo at `http_server.cpp:197`. Named, not fixed —
   renaming a caller's field is a decision, and the OpenAI clients all send `file`.
3. **PCM bounds are checked on wav only.** A compressed container states its
   parameters after a decode, which costs more than the check is worth, and every
   backend that has ever declared bounds accepts only wav. An accepted non-wav
   container is passed through exactly as phase 1 did.
4. **`canonicalWAVHeader` is corrallm's own reading of EXTENSIBLE.** It trusts the
   first two bytes of the sub-format GUID and drops the valid-bits count and
   channel mask. Correct for mono/stereo integer and float PCM, which is all
   corrallm produces; it declines to touch anything else.
5. **The s24/s32/u8/f64 encodings are producible but unproven against any real
   backend.** Nothing declares them. Only s16 and f32 have been exercised end to
   end, and only against a stub.
6. Phase 1's risks 2 and 3 (first-lane-member resolution, transcode spent before a
   capacity refusal) are untouched and still stand.

**blocking decisions** — none. Every choice here followed from §9.0b or from a
cited source line. Two are worth the owner's eye:
- the `pcm` vocabulary is **closed** and a typo fails at LOAD, unlike `formats`
  where an unknown name is dropped. Asymmetric on purpose: dropping the only entry
  of a `pcm` list empties it, and empty means "no limit" — a typo that widens what
  a picky backend is believed to accept.
- `audioFormatsFor` now returns an `audioIntake` and no longer only formats. The
  name was kept because `internal/proxy/proxy.go` calls it and that file belongs to
  another slice; renaming it is a one-line follow-up for whoever owns it.

**assumptions recorded**
- that a backend declaring bounds accepts wav (nothing else is producible);
- that ffmpeg's EXTENSIBLE threshold is bits > 16 or rate > 48 kHz — read off
  `riffenc.c` and confirmed on this box's ffmpeg, not pinned to a version;
- that mono always satisfies `maxChannels`, since 0 means unset and 1 is the floor.

**optional extensions, explicitly out of scope**
- R1: rename a non-`file` upload field.
- ffprobe the PCM parameters of an accepted COMPRESSED container (risk 3).
- surface the four new fields on the admin GraphQL `ModalityView`
  (`internal/api/handlers.go`) — `/v1/models` already publishes them; the admin
  view does not, and that file belongs to another slice.
- the two phase 2 items from §11 that this slice did not take: the strict-fallback
  case from §4, and a route probe that ASKS a backend what it ingests instead of
  reading a declaration.
