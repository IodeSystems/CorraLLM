---
name: capability-tts
class: capability
requires: { capability: audio.tts, modality: audio }
workspace: fixture/
audio:
  speak: "The quick brown fox jumps over the lazy dog."
  voice: af_heart
  format: wav
  thenTranscribe: stt
---

# TTS: does the model synthesize intelligible speech?

Verifies a model's DECLARED `audio.tts` surface, and does it by ROUND TRIP: the
synthesized audio is fed straight back through STT and the transcript compared
against the sentence that produced it.

The round trip is the point. Asserting only that bytes came back is satisfied by
a well-formed blob of silence — a TTS backend that has lost its voice model
still returns a valid WAV header. Reading the audio back is the only assertion
here that can tell "it spoke" from "it emitted a file".

That does mean this probe fails if EITHER direction is broken, so read a failure
alongside capability-stt: stt passing and tts failing isolates the synthesizer.

`format: wav` matters — the output is fed to STT, and requesting a container the
transcriber cannot read would fail for a reason unrelated to speech.

## Prompt

(unused — an audio probe drives the endpoint directly, not a chat turn)

## Checks

- python: |
    # A valid header with no speech in it is the failure this probe exists to catch.
    if audio_bytes < 20000:
        fail("synthesized only %d bytes — too small to be a spoken sentence" % audio_bytes)
    if audio_format != "wav":
        fail("asked for wav, backend returned %s (an STT round trip needs the requested container)" % audio_format)
- response_contains: quick
- response_contains: fox
