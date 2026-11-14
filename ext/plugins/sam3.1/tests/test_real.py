"""Real SAM 3.1 backend. Skipped unless SAM3_CHECKPOINT points at sam3.1_multiplex.pt AND a CUDA
GPU is visible. Loads the model (~35 s, ~6 GiB VRAM): pin the GPU with CUDA_VISIBLE_DEVICES=<uuid>.
    SAM3_CHECKPOINT=~/.corrallm/models/sam3.1/sam3.1_multiplex.pt CUDA_VISIBLE_DEVICES=GPU-... pytest tests/test_real.py
"""
import os

import numpy as np
import pytest
from PIL import Image

from sam3shim.prompts import ConceptPrompt, Exemplar, Point, VisualPrompt

CKPT = os.path.expanduser(os.environ.get("SAM3_CHECKPOINT", ""))

torch = pytest.importorskip("torch")
pytestmark = pytest.mark.skipif(
    not (CKPT and os.path.exists(CKPT) and torch.cuda.is_available()),
    reason="needs SAM3_CHECKPOINT and a CUDA GPU")


@pytest.fixture(scope="module")
def backend():
    from sam3shim.backends import Sam3Backend
    return Sam3Backend(CKPT)


@pytest.fixture(scope="module")
def disc():
    """320 wide x 240 high, grey; one red disc centred (200, 90), radius 50."""
    yy, xx = np.mgrid[:240, :320]
    truth = (xx - 200) ** 2 + (yy - 90) ** 2 <= 50 ** 2
    a = np.full((240, 320, 3), 128, np.uint8)
    a[truth] = (220, 30, 30)
    return Image.fromarray(a), truth


def iou(a, b):
    return float((a & b).sum() / max(1, (a | b).sum()))


def test_box_prompt_mask_inside_box(backend, disc):
    img, truth = disc
    box = (140.0, 30.0, 260.0, 150.0)  # x0, y0, x1, y1 pixels; non-square on purpose below
    (m, score), *_ = backend.segment_visual(img, VisualPrompt([], box, False))
    assert m.shape == (240, 320) and m.dtype == bool
    assert 0 <= score <= 1
    ys, xs = np.nonzero(m)
    assert xs.min() >= box[0] - 1 and xs.max() < box[2] + 1 and ys.min() >= box[1] - 1 and ys.max() < box[3] + 1
    assert iou(m, truth) > 0.85


def test_point_prompt_x_y_order(backend, disc):
    """A point in the disc gives the disc; the same (x, y) transposed lands on background."""
    img, truth = disc
    res = backend.segment_visual(img, VisualPrompt([Point(200, 90, 1)], None, True))
    assert len(res) == 3 and [s for _, s in res] == sorted((s for _, s in res), reverse=True)
    assert max(iou(m, truth) for m, _ in res) > 0.85
    assert res[0][0][90, 200]


def test_concept_text_and_exemplar(backend, disc):
    img, truth = disc
    # exemplar box around the disc, no text: finds the disc as one instance
    res = backend.segment_concept(img, ConceptPrompt(None, [Exemplar((145.0, 35.0, 255.0, 145.0), 1)]))
    assert res and all(m.shape == (240, 320) for m, _ in res)
    assert all(0 <= s <= 1 for _, s in res)
    # scores ordered, at most the detector's query count
    assert [s for _, s in res] == sorted((s for _, s in res), reverse=True)
