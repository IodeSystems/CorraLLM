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

> **Labels.** The seven files are the owner's legal matters and are labelled
> A–F here by container and duration alone. Case numbers, party names and
> matter types are deliberately absent: they are not technical facts, nothing
> in this document needs them, and a plan file is the wrong place for them to
> live. The mapping is not recorded anywhere in this repo.

### 5.2 oidio vs NeMo, same audio, same `diarized_json` — two shape differences

Hearing C, 725.4 s, through the same corrallm:

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
| call A (m4a) | 1451 | 2885 | 503x | 37380 | 39x | 40.4 | 3105 | 78 | 2 |
| hearing B (mp4) | 2523 | 5530 | 456x | 72447 | 35x | 78.3 | 5211 | 54 | 4 |
| hearing B (opus, same recording) | 2523 | 5528 | 456x | 71953 | 35x | 77.7 | 5170 | 53 | 4 |
| hearing C (mp4) | 725 | 1476 | 491x | 16983 | 43x | 18.5 | 1748 | 50 | 2 |
| hearing D (mp4) | 2650 | 6223 | 426x | 76691 | 35x | 83.2 | 5810 | 34 | 4 |
| hearing E (mp4) | 841 | 1974 | 426x | 19875 | 42x | 21.9 | 2330 | 86 | 2 |
| hearing F (m4a) | 1302 | 3443 | 378x | 33354 | 39x | 37.0 | 3260 | 79 | 4 |

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

---

# P35 — Nemotron 3 Diarization, the run yesterday was one commit short of

> **2026-09-24.** Everything above is the **Sortformer v2 baseline** and is
> deliberately not rewritten: its value is being the thing this section is
> compared against. Same seven files, same labels, same rig, same table
> columns. Read §1–§6 first; this section only records what MOVED.

## 7. What changed upstream, and what it cost to build

Yesterday's build was `302ebc9`, the **parent of PR #50**. Overnight two
commits landed and both are merged:

