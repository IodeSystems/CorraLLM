---
name: capability-diarize
class: capability
requires: { capability: audio.stt, modality: audio }
workspace: fixture/
audio:
  transcribe: two-speakers.wav
---

# Diarization: does it actually separate speakers?

Segments are the ONLY thing that distinguishes `stt-diarize` from `stt`. A
diarizer that had stopped diarizing — collapsing everything into one speaker —
still returns a correct joined transcript, so `capability-stt` passes it. This
probe asserts on the structure instead.

The fixture is two sentences in two kokoro voices (`af_heart`, then `am_adam`)
with a beat of silence between them, concatenated. Built from this stack's own
TTS so it is reproducible on the box under test, and deliberately two DIFFERENT
sentences so a segment can be attributed to a speaker by its content rather than
by trusting the labels.

A single-speaker fixture cannot test this at all: one speaker is the answer a
broken diarizer gives for everything.

Speaker ids are per-run UUIDs, so the checks assert they are DISTINCT and never
what they equal.

## Prompt

(unused — an audio probe drives the endpoint directly)

## Checks

- python: |
    if len(segments) < 2:
        fail("expected 2 speaker segments, got %d — a diarizer that merges speakers still returns a correct transcript" % len(segments))

    speakers = {}
    for s in segments:
        speakers[s["speaker"]] = True
    if len(speakers) < 2:
        fail("all %d segments share one speaker id — speakers were not separated" % len(segments))

- python: |
    # Attribute by CONTENT, not by label: the fox sentence and the liquor
    # sentence were spoken by different voices, so they must land in different
    # segments.
    fox = [s for s in segments if "fox" in s["text"].lower()]
    jugs = [s for s in segments if "jugs" in s["text"].lower()]
    if not fox: fail("first sentence missing from segments")
    if not jugs: fail("second sentence missing from segments")
    if fox[0]["speaker"] == jugs[0]["speaker"]:
        fail("both sentences attributed to the same speaker")

- python: |
    # Timings must be ordered and non-overlapping, or the spans are not usable
    # for anything downstream even when the labels happen to be right.
    prev_end = -1.0
    for s in segments:
        if s["end"] <= s["start"]:
            fail("segment %d ends before it starts (%s -> %s)" % (s["id"], s["start"], s["end"]))
        if s["start"] < prev_end:
            fail("segment %d overlaps the previous one" % s["id"])
        prev_end = s["end"]
