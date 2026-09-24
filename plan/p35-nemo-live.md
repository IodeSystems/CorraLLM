# P35 — NeMo-Speech.cpp, actually run

> How this plan works: see the "Planning" rules in `~/CLAUDE.md`. Current state and
> active work only; finished work moves out, deferred work moves to the icebox.
>
> This file records the first run of a NeMo MODEL on this box, and the first time
> `p35-corral-layer.md` §12.3's mapping table and §12.9(e)'s join rules were
> exercised against the backend they were written for. It corrects §12.3 and
> §12.8; it does not edit them — that file belongs to another slice.

## 0. What this closes, in one sentence

Every NeMo-shaped claim in P35 was source-read; phase 2 checked the intake spec
against NeMo's real WAV *parser* and said plainly that recognition was still
untested. **Recognition now ran, the remap ran behind it, and four rows of the
§12.3 table were wrong.**

## 1. Status

**✅ BUILT, ✅ RUN, ✅ DRIVEN END TO END.** Parakeet TDT 0.6B v3 + Sortformer
4-speaker v2 on the 3080, behind an isolated corrallm on `:6513`, over all seven
real hearings in their native containers. Every join-rule clause passes against
real input. Two canonical shape divergences between oidio and NeMo were found;
one is documented-and-intended, one is a **contract error** (§5.2).

Production `:8111` was never restarted, stopped, reconfigured or installed over,
and answered 200 on `/v1/models` before, during and after. No `vite build` ran.
The 5090 was never touched. Nothing was evicted.

## 2. The build — CUDA, and the two things that fought

Source: `NVIDIA/NeMo-Speech.cpp` @ `302ebc93`, **copied out of
`/tmp/nemo-scratch/` and built in the copy** — the upstream checkout has empty
`ggml`/`third_party` submodules and `scripts/configure.sh` patches `ggml` in
place, so "do not modify that checkout" and "build it" cannot both be done in
the same tree.

**The configure line that worked**, from `/tmp/claude-1000/p35nemo/src`:

```bash
export PATH=/tmp/claude-1000/p35nemo/bin:/usr/local/cuda-13.3/bin:$PATH
cmake --preset cuda-asr \
  -DNEMO_SPEECH_BUILD_HTTP=ON \
  -DNEMO_SPEECH_BUILD_TTS=OFF \
  -DNEMO_SPEECH_BUILD_NMT=OFF \
  -DNEMO_SPEECH_BUILD_MIC_CAPTURE=OFF \
  -DCMAKE_CUDA_ARCHITECTURES=86-real \
  -DCMAKE_CUDA_COMPILER=/usr/local/cuda-13.3/bin/nvcc \
  -DCMAKE_C_COMPILER=/usr/bin/gcc-13 \
  -DCMAKE_CXX_COMPILER=/usr/bin/g++-13
cmake --build build/cuda-asr -j 20
```

Prerequisites this box did not have, and how each was met without `sudo` (there
is no passwordless sudo here):

- **ninja** — not installed; the uv cache already had 1.13.0, copied to a local
  `bin/`.
- **SentencePiece** — no `libsentencepiece-dev`. Built static from the pinned
  commit `17d7580d` into `.deps/sentencepiece`, which is exactly what
  `scripts/build_sentencepiece_static.sh` does and where `src/asr/CMakeLists.txt`
  already looks.
- **submodules** — `ggml` and `third_party/cpp-httplib` only. `llama.cpp` is
  needed *only* for `llama.cpp/vendor/miniaudio/miniaudio.h`, and only when
  `NEMO_SPEECH_BUILD_MIC_CAPTURE=ON`; turning that off makes the whole submodule
  unnecessary. `configure.sh` demands it anyway, which is why the preset was
  invoked directly and `scripts/apply-ggml-patches.sh`'s work (all 21 patches,
  in filename order) was done with `git apply` instead.

**Two failures, both environmental, both worth writing down because the next
person hits them too:**

1. **`/usr/bin/c++` is clang 18 while `/usr/bin/cc` is gcc 13.** ggml's
   `cmake/common.cmake` picks its warning flags off `CMAKE_CXX_COMPILER_ID`
   alone and applies them to C too, so a clang-only `-Wunreachable-code-break`
   reached gcc and every C file failed. Pinning BOTH compilers to GNU 13 fixes
   it. Nothing about this is NeMo's fault and nothing in its docs predicts it.
