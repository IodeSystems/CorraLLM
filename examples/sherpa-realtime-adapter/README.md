# sherpa-realtime-adapter

A tiny WebSocket adapter that exposes the **OpenAI Realtime *transcription* schema**
(`/v1/realtime`) backed by **sherpa-onnx** streaming zipformer — a true streaming
transducer with live partial deltas and built-in endpointing, CPU-only.

corrallm passes its `/v1/realtime` websocket straight through to this, so the
adapter looks like a native OpenAI-Realtime backend (no corrallm code involved).
Speaches' realtime *transcription* mode is broken (it fires response-generation
and 500s); parakeet is batch-only. This is corrallm's realtime/VAD path.

## Protocol
- Client → `session.update`, then `input_audio_buffer.append` frames (`audio`:
  base64 **PCM16 mono @ 24 kHz**), and `input_audio_buffer.commit` to flush the
  final utterance before closing.
- Adapter → `session.created` / `session.updated`,
  `conversation.item.input_audio_transcription.delta` (incremental live partial),
  `conversation.item.input_audio_transcription.completed` (final per utterance,
  emitted on sherpa's silence-based endpoint or on commit).
- `GET /health` → 200.

## Run (CPU)
```sh
uv venv && uv pip install sherpa-onnx numpy websockets
# download a streaming model, e.g.:
#   sherpa-onnx-streaming-zipformer-en-2023-06-26  (k2-fsa releases tag asr-models)
PORT=5804 MODEL_DIR=./models/sherpa-onnx-streaming-zipformer-en-2023-06-26 \
  ./.venv/bin/python adapter.py
```
Invoke the venv python **directly** (`./.venv/bin/python`), not `uv run` — with no
`pyproject.toml`, `uv run` builds an ephemeral env *without* the installed deps, so
a process-manager/corrallm-spawned `uv run` fails with `ModuleNotFoundError: numpy`.
Env: `PORT` (5804), `MODEL_DIR`, `SAMPLE_RATE` (client pcm rate, default 24000),
`RULE2_SILENCE` (end-of-utterance trailing silence secs, default 0.8 — lower = snappier finals).

Model files are auto-globbed (encoder/decoder/joiner `*.onnx` + `tokens.txt`), so
any streaming-zipformer model dir works. Diarization is NOT included (sherpa-onnx
diarization is offline-only; realtime diarization is a separate effort).
