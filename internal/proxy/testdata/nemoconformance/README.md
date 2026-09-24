# NeMo intake conformance — the differential half

`internal/proxy/audio_test.go` carries `nemoAccepts`, a Go **transcription** of
NeMo-Speech.cpp's `parse_wav`, rule by rule with a source line for each. A
transcription checked only against its author's reading of the same source
proves nothing: it can confirm only what the author already believed.

`harness.cpp` closes that loop. It links NVIDIA/NeMo-Speech.cpp's **own**
`src/common/audio_file.cpp` and reports, per file, what the real parser did.
`internal/proxy/audio_nemodiff_test.go` drives both over the same bytes and
compares verdicts *and* which rule refused.

## What this proves, and what it does not

- It tests the **intake**. `server/http/http_server.cpp` calls
  `audio::load_wav_memory` on the uploaded multipart body directly — there is no
  decoder in front of it — so `parse_wav` is the door a corrallm upload arrives
  at.
- **No NeMo model has ever run here.** This links one translation unit. It loads
  no model, runs no recognition, and pulls in no ggml and no CUDA.
- `audio_file.cpp` includes only its own header and the standard library, so it
  compiles standalone and does **not** need the unmerged PR #50.

## Getting the source

Not vendored, and not a build dependency of corrallm. The tests **skip** when it
is absent, so a clean checkout still passes.

```
git clone https://github.com/NVIDIA/NeMo-Speech.cpp /tmp/nemo-scratch/NeMo-Speech.cpp
```

The rules in `audio_test.go` were read at commit
`302ebc93f096d03395e5d86c643897b8bf0fd9a8` (2026-09-21). The differential test
reports any refusal whose message no rule covers, which is how a contract change
upstream surfaces instead of passing quietly.

## Running it

`/tmp/nemo-scratch/NeMo-Speech.cpp` is the default location. Override it with
`NEMO_SPEECH_SRC`; override the compiler with `CXX` (default `g++`, needs C++20).

```
go test ./internal/proxy/ -run TestNeMoIntakeSpecMatchesRealParser -v
```

To run the harness by hand:

```
g++ -std=c++20 -O1 \
    -I$NEMO/src/common -I$NEMO/src \
    internal/proxy/testdata/nemoconformance/harness.cpp \
    $NEMO/src/common/audio_file.cpp -o /tmp/nemo_wav
/tmp/nemo_wav some.wav another.wav
```

Output is one tab-separated line per input:

```
<path>	ACCEPT	rate=<hz>	channels=<n>	frames=<n>
<path>	REJECT	<NeMo's own message>
```

## Coverage

Two tests, both comparing verdict and rule:

- **41 named vectors** — the thirteen `audio_test.go` already built, plus the
  cases the spec states a rule for and nothing had exercised:
  `WAVE_FORMAT_EXTENSIBLE` with a real 40-byte fmt chunk, `fmt ` chunks of 15,
  18 and 40 bytes, odd chunk padding present and missing, a truncated `data`
  chunk, `data` declaring `0xFFFFFFFF`, chunks before `fmt `, duplicate `fmt `
  and `data` chunks, and each of R6/R7's bounds hit exactly and missed by one.
- **5000 mutated headers** — one to three random bytes flipped in the 44-byte
  header, sometimes truncated. Hand-written vectors cover only the cases someone
  thought of; this covers the ones nobody did, which is where a transcription
  drifts.

The differential is itself checked for vacuity: changing `nemoAccepts`'s R7
lower bound from 8000 to 7000 makes it fail on the 7999 Hz vector.