2. **The driver is CUDA 13.2 and the only toolkit on the box is 13.3.**
   (`/usr/bin/nvcc` is 12.0 from the distro and is a decoy; the real toolkit is
   `/usr/local/cuda-13.3`.) A build with PTX therefore dies at the first kernel
   with *"the provided PTX was compiled with an unsupported toolchain"*.
   `-DCMAKE_CUDA_ARCHITECTURES=86-real` emits SASS only, nothing to JIT, and the
   problem disappears.

**And one that was nearly a rule violation.** After `86-real` the next failure
was *"no kernel image is available for execution on the device"* — because
**CUDA's default `FASTEST_FIRST` ordering makes device 0 the 5090**, not the
3080 `nvidia-smi` calls device 0. Every run here therefore pins
`CUDA_VISIBLE_DEVICES=GPU-76a4c775-…` (the 3080's UUID), which removes the
ordering question rather than answering it. **Anything on this box that says
`--gpu 0` and means "the 3080" is wrong.**

CPU was never needed.

## 3. What ran, where, and what it cost

| | |
|---|---|
| ASR | `nvidia/parakeet-tdt-0.6b-v3` q8_0, 713,975,456 B, sha256 verified against `models/index.json` |
| diarization | `nvidia/diar_streaming_sortformer_4spk-v2` q8_0, 147,075,776 B, sha256 verified |
| device | RTX 3080, pinned by UUID. 4,970 MiB free before, nothing evicted |
| weights resident | **1,141 MiB** (both models, `/ready` reports `device:"gpu:0"`) |
| peak, 725 s audio | 3,400 MiB above baseline |
| peak, 2,650 s audio | 3,772 MiB above baseline — 9,821 MiB of 10,240 on the card |
| 5090 | untouched, 1,458 MiB free throughout |

**Smoke test, `examples/audio/speech.wav` (2.56 s, 24 kHz mono s16):**
`"The quick brown fox jumps over the lazy dog."` — correct, 1.31 s wall
including model load.

**PR #50 is still unmerged, so diarization here is Sortformer v2 and nothing
else.** Nemotron 3 Diarization was not built, not downloaded and not run. Also
checked, because the shelf entry asks: `nemotron-3.5-asr-streaming-0.6b` loads
and transcribes correctly too (2,005 MiB peak), with a limited attention window
(`attention-left=56 attention-right=3`) rather than Parakeet's full context —
which is the bounded-memory option for §6's risk 1.

## 4. §12.3, checked row by row against what NeMo actually returns

The real `verbose_json` body, `/v1/audio/transcriptions`, diarization on:

```
{"duration":2.56…,"language":"","task":"transcribe","text":"…",
 "words":[{"confidence":1,"end":0.16,"speaker":1,"start":0,"word":"The"}, …]}
```

| §12.3 row | verdict | what NeMo actually does |
|---|---|---|
| `words[].word`, `.start`, `.end` → identity; **NeMo DOES report `end`** | ✅ **right** | seconds, both present on every word. Verified identical through the remap on 1,748 words |
| `words[].speaker` (int) → `"A"`, `"B"`, … by first appearance | ✅ **right**, with a detail | the integer is **1-based**, not 0-based (`recognizer.cpp:453`, `spk + 1`). Ordinal-by-first-appearance makes the base irrelevant; `{1:"A", 2:"B"}` verified |
| `{"segments":[{start,end,speaker:int}]}` — **textless** | ❌ **WRONG ENDPOINT** | that is `/v1/audio/diarizations` (`http_server.cpp:1001-1002`), which corrallm never calls. **`/v1/audio/transcriptions` returns NO `segments[]` at all** — so §12.9(e) **RULE 2 never fires for NeMo, and RULE 2b is the rule that does.** Confirmed live: `{"segments":[{"end":2.64,"speaker":1,"start":0}]}` from the diarizations route, 1-based speaker |
| verbose_json ONLY (400 otherwise) | ❌ **WRONG** | NeMo serves `json`, `verbose_json`, `text`, `srt`, `vtt` (`http_server.cpp:651`). All five measured 200 direct. It 400s only on a value outside that set — `diarized_json` included |
| *(never serialises `num_speakers`)* | ✅ **right** | top-level keys are exactly `duration, language, task, text, words` |
| — | ➕ **MISSING ROW** | **`words[].confidence`** is on every word and the canonical **drops it entirely** — it is not in `wireWord`, and `buildBackendSidecar` only harvests TOP-LEVEL unknown keys, so `include[]=backend` does not recover it either. §12.8's lossiness section does not mention it |
| — | ➕ **MISSING ROW** | **`language` is `""`** unless the caller sent `language=`. `canonTranscription.Language` is `omitempty`, so the canonical has **no `language` key at all** — see §5.2 |
| — | ➕ **MISSING ROW** | NeMo returns **no `usage`**; corrallm synthesises it from `duration`, as §12.1 intends |

**§12.8's "NeMo takes only `verbose_json`" is false, and so is the reason
written into `sttUpstreamFormat`'s comment** (`internal/proxy/sttremap.go`):
*"NeMo-Speech.cpp 400s on anything but verbose_json (source-read)"*. The
upstream rewrite is still right, but for ONE of its three stated reasons (oidio
silently ignores an unknown `response_format`) and not for this one. §12.11's
known gap 2 — "NeMo's `json` still asks upstream for `verbose_json` … that
ruling is now load-bearing for a backend nobody has run" — is answered: the
ruling holds, its justification was wrong, and the cost for NeMo is 901 bytes
upstream for a 55-byte answer.

**One more source-read claim corrected.** Phase 2's §4 table says a wav outside
NeMo's bounds was "forwarded → backend's 400". **NeMo answers HTTP 500**, not
400, for every intake refusal — `{"type":"server_error"}` with the text
`parse_wav` produced. Measured on all three refusal classes:

| file | DIRECT to NeMo | through corrallm |
|---|---|---|
| 24-bit 48 kHz stereo wav, real speech | **500** `unsupported WAV encoding; convert with: ffmpeg …` | **200**, correct transcript |
| 192 kHz wav | **500** `sample rate must be between 8 and 96 kHz` | **200** |
| mp3, real speech | **500** `unsupported container; convert with: ffmpeg …` | **200**, correct transcript |

That is phase 1 and phase 2's entire premise, demonstrated for the first time
against the backend rather than against a stub.

## 5. The corral layer end to end

Isolated daemon `127.0.0.1:6513`, own `--home`, own `--web-root`, own SQLite,
`--insecure`, every model a bare proxy target (no `cmd`, so no lifecycle and no
eviction). NeMo's own server on `127.0.0.1:6534`. Config:
`/tmp/claude-1000/p35nemo/iso/corrallm.yaml`.

### 5.1 The two rules, against real words

`words[].speaker` is an integer NeMo sets only when `diarization=true` is on the
form, so that field is passed through by the caller exactly as oidio's own
`num_clusters` is. corrallm adds nothing.

**RULE 2b (§12.9(e)) — derive segments from maximal same-speaker runs — is the
rule NeMo exercises, and every clause passes on all seven hearings**, checked
mechanically (`checkjoin.py`) rather than by eye:

- run count == segment count;
- `start` = the run's first word's `start`;
- `end` = the run's last word's `end` (NeMo reports one, so it is never omitted);
- `speaker` = the run's speaker;
- `id` = the zero-based run index as a string;
- rule 5's single-space join, trimmed, reproduced exactly;
- every canonical segment carries all five of `{id, speaker, text, start, end}`;
- labels are `A`, `B`, … by order of first appearance.

Largest case: 5,810 words → 34 segments, 4 speakers, `A`–`D`. **RULE 2 (join
words into a backend's own textless segments) still has no backend that
exercises it** — see §4, it was written for the wrong endpoint.

`backend.speakers[].index` (§12.8's "the integer survives") works on real data:
`{"name":"nemo-asr-diarize","speakers":[{"index":1,"label":"A"},{"index":2,"label":"B"}]}`.

### 5.2 oidio vs NeMo, same audio, same `diarized_json` — two shape differences

`H23-00065…mp4`, 725.4 s, through the same corrallm:

```
top-level NEMO : duration, segments, task, text, usage, words
top-level OIDIO: duration, language, segments, task, text, usage, words
  only in OIDIO: language                                   <-- DIVERGENCE 1
segments: NEMO n=50, OIDIO n=43 — item keys IDENTICAL {end,id,speaker,start,text}
words   : NEMO n=1748, OIDIO n=1722
  only in NEMO: end                                         <-- DIVERGENCE 2
usage   : identical {seconds,type}
```

**`segments[]` is byte-shape identical across two backends that share no code.
That is the corral layer's premise, and it holds.**

**Divergence 2 is intended and documented** — §12.6/§12.9(f): `words[].end` is
optional, oidio does not measure it, NeMo does. Not a bug.

**Divergence 1 is a CONTRACT ERROR and it is the most valuable thing here.**
§12.1 lists `language` as **"always"** on both `verbose_json` and
`diarized_json`. It is not: NeMo echoes `""` when the caller named no language,
`canonTranscription.Language` is `omitempty`, and the key vanishes. A client
that reads `.language` gets a string from oidio and `undefined` from NeMo on the
same route and the same format.

**The fix is the manifest, not the code** — and it is the same fix §12.1 already
made for `usage`. Emitting `"language": ""` would assert a language nobody
identified, which is exactly what §9.0b(f) forbade for `words[].end` and what
§12.10.2 settled for `usage`. So: **`language` is OPTIONAL — present when the
backend identified or was told one, absent otherwise.** Verified both ways:
sending `language=en-US` makes the key appear with `"en-US"`.

**Not changed here.** The manifest is normative under §9.0b and correcting it is
a §13 audit item, not a drive-by edit to another slice's file. It is listed in
§6 as the one blocking decision.

Content difference on the same 725 s, for the record and because it is the
number the shelf entry wants: **Sortformer found 2 speakers, oidio found 6**
(one of them holding 588.7 s and four holding under 31 s each) — the
over-splitting the shelf's DER note describes, seen directly.

### 5.3 The whole matrix

`examples/audio/speech.wav` through corrallm, NeMo backend:

| request | result |
|---|---|
| `json` | 200, **55 B** — `{"text":…}` and nothing else |
| `verbose_json` | 200, 527 B — `duration, task, text, usage, words`; words carry `end`, no `speaker` |
| `diarized_json`, no `diarization=true` | **400**, naming the model — §12.7 works against a second backend |
| `verbose_json` + `diarization=true` | 200, 766 B — segments derived, speakers `"A"` |
| `diarized_json` + `diarization=true` | 200, 766 B, `speaker` required |
| … `+ include[]=backend` | 200, 841 B — `speakers:[{index,label}]` |
| `text` / `srt` / `vtt` | 200, **byte-for-byte identical to NeMo direct** (45 / 78 / 84 B) |
| `diarized_json` + `stream=true` | 200 — delta, segment, done, `data: [DONE]` |
| `known_speaker_names[]=Alice` | **400 in 0.6 ms**, no backend call |
| `include[]=logprobs` | **400 in 0.4 ms**, no backend call |
| `/v1/audio/translations` | **501** from NeMo — see §6, risk 4 |

On the real 725 s file the synthesised stream is 102 events: 50 deltas, 50
`transcript.text.segment` (all five fields, speakers `A`/`B`), one
`transcript.text.done` with `usage`, then `[DONE]`.

### 5.4 Transcode in — all seven, native containers, nothing pre-converted

`--transcode-audio-max-seconds 3600`. Three containers, all 48 kHz, audio-only.

| file | s | transcode ms | rate | NeMo ttfb ms | rate | wall s | words | segs | spk |
|---|---|---|---|---|---|---|---|---|---|
| gonzales settlement call (m4a) | 1451 | 2885 | 503x | 37380 | 39x | 40.4 | 3105 | 78 | 2 |
| return hearing (mp4) | 2523 | 5530 | 456x | 72447 | 35x | 78.3 | 5211 | 54 | 4 |
| return hearing (opus) | 2523 | 5528 | 456x | 71953 | 35x | 77.7 | 5170 | 53 | 4 |
| H23-00065 (mp4) | 725 | 1476 | 491x | 16983 | 43x | 18.5 | 1748 | 50 | 2 |
| Larry renewal order (mp4) | 2650 | 6223 | 426x | 76691 | 35x | 83.2 | 5810 | 34 | 4 |
| Larry restraining order (mp4) | 841 | 1974 | 426x | 19875 | 42x | 21.9 | 2330 | 86 | 2 |
| BoE hearing (m4a) | 1302 | 3443 | 378x | 33354 | 39x | 37.0 | 3260 | 79 | 4 |

**Seven of seven at 200.** The mp4 and opus rows are the same recording in two
containers and agree to within 41 words and one segment, which is the closest
thing to a control this corpus has.

Also re-verified with the real backend in place: at the **1800 s default** the
2650 s file is refused **413 in 182 ms**, with no transcode and no backend call.
The blocking decision phase 2 raised is unchanged by anything here.

## 6. Active work

**◻ next** — nothing in this slice. The run is complete.

**blocking decision (owner)** — **`language` is documented as "always" and is
not** (§5.2). Two options and they are not equivalent: make the field OPTIONAL
in the manifest and in §12.1 (matches what `usage` already did, asserts nothing
nobody measured), or make the remap emit `"language": ""` (keeps the key on
every response, at the cost of publishing a value NeMo did not determine). The
first is recommended and consistent with §9.0b(f); neither was applied here,
because §12.1 belongs to another slice.

**risks**

1. **The CUDA compute buffer is retained across requests, and a longer request
   after a shorter one OOMs.** Measured, not theorised: the 2650 s file returned
   **HTTP 500 `failed to allocate named-tensor buffer … (out of device memory?)`**
   after a 725 s and an 841 s request had left ~4.6 GB held; the identical
   request on a freshly started server returned **200 in 83.8 s**. corrallm
   forwards the 500 with NeMo's wording, which is honest and is also exactly the
   late failure §0 of the corral layer exists to prevent — and corrallm cannot
   predict it, because it depends on what the backend served previously. On a
   10 GB card with 4.9 GB free, Parakeet TDT's full-context attention leaves
   419 MiB of headroom at 44 minutes of audio. `nemotron-3.5-asr-streaming-0.6b`
   has a bounded attention window and is the mitigation if one is wanted.
2. **`words[].confidence` is destroyed** (§4). NeMo measures a per-word
   confidence and no caller of corrallm can ever see it — not in the canonical,
   not in `include[]=backend`. Whether that is right is a schema question; that
   it is undocumented is not.
3. **RULE 2 is STILL unexercised.** It was written for a shape
   `/v1/audio/transcriptions` does not return. It remains unit-covered only, and
   it is now known that NeMo will not be the backend that covers it.
4. **`/v1/audio/translations` is untested against NeMo.** This build has
   `NEMO_SPEECH_BUILD_NMT=OFF`, so the route answers 501 and corrallm forwards
   it. Testing it needs the `llama.cpp` submodule and an NMT model. Contract 3
   (`"task":"transcribe"` on a translation) is therefore still oidio-only.
5. **The 27-speaker rollover is still unit-covered only.** The largest real
   recording has 4 speakers and Sortformer v2 cannot report more.
6. **One backend, one model pair.** Every number here is Parakeet TDT v3 +
   Sortformer v2 at q8_0 on one card. Nothing says the shape holds for Parakeet
   CTC 1.1B or for PR #50's Nemotron 3 Diarization when it lands.

**assumptions recorded**
- that `diarization=true` is the caller's to send. corrallm does not add it, and
  a NeMo-backed model asked for `diarized_json` without it gets §12.7's 400 —
  which is correct but is a trap for a client that never had to think about it
  with oidio.
- that `--gpu 0` plus `CUDA_VISIBLE_DEVICES` by UUID is the only safe way to
  name a GPU on this box (§2).
- that a second corrallm in front of the production one is a fair test of the
  remap. It is the rig §12.11 already used, and it is the only way to reach
  oidio without reconfiguring `:8111`.

**optional extensions, explicitly out of scope**
- build with `NEMO_SPEECH_BUILD_NMT=ON` and settle contract 3 against NeMo.
- put `words[].confidence` somewhere (risk 2) — a schema decision, not a fix.
- score Sortformer against the three hearing truth files with `oidio verify
  score`, for a same-corpus DER against the 55.8 / 55.0 / 27.9 baseline. The
  shelf entry asks for exactly this and it is now runnable; it was not done here
  because it is a quality question, not a corral-layer one.
- a `nemo-speech`-shaped entry in `internal/config` presets, so the intake
  declaration in §5's config does not have to be retyped.
