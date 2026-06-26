# sherpa-diarize

A small HTTP service that produces a **speaker-labeled transcript** from an
uploaded recording — OpenAI-shaped `POST /v1/audio/transcriptions`, backed by
**sherpa-onnx** *offline* speaker diarization + an *offline* zipformer recognizer,
CPU-only.

Diarization is **offline by design**: stable speaker IDs need the whole utterance
(clustering can't label a speaker before it has heard enough of them), so this is
the **batch** path. corrallm proxies it as an `audio.stt` model with
`modes: [batch]`. The realtime ws path (live partials, no speakers) stays
[`sherpa-realtime-adapter`](../sherpa-realtime-adapter). Same engine, two honest
surfaces: realtime-without-speakers vs batch-with-speakers.

## Pipeline
1. **decode** — any container/codec → 16 kHz mono float32 via `ffmpeg` (stdin).
2. **diarize** — pyannote segmentation + speaker-embedding + FastClustering →
   `[(start, end, speaker)]`.
3. **transcribe** — offline zipformer → text with per-token timestamps.
4. **align** — assign each ASR token to the diarization segment its timestamp
   falls in (nearest if none), group consecutive same-speaker tokens.

## Response
```jsonc
{
  "text": "full transcript …",                 // OpenAI-compatible; plain clients read this
  "segments": [{"speaker": 0, "start": 0.0, "end": 3.3, "text": "…"}],
  "num_speakers": 2,
  "duration": 16.2
}
```

## Run (CPU)
```sh
uv venv --python 3.12 && uv pip install sherpa-onnx aiohttp numpy
# models (into ./models): pyannote segmentation + an English speaker embedding +
# an offline English zipformer (gigaspeech int8) — see download commands below.
PORT=5805 ./.venv/bin/python diarize.py
```
Invoke the venv python **directly** (`./.venv/bin/python`), not `uv run` — with no
`pyproject.toml`, `uv run` builds an ephemeral env *without* the installed deps, so
a corrallm-spawned `uv run` fails with `ModuleNotFoundError`. Needs `ffmpeg` on PATH.

Env: `PORT` (5805), `MODEL_DIR` (`./models`), `ASR_DIR`, `SEG_MODEL`, `EMB_MODEL`,
`NUM_THREADS` (4), `NUM_SPEAKERS` (-1 auto; set to a known count for far better
results), `CLUSTER_THRESHOLD` (0.7, auto-mode only; **lower → more speakers**).

## Models
```sh
mkdir -p models && cd models
# segmentation (7 MB)
curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-segmentation-models/sherpa-onnx-pyannote-segmentation-3-0.tar.bz2 | tar xj
# English speaker embedding (29 MB)
curl -fsSL "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/wespeaker_en_voxceleb_CAM%2B%2B.onnx" -o wespeaker_en_voxceleb_CAMpp.onnx
# offline English ASR with timestamps (gigaspeech int8)
curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-zipformer-gigaspeech-2023-12-12.tar.bz2 | tar xj
```

## Tuning note
Auto speaker-count detection is threshold-sensitive. On **real** conversational
audio it tracks the true count well — the 0.7 default gets the right count on all
three of sherpa's English 2-speaker reference clips (`{1,2,3}-two-speakers-en.wav`).
Concatenated-TTS test clips are a **poor** diarization test — hard cuts and
near-identical synthetic timbres make clustering unstable; validate with real
recordings, or pass `NUM_SPEAKERS` when the count is known.
