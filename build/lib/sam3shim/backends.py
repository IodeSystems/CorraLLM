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
    """Real SAM 3.1 (Object Multiplex checkpoint `sam3.1_multiplex.pt`) via facebookresearch/sam3.

    Verified against sam3 commit eef6839 on an RTX 3080 (see README for measurements).
    The 3.1 checkpoint is a merged video checkpoint (`detector.*` + `tracker.*`, tri-head vision
    neck). `build_sam3_image_model` does NOT load it (that is the SAM 3 layout). The one-image
    path is assembled here from the multiplex builder's parts:
      * concept (text / box exemplars): `detector.forward_grounding` on the "sam3" neck output;
        same maths as `Sam3Processor` (score = sigmoid(logit) * sigmoid(presence)).
      * visual (points / one box): the tracker's "interactive" neck + `interactive_sam_*`
        prompt encoder and mask decoder, same maths as `SAM3InteractiveImagePredictor`
        (box = two corner points with labels 2 and 3).
    The video predictor is not used: its add_prompt applies video heuristics (tracking, hotstart,
    score_threshold_detection=0.4) that do not belong in a one-image API.

    Candidates below `min_score` are dropped before mask upsampling: the detector has ~200 queries
    and each full-resolution mask costs H*W bytes. The request `threshold` is applied by the
    caller, so a request threshold under `min_score` cannot return the lower candidates.
    """
    name = "sam3"
    RES = 1008

    def __init__(self, checkpoint: str, device: str = "cuda", dtype: str = "bf16", min_score: float = 0.05):
        import torch
        from sam3.model_builder import build_sam3_multiplex_video_predictor

        if dtype not in ("bf16", "fp16", "fp32"):
            raise ValueError("dtype must be bf16, fp16 or fp32")
        if dtype == "fp32":
            # sam3's ViT MLP (perflib.fused.addmm_act) hardcodes bf16 for fc1 -> dtype clash under
            # fp32. Replace with the plain op, and use full-precision matmuls (the predictor
            # builder otherwise enables TF32).
            import sam3.model.vitdet as _vitdet

            def _plain(act, lin, x):
                return (act() if isinstance(act, type) else act)(lin(x))
            _vitdet.addmm_act = _plain
        self._torch = torch
        self.device = device
        self.dtype = dtype
        self.min_score = min_score
        # use_fa3=False: flash-attention-3 is an optional extra and targets Hopper.
        pred = build_sam3_multiplex_video_predictor(
            checkpoint_path=checkpoint, use_fa3=False, use_rope_real=True)
        pred.bf16_context.__exit__(None, None, None)  # the builder enters a global bf16 autocast
        if dtype == "fp32":
            torch.backends.cuda.matmul.allow_tf32 = False
            torch.backends.cudnn.allow_tf32 = False
        self.detector = pred.model.detector
        self.tracker = pred.model.tracker.model
        del pred
        self.detector.eval()
        self.tracker.eval()
        self._fs = None

    # -- helpers ------------------------------------------------------------------------------

    def _autocast(self):
        t = self._torch
        if self.dtype == "fp32":
            return t.autocast(device_type="cuda", enabled=False)
        return t.autocast(device_type="cuda", dtype=t.bfloat16 if self.dtype == "bf16" else t.float16)

    def _image_tensor(self, image):
        t = self._torch
        arr = np.array(image.convert("RGB"))  # copy: from_numpy needs a writable array
        x = t.from_numpy(arr).to(self.device).permute(2, 0, 1)[None].float() / 255.0
        x = t.nn.functional.interpolate(x, size=(self.RES, self.RES), mode="bilinear", align_corners=False)
        return (x - 0.5) / 0.5

    def _find_stage(self):
        from sam3.model.data_misc import FindStage
        t = self._torch
        return FindStage(
            img_ids=t.tensor([0], device=self.device, dtype=t.long),
            text_ids=t.tensor([0], device=self.device, dtype=t.long),
            input_boxes=None, input_boxes_mask=None, input_boxes_label=None,
            input_points=None, input_points_mask=None)

    def _upsample(self, logits, h, w):
        """[N, h0, w0] float logits -> [N, h, w] bool (sigmoid > 0.5 == logit > 0)."""
        t = self._torch
        out = []
        for i in range(0, logits.shape[0], 8):  # chunk: bounded peak for large images
            up = t.nn.functional.interpolate(logits[i:i + 8, None].float(), size=(h, w),
                                             mode="bilinear", align_corners=False)
            out.append((up[:, 0] > 0).cpu().numpy())
        return np.concatenate(out) if out else np.zeros((0, h, w), bool)

    # -- concept: text and/or box exemplars ----------------------------------------------------

    def segment_concept(self, image, p):
        from sam3.model import box_ops
        t = self._torch
        w, h = image.size
        det = self.detector
        with t.inference_mode(), self._autocast():
            bo = det.backbone.forward_image(
                self._image_tensor(image), need_sam3_out=True,
                need_interactive_out=False, need_propagation_out=False)
            # no text -> the processor's convention: text "visual", geometry decides
            bo.update(det.backbone.forward_text([p.text or "visual"], device=self.device))
            gp = det._get_dummy_prompt()
            for e in p.boxes:
                box = t.tensor(box_xyxy_to_cxcywh_norm(e.box, w, h), device=self.device, dtype=t.float32).view(1, 1, 4)
                gp.append_boxes(box, t.tensor([bool(e.label)], device=self.device, dtype=t.bool).view(1, 1))
            if self._fs is None:
                self._fs = self._find_stage()
            out = det.forward_grounding(backbone_out=bo, find_input=self._fs, find_target=None,
                                        geometric_prompt=gp)
            probs = (out["pred_logits"].sigmoid() * out["presence_logit_dec"].sigmoid().unsqueeze(1)).squeeze(-1)[0]
            keep = probs > self.min_score
            masks = self._upsample(out["pred_masks"][0][keep], h, w)
            scores = probs[keep].float().cpu().numpy()
        order = np.argsort(-scores)
        return [(masks[i], float(scores[i])) for i in order]

    # -- visual: points and/or one box ----------------------------------------------------------

    def segment_visual(self, image, p):
        t = self._torch
        w, h = image.size
        trk = self.tracker
        coords, labels, box = visual_to_sam1(p)
        pts, lab = [], []
        if box:  # SAM 2/3 convention: box = two corner points, labels 2 (top-left), 3 (bottom-right)
            pts += [[box[0], box[1]], [box[2], box[3]]]
            lab += [2, 3]
        pts += coords
        lab += labels
        pc = t.tensor(pts, dtype=t.float32, device=self.device)
        pc = pc / t.tensor([w, h], dtype=t.float32, device=self.device) * self.RES
        pl = t.tensor(lab, dtype=t.int32, device=self.device)
        with t.inference_mode(), self._autocast():
            bo = self.detector.backbone.forward_image(
                self._image_tensor(image), need_sam3_out=False,
                need_interactive_out=True, need_propagation_out=False)
            fpn = bo["interactive"]["backbone_fpn"]
            dec = trk.interactive_sam_mask_decoder
            fpn[0].tensors = dec.conv_s0(fpn[0].tensors)
            fpn[1].tensors = dec.conv_s1(fpn[1].tensors)
            feats = trk._prepare_backbone_features(bo)["interactive"]
            vf, fs = feats["vision_feats"], feats["feat_sizes"]
            high_res = [x.permute(1, 2, 0).view(x.size(1), x.size(2), *s) for x, s in zip(vf[:-1], fs[:-1])]
            pix = trk._get_interactive_pix_mem(vf, fs)
            sparse, dense = trk.interactive_sam_prompt_encoder(
                points=(pc[None], pl[None]), boxes=None, masks=None)
            low, ious, _, _ = dec(
                image_embeddings=pix, image_pe=trk.interactive_sam_prompt_encoder.get_dense_pe(),
                sparse_prompt_embeddings=sparse, dense_prompt_embeddings=dense,
                multimask_output=p.multimask, repeat_image=False, high_res_features=high_res)
            masks = self._upsample(low[0], h, w)
            iou = ious[0].float()
            scores = (iou if dec.iou_prediction_head.sigmoid_output else iou.sigmoid()).cpu().numpy()  # -> [0, 1]
        order = np.argsort(-scores)
        return [(masks[i], float(scores[i])) for i in order]
