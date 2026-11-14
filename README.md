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

Real backend, after the gated weights are available (not done yet):

    uv pip install --python .venv/bin/python torch torchvision --index-url https://download.pytorch.org/whl/cu126
    git clone https://github.com/facebookresearch/sam3 && uv pip install --python .venv/bin/python -e ./sam3
    # (check the sam3 repo README for its current install and for SAM 3.1 specifics)

## Run

    .venv/bin/python -m sam3shim --stub --port 5821                       # no model, no GPU
    .venv/bin/python -m sam3shim --port 5821 --checkpoint /path/sam3.pt   # real; loads before binding

`--stub` is deterministic and colour-based (see `backends.StubBackend`); it exists for tests and
for corrallm integration tests. The `Sam3Backend` is **untested**: written from the official repo's
API, never run (weights gated, licence pending).

## corrallm extension config

```yaml
extensions:
  sam3:
    cmd: |
      exec systemd-run --user --scope --quiet --collect \
        --property=MemoryMax=8G --property=MemorySwapMax=0 \
        --setenv=CUDA_VISIBLE_DEVICES=GPU-76a4c775-a47f-61b9-3a9f-9c7d5edfc544 \
        /home/nthalk/.corrallm/ext/sam3/venv/bin/python -m sam3shim --port 5821 \
          --checkpoint /home/nthalk/.corrallm/models/sam3.1/sam3.pt
    server: box1
    ramUsage: { gpu1: 4600MiB, system: 4GB }
    swap: { loadSeconds: 25, loadWatts: 200 }
    proxy: 5821
    sticky: { ttl: 2m, idleUnload: 10m }
    provides:
      segment: { type: segment, pool: gpu1, maxConcurrent: 1, contextPerRequest: 0 }
```

(`sam3shim` must be installed in that venv: `uv pip install --python <venv>/bin/python -e <repo>`.)

## Layout

`server.py` HTTP · `service.py` request -> response · `prompts.py` validation and pixel -> SAM
conversion · `rle.py` COCO RLE · `orient.py` decode + EXIF · `backends.py` stub and SAM 3.1.
