---
name: capability-stt
class: capability
requires: { capability: audio.stt, modality: audio }
workspace: fixture/
audio:
  transcribe: speech.wav
---

# STT: does the model actually transcribe?

Verifies a model's DECLARED `audio.stt` surface against the live backend.

Until this existed, nothing exercised audio at all. A UI-triggered bench put the
thirteen *chat* probes against `stt`, `tts`, `stt-diarize` and `realtime-stt` —
they scored 1/21 apiece, published results that meant nothing, and the audio
models still read "audio unverified" afterwards. A text-shaped probe cannot say
anything about a multipart-upload surface.

`fixture/speech.wav` is 16-bit PCM, mono 24 kHz, produced by this stack's own
TTS. Using our own output is deliberate: it keeps the fixture reproducible from
the box under test, and it is the same artifact `examples/audio/` documents.

The checks are loose on purpose — Whisper normalises punctuation and casing, and
"the quick brown fox" arriving as "The quick brown fox." is a transcription, not
a failure. Asserting on individual words answers "did audio reach the model",
which is what a capability probe is for; transcription *quality* is a different
measurement.

## Prompt

(unused — an audio probe drives the endpoint directly, not a chat turn)

## Checks

- response_contains: quick
- response_contains: brown
- response_contains: fox
- python: |
    words = response.lower().replace(".", "").split()
    if len(words) < 5:
        fail("transcript too short to be the fixture: %r" % response)
