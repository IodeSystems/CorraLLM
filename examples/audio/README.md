# Audio examples — OpenAI media format, end to end

corrallm claims OpenAI-compatible audio surfaces. This directory makes that
claim runnable instead of asserted: `speech.wav` is a real sample, so every
command below can be pasted as-is.

`speech.wav` was produced by *this* stack (`/v1/audio/speech`, kokoro voice
`af_heart`) and transcribes back to its own source sentence through
`/v1/audio/transcriptions`. That round trip is the point — it exercises the
request shape in both directions with one artifact.

- 16-bit PCM WAV, mono, 24 kHz, ~123 KB
- content: "The quick brown fox jumps over the lazy dog."

## Speech to text — `POST /v1/audio/transcriptions`

Whisper-compatible `multipart/form-data`. The file field is `file`, the model
field is `model`, exactly as the OpenAI client sends them.

```sh
curl -sS http://localhost:8111/v1/audio/transcriptions \
  -H "Authorization: Bearer $CORRALLM_KEY" \
  -F model=stt \
  -F file=@examples/audio/speech.wav
# {"text":"The quick brown fox jumps over the lazy dog."}
```

### Diarized transcription

`stt-diarize` returns the same envelope plus speaker-labeled segments. A
single-speaker sample yields one speaker; the shape is what matters here.

```sh
curl -sS http://localhost:8111/v1/audio/transcriptions \
  -H "Authorization: Bearer $CORRALLM_KEY" \
  -F model=stt-diarize \
  -F file=@examples/audio/speech.wav
# {"duration":1.82,"language":"en","segments":[{"id":0,"start":0,"end":1.48,"speaker":"...","text":"..."}]}
```

## Text to speech — `POST /v1/audio/speech`

JSON in, binary audio out. `response_format` accepts `mp3` (default) or `wav`;
ask for `wav` if you intend to feed the result straight back into STT.

```sh
curl -sS http://localhost:8111/v1/audio/speech \
  -H "Authorization: Bearer $CORRALLM_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"tts","input":"Hello from corrallm","voice":"af_heart","response_format":"wav"}' \
  --output hello.wav
```

## The round trip

Regenerates the sample and reads it back — the fastest check that both audio
surfaces are alive after a config or backend change:

```sh
curl -sS http://localhost:8111/v1/audio/speech \
  -H "Authorization: Bearer $CORRALLM_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"tts","input":"The quick brown fox jumps over the lazy dog.","voice":"af_heart","response_format":"wav"}' \
  --output /tmp/rt.wav \
&& curl -sS http://localhost:8111/v1/audio/transcriptions \
  -H "Authorization: Bearer $CORRALLM_KEY" -F model=stt -F file=@/tmp/rt.wav
```

## Python — the OpenAI client, unmodified

The point of the compatibility claim: the official client works by changing only
`base_url`.

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8111/v1", api_key="<key>")

with open("examples/audio/speech.wav", "rb") as f:
    print(client.audio.transcriptions.create(model="stt", file=f).text)

client.audio.speech.create(model="tts", voice="af_heart",
                           input="Hello from corrallm").stream_to_file("hello.mp3")
```

## Realtime — `GET /v1/realtime` (WebSocket)

Live transcription on the OpenAI Realtime schema. Not curl-able; see the
`/v1/capabilities` manifest for the message flow, and
`examples/mic-transcribe/` for a working client.

## Note

These commands hit a live corrallm on `:8111`. While a bench/calibration lease
is held every caller except the bench receives `429` with `Retry-After` — that
is backpressure, not a fault. Wait it out or retry.
