---
name: capability-vision
class: capability
requires: { modality: image }
run: warm
limits: { maxTurnsPerStage: 2, maxToolCallsPerStage: 0 }
---

# Vision: does the model actually see the image?

Verifies a model's DECLARED `image` modality against the live backend. A model
that declares `modalities.image` in corrallm.yaml but silently drops the
attachment passes every other form of inspection: `/props` still reports
`vision: true`, the mmproj still loads, and the model answers the question
fluently — from the text alone.

That is not hypothetical. On 2026-07-18 `ternary-bonsai-27b` did exactly this on
the FIRST request after a cold load, saying "there is no actual image attached"
in its reasoning while `/props` reported vision support. Warm, it answered
correctly. The config had claimed the modality was "verified end-to-end" because
the one manual check anyone ran happened to hit a warm model.

This probe used to declare `run: both` — once cold, once warm, with a
DISAGREEMENT between the passes as the finding. Cold mode is gone: arranging it
meant evicting a model, which is a cost every other caller on the box pays, and
it went with the exclusive lease.

So read this probe for what it now is: a check that the pixels arrive on a WARM
model. By its own history that proves very little about the cold path — the
2026-07-18 failure passed warm and failed cold. The gap is recorded in
probes/README.md rather than papered over. `warm` needs an admin token
(`llm.adminTokenFile` / `llm.adminTokenEnv`); without one the pass is recorded
with a loud warning rather than quietly passing.

The image is a solid red circle, centred on white. Deliberately trivial: the
probe asks whether the pixels arrived, not whether the model is a good
describer. Quality belongs to the judge, not to a capability check.

## Prompt

What shape and what colour is in this image? Answer in a few words.

![a solid red circle on a white background](_fixture/red-circle.png)

## Checks

Single words, not phrases — models write `a **red** circle`, and a substring
match for `red circle` fails on the emphasis markers between the words.

- response_contains: red
- response_contains: circle
