---
name: capability-vision
class: capability
requires: { modality: image }
run: both
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

So this probe declares `run: both`: it runs once cold and once warm, and a
DISAGREEMENT between the two passes is the finding. A warm-only run of it proves
very little. Cold requires an admin token (`llm.adminTokenFile` /
`llm.adminTokenEnv`); without one the pass is recorded with a loud warning that
it does not prove the cold path, rather than quietly passing.

The image is a solid red circle, centred on white. Deliberately trivial: the
probe asks whether the pixels arrived, not whether the model is a good
describer. Quality belongs to the judge, not to a capability check.

## Prompt

What shape and what colour is in this image? Answer in a few words.

![a solid red circle on a white background](fixture/red-circle.png)

## Checks

Single words, not phrases — models write `a **red** circle`, and a substring
match for `red circle` fails on the emphasis markers between the words.

- response_contains: red
- response_contains: circle
