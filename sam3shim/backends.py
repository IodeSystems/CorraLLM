"""Model backends. A backend returns raw candidates; the server applies threshold, max_masks,
ordering, area, box and RLE.

Candidate = (mask: bool ndarray [H, W], score: float).
"""
from typing import Protocol

import numpy as np
from PIL import Image

from .prompts import ConceptPrompt, VisualPrompt, box_xyxy_to_cxcywh_norm, visual_to_sam1


class Backend(Protocol):
    name: str

    def segment_concept(self, image: Image.Image, p: ConceptPrompt) -> list[tuple[np.ndarray, float]]: ...

    def segment_visual(self, image: Image.Image, p: VisualPrompt) -> list[tuple[np.ndarray, float]]: ...


def _color_match(arr: np.ndarray, color, tol: int) -> np.ndarray:
    return (np.abs(arr.astype(np.int16) - np.asarray(color, dtype=np.int16)).max(axis=2) <= tol)


class StubBackend:
    """Deterministic, image-aware, no model. Segments by colour:

    visual: mask = pixels within tolerance of the colour under the first include point (or the
      centre of the box), restricted to the box if given; exclude points drop their own colour.
      multimask -> tolerances 0, 40, 120 with scores 0.9, 0.8, 0.7.
    concept: exemplar boxes -> one instance per include box, mask = pixels matching the box
      centre colour inside the box (score 0.95); exclude boxes remove their area.
      text only -> one instance per distinct non-background colour (background = most common
      colour), scores 0.9, 0.8, ... by descending area.
    """
    name = "stub"

    def segment_visual(self, image, p):
        arr = np.asarray(image)
        h, w = arr.shape[:2]
        inc = [pt for pt in p.points if pt.label == 1]
        exc = [pt for pt in p.points if pt.label == 0]
        if inc:
            cx, cy = inc[0].x, inc[0].y
        else:
            x0, y0, x1, y1 = p.box
            cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
        ref = arr[min(int(cy), h - 1), min(int(cx), w - 1)]
        out = []
        for tol, score in ((0, 0.9), (40, 0.8), (120, 0.7)) if p.multimask else ((0, 0.9),):
            m = _color_match(arr, ref, tol)
            if p.box:
                x0, y0, x1, y1 = p.box
                box_m = np.zeros_like(m)
                box_m[int(np.floor(y0)):int(np.ceil(y1)), int(np.floor(x0)):int(np.ceil(x1))] = True
                m &= box_m
            for pt in exc:
                m &= ~_color_match(arr, arr[min(int(pt.y), h - 1), min(int(pt.x), w - 1)], tol)
            out.append((m, score))
        return out

    def segment_concept(self, image, p):
        arr = np.asarray(image)
        h, w = arr.shape[:2]
        out = []
        pos = [e for e in p.boxes if e.label == 1]
        neg = [e for e in p.boxes if e.label == 0]
        if pos:
            for e in pos:
                x0, y0, x1, y1 = e.box
                ref = arr[min(int((y0 + y1) / 2), h - 1), min(int((x0 + x1) / 2), w - 1)]
                m = np.zeros((h, w), dtype=bool)
                ys, xs = slice(int(np.floor(y0)), int(np.ceil(y1))), slice(int(np.floor(x0)), int(np.ceil(x1)))
                m[ys, xs] = _color_match(arr[ys, xs], ref, 0)
                out.append((m, 0.95))
        elif p.text:
            colors, counts = np.unique(arr.reshape(-1, 3), axis=0, return_counts=True)
            order = np.argsort(-counts)
            for rank, idx in enumerate(order[1:]):
                out.append((_color_match(arr, colors[idx], 0), max(0.05, 0.9 - 0.1 * rank)))
        for e in neg:
            x0, y0, x1, y1 = e.box
            for m, _ in out:
                m[int(np.floor(y0)):int(np.ceil(y1)), int(np.floor(x0)):int(np.ceil(x1))] = False
        return out


class Sam3Backend:
    """Real SAM 3.1 via the official facebookresearch/sam3 repo.

    UNTESTED: written against the repo README / source as read, never executed (weights are
    gated; licence pending). Entry points to verify first on gpu1:
      sam3.model_builder.build_sam3_image_model(checkpoint_path=..., load_from_HF=False,
          enable_inst_interactive_predictor=True)
      sam3.model.sam3_image_processor.Sam3Processor: set_image, set_text_prompt,
          add_geometric_prompt(box=[cx, cy, w, h] normalized, label, state), reset_all_prompts
      model.inst_interactive_predictor.set_image / .predict(point_coords, point_labels, box,
          multimask_output) with pixel coords (normalize_coords=True).
    SAM 3.1 may use a different builder or checkpoint key than SAM 3; check the repo's 3.1 notes.
    """
    name = "sam3"

    def __init__(self, checkpoint: str, device: str = "cuda", confidence_threshold: float = 0.0):
        import torch  # noqa: F401  (heavy imports stay out of the stub path)
        from sam3.model.sam3_image_processor import Sam3Processor
        from sam3.model_builder import build_sam3_image_model

        self._torch = torch
        self.model = build_sam3_image_model(
            checkpoint_path=checkpoint, load_from_HF=False, device=device,
            enable_inst_interactive_predictor=True,
        )
        # Low processor threshold: the server applies the request threshold itself.
        self.processor = Sam3Processor(self.model, confidence_threshold=confidence_threshold)

    def segment_concept(self, image, p):
        h, w = image.size[1], image.size[0]
        with self._torch.inference_mode():
            state = self.processor.set_image(image)
            if p.text:
                state = self.processor.set_text_prompt(state=state, prompt=p.text)
            for e in p.boxes:
                state = self.processor.add_geometric_prompt(
                    box=box_xyxy_to_cxcywh_norm(e.box, w, h), label=bool(e.label), state=state)
        masks = state["masks"].squeeze(1).cpu().numpy().astype(bool)  # [N, H, W]
        scores = state["scores"].float().cpu().numpy()
        self.processor.reset_all_prompts(state)
        return [(m, float(s)) for m, s in zip(masks, scores)]

    def segment_visual(self, image, p):
        predictor = self.model.inst_interactive_predictor
        coords, labels, box = visual_to_sam1(p)
        import numpy as _np
        with self._torch.inference_mode():
            state = self.processor.set_image(image)
            masks, ious, _ = predictor.predict(
                point_coords=_np.asarray(coords, dtype=_np.float32) if coords else None,
                point_labels=_np.asarray(labels, dtype=_np.int32) if coords else None,
                box=_np.asarray(box, dtype=_np.float32) if box else None,
                multimask_output=p.multimask,
            )
        return [(m.astype(bool), float(s)) for m, s in zip(masks, ious)]
