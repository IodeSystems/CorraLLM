#!/usr/bin/env bash
# Add the real backend to a venv (run scripts/venv.sh first). No sudo; ~8 GB of wheels.
# torch 2.10.0 + cu128 wheels (driver CUDA 13 runs them); sam3 pinned to the commit measured in the README.
# Dev default: ./.venv, sam3 editable from ./.sam3-src (git-ignored).
# Plugin install: VENV=<dir> SAM3_INSTALL=wheel (sam3 built from a temp clone, non-editable,
# so the venv holds no path into a temp dir).
set -euo pipefail
cd "$(dirname "$0")/.."
VENV="${VENV:-.venv}"
SAM3_COMMIT=eef6839bcfc3458dd52ea80535dc6bd62aea86b1
PY="$VENV/bin/python"
uv pip install --python "$PY" torch==2.10.0 torchvision --index-url https://download.pytorch.org/whl/cu128
if [ "${SAM3_INSTALL:-editable}" = wheel ]; then
  src="$(mktemp -d)"; trap 'rm -rf "$src"' EXIT
  git clone -q https://github.com/facebookresearch/sam3 "$src/sam3"
  git -C "$src/sam3" checkout -q "$SAM3_COMMIT"
  uv pip install --python "$PY" "$src/sam3" einops psutil huggingface_hub pycocotools
else
  [ -d .sam3-src ] || git clone https://github.com/facebookresearch/sam3 .sam3-src
  git -C .sam3-src checkout "$SAM3_COMMIT"
  uv pip install --python "$PY" -e ./.sam3-src einops psutil huggingface_hub pycocotools
fi
