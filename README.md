# sam3shim

HTTP shim in front of SAM 3.1 for corrallm (spec: corrallm `plan/p78-sam-colmap.md`, section 4.1).
Serves `POST /v1/images/segmentations` (same body and response as the spec, so corrallm proxies it
unchanged) and `GET /health`. One inference at a time (lock); body cap 32 MiB.

Coordinates: pixels of the EXIF-upright image, top-left origin, continuous. Box `[x0,y0,x1,y1]`
edges (x1, y1 exclusive). Point label 1 include / 0 exclude. Out of range -> 400 with `param`
(never clamped). Masks: COCO RLE, `size` = `[height, width]`, pycocotools-compatible counts string.
Point `label` is required; box-exemplar `label` defaults to 1. Errors use the OpenAI envelope.

## Setup (no sudo; everything in ./.venv)

    scripts/venv.sh                       # uv venv (Python >= 3.12) + test deps
    CUDA_VISIBLE_DEVICES= .venv/bin/python -m pytest

Real backend (gated weights `facebook/sam3.1`, file `sam3.1_multiplex.pt`, 3.5 GB, FP32):

    scripts/venv-sam3.sh     # torch 2.10.0+cu128, torchvision 0.25.0+cu128, triton 3.6.0, timm 1.0.31,
                             # facebookresearch/sam3 @ eef6839 (editable, ./.sam3-src, git-ignored)
    SAM3_CHECKPOINT=/path/sam3.1_multiplex.pt CUDA_VISIBLE_DEVICES=GPU-<uuid> .venv/bin/python -m pytest tests/test_real.py

## Run

    .venv/bin/python -m sam3shim --stub --port 5821                       # no model, no GPU
    .venv/bin/python -m sam3shim --port 5821 --checkpoint /path/sam3.1_multiplex.pt [--dtype bf16|fp16|fp32]   # real; loads before binding

`--stub` is deterministic and colour-based (see `backends.StubBackend`); it exists for tests and
for corrallm integration tests.

## Real backend: how it works and what was measured (RTX 3080 10 GB, 2026-11-14)

`Sam3Backend` does not use `build_sam3_image_model`: the 3.1 checkpoint is a merged multiplex video
checkpoint (`detector.*` + `tracker.*`, tri-head neck) that the image builder cannot load. It builds
the multiplex model with the official builder, then runs one image through the parts: text and box
exemplars via `detector.forward_grounding` (score = sigmoid(logit) x sigmoid(presence)); points and
one box via the tracker's interactive prompt encoder + mask decoder (box = two corner points, labels
2 and 3; mask score = sigmoid(IoU head)). Verified against sam3's own video predictor on one image:
same instances and scores within 0.02 (the reference does its resize in fp16).
Coordinates verified: a box prompt gives a mask 100% inside the box (0 px outside), x/y order correct.

| measurement | value |
|---|---|
| load to ready (python start, ckpt read, model build, to GPU) | 21 s (page cache warm), 33-54 s otherwise; peak system RSS 7.7 GiB while loading, 4.2 GiB after |
| VRAM after load (process, nvidia-smi) | 4.1 GiB (3.7 GiB tensors) |
| VRAM peak per request (process, nvidia-smi), bf16 | 5.8 GiB at 1024 px, 6.1 GiB at 4000 px (same for text, point, box) |
| VRAM peak, fp32 | 4.9 GiB (bf16 autocast keeps cast weight copies; fp32 does not) |
| first request after load | 0.7-1.2 s |
| latency median of 5, bf16, 1024x683 | text 267 ms, point 210 ms, box 210 ms |
| latency median of 5, bf16, 4000x2667 | text 296 ms, point 245 ms, box 241 ms (input is resized to 1008x1008 on the GPU; image size only adds decode and mask upsampling) |
| latency fp16 (1024 / 4000 px) | text 273 / 316, point 213 / 257, box 211 / 273 ms; VRAM same as bf16 |
| latency fp32 (1024 / 4000 px) | text 898 / 975, point 811 / 858, box 824 / 860 ms (needs a patch, see below) |
| bf16 vs fp32 masks | min IoU 0.998 on 11 masks; scores differ up to 0.13 near threshold |

Correctness (overlays in the P78 scratchpad `sam3/`): "child" on a 6-children photo finds 6 masks,
scores 0.88-0.92; "paper bag" finds the 4 bags (0.82-0.83); "truck" finds the truck (0.73, 29% of
the frame). Box around an object and a point inside it reproduce the text mask at IoU 0.97-0.99.

Behaviour to know:
- The bare noun `person` scores 0.06 on that photo (0 instances above threshold 0.5), same in
  sam3's own pipeline; `child` scores 0.92, `a person` 0.38-0.60 (borderline, dtype dependent).
  Text prompts are model-sensitive: callers should test phrasings and may lower `threshold`.
- Candidates below `min_score` (0.05) are dropped before mask upsampling (about 200 queries, one
  full-size mask each); a request `threshold` below 0.05 cannot return them.
- fp32 replaces sam3's `perflib.fused.addmm_act` (hardcodes bf16 for the ViT MLP) and disables TF32.
- `use_fa3=False` (FlashAttention 3 is a Hopper extra).
- Not done: HTTP end-to-end with the real backend (the server path is covered by the stub tests),
  concurrent requests, long-run VRAM growth, `torch.compile`.

## corrallm extension config

```yaml
extensions:
  sam3:
    cmd: |
      exec systemd-run --user --scope --quiet --collect \
        --property=MemoryMax=12G --property=MemorySwapMax=0 \
        --setenv=CUDA_VISIBLE_DEVICES=GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544 \
        /home/nthalk/.corrallm/ext/sam3/venv/bin/python -m sam3shim --port 5821 \
          --checkpoint /home/nthalk/.corrallm/models/sam3.1/sam3.1_multiplex.pt
    server: box1
    ramUsage: { gpu1: 6800MiB, system: 8GB }   # measured: VRAM peak 6.1 GiB (+10%); RSS peak 7.7 GiB at load, 4.2 GiB steady
    swap: { loadSeconds: 45, loadWatts: 200 }   # load 21-54 s measured; watts not measured
    proxy: 5821
    sticky: { ttl: 2m, idleUnload: 10m }
    provides:
      segment: { type: segment, pool: gpu1, maxConcurrent: 1, contextPerRequest: 0 }
```

(`sam3shim` must be installed in that venv: `uv pip install --python <venv>/bin/python -e <repo>`.)

## Layout

`server.py` HTTP · `service.py` request -> response · `prompts.py` validation and pixel -> SAM
conversion · `rle.py` COCO RLE · `orient.py` decode + EXIF · `backends.py` stub and SAM 3.1.
