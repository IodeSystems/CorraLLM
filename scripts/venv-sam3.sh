#!/usr/bin/env bash
# Add the real backend to ./.venv (run scripts/venv.sh first). No sudo; ~8 GB of wheels.
# torch 2.10.0 + cu128 wheels (driver CUDA 13 runs them); sam3 pinned to the commit measured in the README.
set -euo pipefail
cd "$(dirname "$0")/.."
SAM3_COMMIT=eef6839bcfc3458dd52ea80535dc6bd62aea86b1
uv pip install --python .venv/bin/python torch==2.10.0 torchvision --index-url https://download.pytorch.org/whl/cu128
[ -d .sam3-src ] || git clone https://github.com/facebookresearch/sam3 .sam3-src
git -C .sam3-src checkout "$SAM3_COMMIT"
uv pip install --python .venv/bin/python -e ./.sam3-src einops psutil huggingface_hub