- **`8c15060`** — `feat(diar): Nemotron 3 Diarization` (#50), the V3 31-layer
  RoPE encoder.
- **`97a15af`** — `feat(diar): make Nemotron 3 Diarization the default
  diarizer` (#52).

No release past `v0.1.0`, so this is a source build from merged `main` at
`97a15af`, in a fresh copy at `/tmp/claude-1000/p35nemo2/src`.

**§2's configure line is unchanged and still correct** — it was pasted verbatim
and configured clean on the first try. All three environment traps still apply
and all three were still needed: `gcc-13`/`g++-13` pinned for BOTH languages,
`-DCMAKE_CUDA_ARCHITECTURES=86-real`, and `CUDA_VISIBLE_DEVICES` set to the
3080's UUID. Nothing about the build itself moved.

**What #50/#52 changed about the build, concretely:**

| | |
|---|---|
| ggml submodule pin | **unchanged** — `.gitmodules` and the gitlink are byte-identical across `302ebc9..97a15af` |
| ggml patch series | **21 → 23**. New: `0022-cuda-q8-gelu-fusion`, `0023-cuda-skinny-q8-cache-lifetime` |
| applying them | `scripts/apply-ggml-patches.sh` refuses a partially-patched tree — it compares the WHOLE worktree against the pinned commit plus the full series. The 21-patch tree had to be reset to the pin and all 23 re-applied from clean. §2's per-patch `git apply` shortcut no longer works |
| CMake cache | not relocatable — it stores absolute source/build paths, so a copied build tree needs `build/` deleted before reconfiguring |
| new encoder | `src/asr/encoder/rope_transformer.{cpp,h}`, 250 lines, new to the ASR library |
| build time | **4 m 29 s wall** at `-j 20`, 235 targets, exit 0, no errors |
| SentencePiece | the existing static `.deps/sentencepiece` was found unchanged; no rebuild |

**Model.** `models/index.json` now ships a **prebuilt q8_0 GGUF** —
`nvidia/Nemotron-3-Diarization`, rev `f667ed73`,
`Nemotron-3-Diarization.q8_0.gguf`, **107,012,128 B**, sha256 verified against
the index after download. **The shelf entry's "No GGUF, no ONNX, no
safetensors — conversion is `conversion/diarization.py`" is now stale**: no
conversion step is needed. #52 also flipped `defaults.diarization` in that index
from `diar_streaming_sortformer_4spk-v2` to `Nemotron-3-Diarization`.

**Capacity.** `include/nemo_speech/diar.h`: V2 is **4 speakers at an 80 ms**
output cadence, V3 is **8 speakers at 10 ms**. The geometry preset is resolved
from the model file itself (`v3-streaming` / `v3-offline`), so passing only
`--diar-model <the v3 gguf>` is enough — no flag change in `serve`.

## 8. VRAM — it fits, and it is SMALLER resident than v2

| | v2 (2026-09-23) | v3 (2026-09-24) |
|---|---|---|
| diarizer file | 147,075,776 B | **107,012,128 B** |
| weights resident, both models | 1,141 MiB | **1,101 MiB** |
| free on the 3080 before | 4,970 MiB | 4,970 MiB |
| peak observed across the sweep | 9,821 MiB of 10,240 | **9,851 MiB of 10,240** |

**It fits, with 389 MiB of headroom at the peak.** The 31-layer encoder does not
cost more resident memory than the 17-layer one — its q8_0 file is 27% smaller.
Peak is dominated by Parakeet TDT's full-context attention compute buffer, not
by the diarizer, which is why the ceiling barely moved.

Nothing was evicted, the 5090 was never touched (1,458–1,466 MiB free
throughout), and production `:8111` answered 200 on `/v1/models` before, during
and after. No `vite build` ran.

## 9. The headline — speaker counts, three diarizers, same seven files

Same isolated corrallm on `:6513`, own `--home`/`--web-root`/SQLite, every model
a bare proxy target. NeMo on `:6534`, restarted fresh per file. oidio reached as
a pure proxy through `:8111` — nothing spawned, loaded or evicted.

| file | s | **Sortformer v2** | **Nemotron 3** | **oidio** |
|---|---|---|---|---|
| call A (m4a) | 1451 | 2 | **2** | **10** |
| hearing B (mp4) | 2523 | 4 | **8** | **8** |
| hearing B (opus, same recording) | 2523 | 4 | **7** | **7** |
| hearing C (mp4) | 725 | 2 | **2** | **6** |
| hearing D (mp4) | 2650 | 4 | **5** | **15** |
| hearing E (mp4) | 841 | 2 | **3** | **5** |
| hearing F (m4a) | 1302 | 4 | **4** | **9** |

**Seven of seven at 200 for all three.** Word counts are **identical between v2
and v3 on every file** (3105 / 5211 / 5170 / 1748 / 5810 / 2330 / 3260) — same
ASR, same decode; only the labels moved. oidio's word counts are its own and
lower throughout.

**The shelf entry's argument does not survive contact with this corpus.** It
says Sortformer "has no clustering step, so over-splitting is not a failure mode
it has". Per-speaker segment TIME says otherwise for V3:

| file | Nemotron 3, time per speaker | oidio, time per speaker |
|---|---|---|
| hearing B (mp4) | A 2249 s, then **81 / 21 / 19 / 14 / 12 / 6 / 1 s** | A 1637, C 598, then 154 / 98 / 14 / 10 / 6 / 4 s |
| hearing D | A 1777, B 695, C 105, D 58, **E 1 s** | B 1194, C 433, H 366, M 191, then 11 more ≤ 153 s |
| hearing F | B 769, A 481, C 13, **D 0 s** | A 953, B 144, then 7 more ≤ 75 s |

V3's extra labels over V2 are **slivers**: seven of hearing B's eight speakers
hold 81 s or less between them, and hearing D's fifth speaker holds **one
second**. That is over-splitting, in the same shape the shelf blamed oidio for
(hearing C: one cluster of 588.7 s and four under 31 s). It is far milder than
oidio's — oidio invents 15 speakers on hearing D and 10 on a two-party call —
but it is the same failure mode, and the claim that Sortformer cannot have it is
now measured false.

**The mp4/opus pair is the control, and V3 fails it where V2 passed.** It is one
recording in two containers.

| | v2 | v3 | oidio |
|---|---|---|---|
| speakers, mp4 vs opus | **4 / 4** | **8 / 7** | 8 / 7 |
| segments | 54 / 53 | 72 / 51 | 120 / 127 |
| largest speaker | 2250 s / 2254 s | 2249 s / 2238 s | A 1637 s / **B 1214 s** |

V2 agreed with itself to within one segment. V3 disagrees by one whole speaker
and by 21 segments on audio that differs only by codec. oidio is worse again: it
does not even agree on the partition, splitting 2523 s as 1637+598 one way and
1214+722+242+233 the other. **The 41-word transcode difference between the two
containers is enough to move V3's speaker count. That is a stability result, and
it is not a good one.**

## 10. Ground truth — the only place a DER can honestly be computed

The private corpus has no reference, so no DER claim is made about it. #50 ships
**`test_files/diar/ami_en2002d_2132.wav`** — 60 s of AMI EN2002d, CC BY 4.0,
with a word-aligned RTTM. Public data, four reference speakers (the fourth
speaks only a few words). Frame-level 10 ms scoring, collar 0, overlap counted,
best injective reference→hypothesis map.

**On NeMo's own `/v1/audio/diarizations` — a like-for-like diarizer test:**

| diarizer | DER | miss | false alarm | speakers found |
|---|---|---|---|---|
| Sortformer v2 | 29.73 % | 957 fr | 427 fr | 3 of 4 |
| **Nemotron 3** | **27.56 %** | 740 fr | 543 fr | 3 of 4 |

**V3 is better by 2.2 points where there is ground truth.** Both miss the fourth
speaker. This is one 60 s clip and is not a benchmark; it is the only honest
number available on this box.

**Through corrallm's canonical `segments[]`, all three:**

| model | DER | speakers found |
|---|---|---|
| NeMo + Sortformer v2 | 50.11 % | 3 |
| NeMo + Nemotron 3 | 52.41 % | 3 |
| oidio | **135.01 %** | 6 |

**These three numbers do not measure diarizers and must not be quoted as if they
did.** RULE 2b's canonical segment is a maximal same-speaker run of ASR WORDS,
so it spans the silences between them; scored against an RTTM it produces false
alarm by construction (1,721–3,776 frames here). The projection dominates the
error and it reverses the v2/v3 ordering. **`oidio verify score` against the
three hearing truth files — §6's optional extension and the shelf's "first step
on resume" — must therefore be run on a diarization-only output, not on a
corrallm transcription body.** That is a correction to how that task was
specified, and it is worth more than the numbers above.

## 11. Timings, against yesterday's

`--transcode-audio-max-seconds 3600`, `diarized_json` + `diarization=true`.

| file | s | transcode ms | rate | **v3 ttfb ms** | rate | wall s | words | segs | spk |
|---|---|---|---|---|---|---|---|---|---|
| call A (m4a) | 1451 | 3087 | 470x | 40266 | 36x | 43.5 | 3105 | 74 | 2 |
| hearing B (mp4) | 2523 | 6856 | 368x | 82470 | 31x | 89.6 | 5211 | 72 | 8 |
| hearing B (opus) | 2523 | 5288 | 477x | 81658 | 31x | 87.2 | 5170 | 51 | 7 |
| hearing C (mp4) | 725 | 1625 | 446x | 18021 | 40x | 19.7 | 1748 | 54 | 2 |
| hearing D (mp4) | 2650 | 5553 | 477x | 88252 | 30x | 94.1 | 5810 | 30 | 5 |
| hearing E (mp4) | 841 | 1646 | 511x | 21686 | 39x | 23.4 | 2330 | 97 | 3 |
| hearing F (m4a) | 1302 | 4270 | 305x | 37820 | 34x | 42.3 | 3260 | 45 | 4 |

**V3 costs 6–15 % more time than V2** — ttfb ratio 1.06 / 1.14 / 1.14 / 1.06 /
1.15 / 1.09 / 1.13, mean 1.11. §5.4's v2 rates were 35–43x realtime; v3 is
30–40x. Transcode is ffmpeg and unrelated to the diarizer; its spread here is
disk noise.

**oidio, for scale, on the same seven files through the same corrallm:**
354.5 / 626.4 / 643.0 / 182.1 / 652.1 / 219.3 / 338.6 s wall — a flat **~4x
realtime**. NeMo is **8–9x faster than oidio on identical input**, and that gap
is the same for both diarizers.

## 12. §4's four corrections, re-checked against `97a15af`

`http_server.cpp` is untouched by `302ebc9..97a15af`, and it shows: **all four
hold, none moved.**

| correction | verdict at `97a15af` |
|---|---|
| `/v1/audio/transcriptions` returns **no `segments[]`** | ✅ **holds.** Top-level keys are exactly `duration, language, task, text, words`. `/v1/audio/diarizations` still returns the textless `{"segments":[{end,speaker,start}]}` with a 1-based speaker. **§12.9(e) RULE 2 therefore still never fires for NeMo, and RULE 2b is still the rule that does** |
| `words[].confidence` present, and destroyed | ✅ **holds.** On the wire: `{confidence, end, speaker, start, word}`. Through corrallm: `{end, speaker, start, word}`. `include[]=backend` returns `{name, speakers:[{index,label}]}` and does **not** recover it — §6's risk 2 is unchanged |
| intake refusals are **500**, not 400 | ✅ **holds.** 24-bit 48 kHz stereo wav → 500 `unsupported WAV encoding`; 192 kHz → 500 `sample rate must be between 8 and 96 kHz`; mp3 → 500 `unsupported container`. All three still **200 through corrallm**, transcoded |
| NeMo serves five `response_format`s, not `verbose_json` only | ✅ **holds.** `json` / `verbose_json` / `text` / `srt` / `vtt` all 200 direct; `diarized_json` and an unknown value both 400 |

**§5.3's whole matrix reproduces byte for byte** on `examples/audio/speech.wav`:
`json` 55 B, `verbose_json` 527 B, `diarized_json` without the flag **400**,
`verbose_json`+diar 766 B, `diarized_json`+diar 766 B, `+include[]=backend`
841 B, `text`/`srt` 45/78 B, stream → delta/segment/done/`[DONE]`. Both §5.2
divergences also reproduce: only oidio has `language`, only NeMo has
`words[].end`, and `segments[]` item keys are identical across the two backends
at `{end, id, speaker, start, text}`.

**RULE 2b passes all eight clauses on all seven files under V3**, checked
mechanically with the same `checkjoin.py`. V3 is the first thing on this box to
exercise more than four labels: hearing B returns **`A` through `H`** on both
`segments[].speaker` and `words[].speaker`, ordinal by first appearance, `id` a
string. §6's risk 5 is narrowed — the label ladder is now real-data-tested to 8,
though the 27-speaker rollover remains unit-covered only.

## 13. The `language` contract error is FIXED, in the manifest

§5.2's contract error reproduces exactly at `97a15af`: oidio returns
`"language":"en"`, NeMo returns no `language` key at all, same route and same
`diarized_json`. Sending `language=en-US` makes NeMo's key appear with
`"en-US"`.

**Fixed as §12.1 already fixed `usage` — by correcting the manifest, not the
remap.** `language` is now stated **OPTIONAL: present when the backend
identified one or the caller named one, absent otherwise, never guessed by
corrallm**, and the manifest now names who reports it (oidio always; NeMo only
back to a caller who sent `language=`). Two edits, and only these two:

- `plan/p35-corral-layer.md` §12.1 — the `verbose_json` row, a correction
  paragraph beside the `usage` one, and the `diarized_json` row split so
  `language` and `usage` are no longer lumped under "always".
- `internal/proxy/proxy.go` — the `/v1/capabilities` `response_format` note:
  `{task, language?, …}` and a FIELD RULE sentence next to `usage`'s.

**§6's blocking decision is closed.** The recommended option was taken.

## 14. The one real regression — a V3 OOM kills the server

§6's risk 1 (the retained CUDA compute buffer) is **worse under V3, and in a
different way.** Same sequence both times on one server — hearing C (725 s),
then hearing E (841 s), then hearing D (2650 s) — both diarizers on **the same
`97a15af` build**, so the build is not the variable:

| diarizer | 725 s | 841 s | 2650 s | server afterwards |
|---|---|---|---|---|
| Sortformer v2 | 200 | 200 | **500** `failed to allocate named-tensor buffer … (out of device memory?)` | **alive, 200 on `/ready`** |
| **Nemotron 3** | 200 | 200 | **502 from corrallm** | **DEAD — process gone, port closed** |

Under V3 the allocation fails inside `SortformerModel::SortformerBatcher::execute`
→ `ggml_cuda_graph_evaluate_and_capture` → `cudaGraphLaunch` → **`CUDA error:
out of memory` → `ggml_abort` → the whole process aborts.** corrallm answers 502
because the connection dropped mid-request, and every later request to that
backend fails too.

**This is a behaviour change, not a capacity change.** V2 turns an OOM into a
per-request 500 and keeps serving; V3 turns the same OOM into a fatal abort. On
a box where the fit depends on what the backend served previously — which
corrallm cannot predict — that is the difference between one bad answer and an
outage. A fresh server serves the 2650 s file fine under both (94.1 s, 200).

## 15. Active work

**◻ next** — nothing in this slice. Both sweeps are complete and the manifest
fix is in.

**blocking decision (owner)** — **is Nemotron 3 actually the diarizer to
adopt?** The evidence now cuts both ways and the call is a product one.
*For*: 2.2 points better DER on the only ground truth available, 8 speakers
instead of 4, smaller resident, still 8–9x faster than oidio, and upstream's own
default. *Against*: it over-splits into sub-second speakers on real recordings,
it does not reproduce its own speaker count across two containers of one
recording, it costs 11% more time, and its OOM kills the server. **oidio is
worse on every one of those axes**, so "replace oidio's segmenter" is still the
right direction; "Nemotron 3 rather than Sortformer v2" is the part that is not
established.

**risks**

1. **A V3 OOM aborts the process** (§14). New, V3-specific, and the most serious
   thing in this section.
2. **V3 does not reproduce its own speaker count across containers** (§9). V2
   did. Nothing downstream should treat the count as stable.
3. **V3 emits sub-second speakers** (§9) — a 1 s speaker and a 0 s speaker in
   this corpus. A caller counting distinct `segments[].speaker` will be wrong.
4. **`words[].confidence` is still destroyed** (§12). Unchanged.
5. **RULE 2 is still unexercised** (§12). Unchanged, and still for the same
   reason.
6. **`/v1/audio/translations` still untested** — `NEMO_SPEECH_BUILD_NMT=OFF`
   here too.
7. **The DER numbers are one 60 s public clip** (§10). They are not a benchmark,
   and the private corpus still has no reference on this box.

**assumptions recorded**
- that the label mapping to A–F is the duration+container match §5.4 already
  used. It was re-verified mechanically against that table before any file was
  opened, and no basename appears in this document or in any output.
- that oidio through the `:8111` proxy is a fair oidio reading. It is §5's rig
  unchanged.
- that yesterday's v2 column is comparable. Same files, same rig, same columns,
  same host; only the diarizer and the upstream commit differ.

**optional extensions, explicitly out of scope**
- report the V3 abort-on-OOM upstream. It reproduces in ~2 minutes on a 10 GB
  card, and the v2 path on the same build shows the graceful behaviour is
  achievable.
- run `oidio verify score` on **diarization-only** output (§10's correction) for
  a same-corpus DER against the 55.8 / 55.0 / 27.9 baseline.
- `--preset v3-offline`, and the V3 geometry overrides, are untouched here —
  every V3 number above is the model's own default preset.

---

# P35 — NeMo-Speech.cpp is a corrallm BACKEND, not a thing in /tmp

> **2026-09-25.** §1–§15 are two measurement runs against a hand-started server
> in `/tmp`. This section is what makes it reproducible: a toolchain recipe, a
> config entry, and the placement arithmetic for the card it has to live on.
> **Nothing here was written to the production config DB.**

## 16. The recipe — `internal/toolchain/recipes/nemo-speech.cpp.sh`

Shaped on `stable-diffusion.cpp.sh` (submodules, a backend that is off unless
asked for, a banner on the wrong stream). **Five things are not inherited from
it**, each because copying would produce a build that fails or an answer that
lies:

| | |
|---|---|
| **the tree patches its own ggml** | `scripts/apply-ggml-patches.sh` edits the ggml submodule's WORKTREE (23 patches). A dirty submodule makes the SUPERPROJECT's `git diff --quiet` exit 1 — **verified** — which is exactly what `align_tree` refuses to move. Unhandled, the checkout freezes at the patched commit on the SECOND build and silently stops taking upstream, while every verb keeps answering. `reset_vendored_ggml` runs BEFORE `align_tree`; sd.cpp's `sync_submodules` runs only after. A recipe test asserts that ordering |
| **`-real` on every arch** | `86;120` becomes `86-real;120-real`. The driver is CUDA 13.2, the only toolkit is 13.3, so any PTX is newer than the driver can consume. llama.cpp passes the bare list on this same box and is right to — every card has SASS and nothing JITs — so this cannot be inherited, it has to be decided. `CUDA_ARCHS` is passed through verbatim for anyone who wants the PTX back |
| **`cmake --preset`, not `scripts/configure.sh`** | configure.sh demands the `llama.cpp` submodule for EVERY ASR preset (`scripts/configure.sh:142-144`), for `vendor/miniaudio/miniaudio.h`, which only `NEMO_SPEECH_BUILD_MIC_CAPTURE=ON` compiles. The recipe does configure.sh's two useful jobs itself and skips the clone |
| **a dependency that is not a submodule** | SentencePiece is REQUIRED by `src/asr/CMakeLists.txt:71-103` and is not vendored. Built static ONCE per prefix into `<prefix>/deps`, not upstream's `<src>/.deps` default — `git clean -xdf` deletes that (`-x` takes ignored files too), so every build would re-clone and recompile it |
| **the banner carries no commit** | `nemo-speech --version` prints `nemo-speech 0.1.0` to stdout and nothing else. `CMakeLists.txt:12-22` reads the VERSION file; there is no `git describe`. A third answer to the question llama.cpp (stderr, with a commit) and sd.cpp (stdout, with a commit) answer differently. **The stamp is the only thing that identifies a build**, so an ADOPTED install can never be told it is behind |

Also: `preflight` requires **ninja** (every preset sets `"generator": "Ninja"`
and a preset's generator cannot be overridden on the command line) with the
right package name per manager; it reports the `cc`/`c++` disagreement by
COMPILER FAMILY, because box1's `cc` introduces itself as `cc (Ubuntu 13.3.0…)`
and never says "gcc". The toolchain is pinned to **gcc/g++**, where llama.cpp
and sd.cpp pin clang — not a preference, it is the toolchain this tree has
actually been compiled with, twice.

`preflight` on box1 today returns `ok:false` for exactly one reason: **ninja is
not installed.** Everything else passes.

## 17. The config, and the intake it declares

Verified end to end on an isolated daemon (`:6514`, own `--home`/`--web-root`/
SQLite/tune-cache, NeMo on `:6541`), with the tool ADOPTED at the existing
`/tmp` build so no 4m29s compile was needed to test the config.

```yaml
tools:
    nemo-speech.cpp:
        url: https://github.com/NVIDIA/NeMo-Speech.cpp.git
        ref: main
        pin: 97a15afa5caa9bce5baaa86c1184103877af4101   # #50 + #52
        bin: nemo-speech
        hosts:
            box1: {}          # managed: corrallm clones and builds

providers:
    local:
        models:
            nemo-stt-diarize:
                sticky: { ttl: 2m, evictCost: low, idleUnload: 5m }
                cmd: |-
                    CUDA_VISIBLE_DEVICES=GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544 \
                    NEMO_SPEECH_MODEL_DIR=/home/nthalk/.corrallm/models/nemo-speech \
                    exec ${tool:nemo-speech.cpp}/nemo-speech serve \
                        --host 127.0.0.1 --port 5820 \
                        --asr-model nvidia/parakeet-tdt-0.6b-v3 \
                        --diar-model nvidia/Nemotron-3-Diarization \
                        --gpu 0 --no-ui --threads 2 \
                        --max-upload-mb 512 \
                        --read-timeout 900 --write-timeout 900
                server: box1
                pool: gpu1
                ramUsage: { gpu1: 4900MiB, system: 1GB }
                swap: { loadSeconds: 3 }
                proxy: 5820
                type: stt
                maxConcurrent: 1
                contextPerRequest: 0
                modalities:
                    audio:
                        formats: [wav]
                        minSampleRate: 8000
                        maxSampleRate: 96000
                        maxChannels: 2
                        pcm: [s16, f32]
```

**Every bound is read off `src/common/audio_file.cpp` at `97a15af`**, the
function corrallm's phase 2 was written against, and matches `nemoIntake()` in
`internal/proxy/audio_test.go:727-737` field for field:

| field | value | source |
|---|---|---|
| `formats` | `[wav]` | `parse_wav:38` — RIFF/WAVE or `unsupported container` |
| `maxChannels` | `2` | `audio_file.cpp:77` — `channels != 1 && channels != 2` |
| `minSampleRate` / `maxSampleRate` | `8000` / `96000` | `audio_file.cpp:79` |
| `pcm` | `[s16, f32]` | `audio_file.cpp:82-89` — format 1 @ 16 bits, format 3 @ 32 bits, everything else `unsupported WAV encoding` |

`type: stt` is what makes it `audio.stt`: `config.TypeIO` matches `stt` (also
`asr`/`parakeet`/`transcri*`) — audio in, text out — then `IO.capability()`
(`internal/config/iomodality.go:83-120`). There is no `capability:` field to set.

`maxConcurrent: 1`, unlike oidio's `stt-diarize: 2`. The compute buffer is
per-process and per-request; two 44-minute requests in flight would double
§14's allocation on a card that has 389 MiB of headroom at one.

**The intake works against the real backend, from this entry.** Isolated
`:6514`, `local-nemo-stt-diarize`:

| input | DIRECT to NeMo | through corrallm |
|---|---|---|
| 16 kHz mono s16 wav | 200 | **200** (11.1 s, cold; the transcript is correct) |
| 24-bit stereo 48 kHz wav | **500** `unsupported WAV encoding` | **200**, correct transcript |
| mp3 | (`unsupported container`) | **200**, correct transcript |
| 726 s wav, `diarized_json` + `diarization=true` | — | **200 in 18.6 s**, 1447 words, canonical `segments[]` = `{end,id,speaker,start,text}`, `usage: {type:duration, seconds:726}` |

`/v1/models` reports `"capability":"audio.stt"` and echoes all four PCM bounds.

## 18. What fits on the 3080 — it displaces chandra, and that is the whole answer

**Measured today, not carried over.** gpu1 is the RTX 3080 (gpu0 is the 5090);
gpu1 declares **10240MiB and NO reserve**, and `--vram-margin` keeps 512 MiB
back, so the admission budget is **9728 MiB**.

Peaks from the tune cache (`~/.corrallm/var/vram-profile.json`) and from this
slice's own run:

| model | PeakMiB on the 3080 |
|---|---|
| `chandra-ocr-2` | 8240 |
| `nomic-embed-text` | 822 |
| **`nemo-stt-diarize`** | **4510** — measured here on a 726 s file, Parakeet TDT v3 + Nemotron 3 |

| combination | MiB | verdict |
|---|---|---|
| nomic + chandra (today) | 9062 | fits, 666 MiB spare |
| nomic + **NeMo** | 5332 | **fits, 4396 MiB spare** |
| chandra + NeMo | 12750 | **over by 3022 MiB — never** |
| all three | 13572 | over by 3844 MiB |

**NeMo and chandra-ocr-2 cannot be co-resident on this card, at any quant of
either, by 3 GB.** That is not a tuning question. The owner's options, with
costs, and this slice does not choose between them:

1. **Let them evict each other.** chandra is already `evictCost: low`,
   `ttl: 2m`, `idleUnload: 5m` — it is built to be thrown off. NeMo with the
   same profile means whichever was used last holds the card. **Cost:**
   interleaved OCR and STT traffic thrashes — chandra's cold start is a 5.2 GB
   GGUF load, NeMo's is ~1.8 s (measured). Cheapest, and reversible.
2. **Move chandra to the 5090.** It has a measured gpu0 profile (Peak 7080). But
   gpu0 holds `local-Swift-Qwen3.8-27B` at Peak 30688 of a 32095 budget, so
   chandra can only land there by evicting the chat model. **Cost:** moves the
   problem onto the card that serves chat.
3. **Bound NeMo's peak.** `nemotron-3.5-asr-streaming-0.6b` has a limited
   attention window and measured 2005 MiB (§3). **Cost:** it still does not fit
   beside chandra (2005 + 8240 = 10245 > 9728) — it only buys more spare next to
   nomic. It buys nothing against the constraint that actually binds.
4. **Do not adopt it on box1.** oidio keeps serving STT from the `system` pool.
   **Cost:** oidio is 8–9x slower and over-splits speakers far worse (§9, §11).

**A footnote that matters for option 1.** The compute buffer is RETAINED after
a request — verified: the process still held 4510 MiB minutes after the 726 s
file finished. `idleUnload: 5m` is therefore not just residency hygiene, it is
the mitigation for §6's risk 1: a quiet backend gives the buffer back instead of
ratcheting into the next long request.

**And a real caveat about `ramUsage`.** corrallm does NOT trust it: at the first
spawn it measures and logs `using measured VRAM footprint instead of configured
ramUsage … config=over-declared configuredMiB=4900 measuredMiB=1198`. **1198 is
weights only.** Admission charges that until `sampleVRAMPeak`'s 15 s ticker has
seen a long request — after which PeakMiB ratchets to 4510 and stays (verified,
and it survives a respawn). So there IS a window, on a cold tune cache, where
corrallm believes NeMo costs 1.2 GB and would happily co-schedule chandra
beside it. **Warming the profile with one long file before real traffic closes
it permanently.** (`tune.Cache.BumpPeak` mutates in memory and does not write
the file; `vram-profile.json` catches up at the next spawn. `corrallm
introspect` reads the file, so it can lag the daemon by one spawn.)

## 19. §14's abort IS survivable, and it costs exactly one 502

Measured on the isolated rig by sending the backend **SIGABRT**, which is what
`ggml_abort` raises:

```
INFO backend exited name=local-nemo-stt-diarize err="signal: aborted (core dumped)"
INFO backend spawned name=local-nemo-stt-diarize id=pgid:2605004
INFO backend ready  name=local-nemo-stt-diarize
```

| moment | what the caller sees |
|---|---|
| the request that races the exit | **502 in 1.7 ms** |
| the next request | **200 in 1.8 s** — cold respawn, model load included |
| every request after | normal |

**How, and its limit.** There is no supervisor and no restart policy —
`Manager.onProcExit` (`internal/proc/manager.go:1761`) only drops the process
from the ledger and frees its pools. What makes this survivable is that
`EnsureReady` then takes the cold-spawn path on the NEXT demand. So recovery is
**lazy, not proactive**: nothing notices the death until a caller does, and that
caller pays for it. `waitHealthy` polls `GET /health` (NeMo serves it,
`server/http/http_server.cpp:448`) only during a spawn, never while resident.

**This is as good as the existing machinery gets, and it is good enough**: one
lost request per abort, no operator action, no outage. What it cannot do is
prevent the abort — that still depends on what the backend served before, which
corrallm cannot predict. §18's `idleUnload: 5m` is the only lever that reduces
how often the condition arises.

## 20. Adopting it in production

Nothing below was run. `:8111` was never restarted, stopped, reconfigured or
installed over, and answered 200 on `/v1/models` before, during and after. No
`vite build` ran. Both GPUs were returned empty.

```bash
sudo apt-get install -y ninja-build          # preflight's one failure on box1
corrallm config export > ~/corrallm-pre-nemo.yaml   # rollback point
# edit that file: add §17's tools: and providers.local.models: entries
corrallm validate --config ~/corrallm-pre-nemo.yaml
corrallm config load ~/corrallm-pre-nemo.yaml
corrallm tools preflight nemo-speech.cpp     # expect ok:true once ninja is in
corrallm tools build nemo-speech.cpp         # ~4m29s + a one-time SentencePiece build
# warm the VRAM profile before real traffic (see §18's caveat):
curl -F model=local-nemo-stt-diarize -F response_format=json \
     -F file=@<a file over ~10 minutes> http://127.0.0.1:8111/v1/audio/transcriptions
```

Rollback is `corrallm config restore <revision>` (`corrallm config history`).

## 21. Active work

**◻ next** — nothing. The recipe and the config exist and are exercised.

**blocking decision (owner)** — **§18's placement.** NeMo displaces
`chandra-ocr-2` on the 3080 by 3 GB. Four options with their costs are listed;
none was taken.

**risks**

1. **The recipe has never performed a build.** Every verb but `build` ran on
   this host (`preflight` correctly found the one missing package); the config
   was exercised against an ADOPTED tree instead, because a real run would clone
   NeMo-Speech.cpp and SentencePiece and compile for ~4m30s. The steps `build`
   takes are each verified against the upstream scripts and against the git
   semantics they depend on, but the end-to-end compile through this recipe is
   UNTESTED.
2. **The `-real` decision is box-shaped.** It is right here and right on any box
   whose driver is older than its toolkit; on a matched box it costs the JIT
   fallback for an unlisted arch and buys nothing.
3. **The cpu-asr and metal-asr paths are untested.** Both are reachable from
   `backend_spec` and neither has been built. Vulkan is deliberately not offered.
4. **`maxConcurrent: 1` is reasoning, not measurement.** Concurrency against
   NeMo has never been run.
5. **Everything §15 lists is unchanged** — V3 over-splits, does not reproduce
   its speaker count across containers, and destroys `words[].confidence`.

**assumptions recorded**
- that `ramUsage: 4900MiB` is the right bootstrap. Measured peak is 4510 on a
  726 s file; a 2650 s file measured higher in §8. It is deliberately on the
  conservative side and corrallm overrides it with measurement anyway.
- that the served name is `local-nemo-stt-diarize` (provider prefix), matching
  every other local model. `aliases:` can add a bare name if a caller needs one.

**optional extensions, explicitly out of scope**
- run `corrallm tools build nemo-speech.cpp` for real and close risk 1.
- `tune.Cache.BumpPeak` does not persist; `vram-profile.json` and `corrallm
  introspect` lag the daemon by one spawn. Harmless here, surprising elsewhere.
