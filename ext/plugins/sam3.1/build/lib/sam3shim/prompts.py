"""Request validation and pixel -> SAM prompt conversion.

Frame: pixels of the EXIF-upright image, origin top-left, continuous coordinates
(pixel (i, j) covers [i, i+1) x [j, j+1)). Out-of-range values are refused, never clamped.
"""
import math
from dataclasses import dataclass, field

MAX_PROMPTS = 16
MAX_POINTS = 64
MAX_EXEMPLARS = 16
MAX_TEXT = 256


class ValidationError(ValueError):
    def __init__(self, message: str, param: str | None = None, code: str | None = None):
        super().__init__(message)
        self.param = param
        self.code = code


@dataclass
class Exemplar:
    box: tuple[float, float, float, float]  # xyxy pixels
    label: int


@dataclass
class Point:
    x: float
    y: float
    label: int


@dataclass
class ConceptPrompt:
    text: str | None
    boxes: list[Exemplar]
    type: str = "concept"


@dataclass
class VisualPrompt:
    points: list[Point]
    box: tuple[float, float, float, float] | None
    multimask: bool
    type: str = "visual"


@dataclass
class Request:
    model: str
    image: str
    prompts: list = field(default_factory=list)  # filled by validate_prompts
    raw_prompts: list = field(default_factory=list)
    threshold: float = 0.5
    max_masks: int = 100
    output: str = "rle"


def _num(v, param: str) -> float:
    if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v):
        raise ValidationError("must be a finite number", param)
    return float(v)


def _label(v, param: str) -> int:
    if isinstance(v, bool) or not isinstance(v, int) or v not in (0, 1):
        raise ValidationError("label must be the integer 0 or 1", param)
    return v


def _box(v, w: int, h: int, param: str) -> tuple[float, float, float, float]:
    if not isinstance(v, list) or len(v) != 4:
        raise ValidationError("box must be [x0, y0, x1, y1]", param)
    x0, y0, x1, y1 = (_num(c, f"{param}[{i}]") for i, c in enumerate(v))
    for i, (c, hi) in enumerate(((x0, w), (y0, h), (x1, w), (y1, h))):
        if not 0 <= c <= hi:
            raise ValidationError(f"out of range 0..{hi} (pixels)", f"{param}[{i}]")
    if x0 >= x1:
        raise ValidationError("x0 must be less than x1", param)
    if y0 >= y1:
        raise ValidationError("y0 must be less than y1", param)
    return (x0, y0, x1, y1)


def _only(d: dict, allowed: set, param: str):
    extra = sorted(set(d) - allowed)
    if extra:
        raise ValidationError(f"unknown or not allowed field '{extra[0]}'", f"{param}.{extra[0]}")


def parse_request(body) -> Request:
    """Top-level checks that do not need the image size."""
    if not isinstance(body, dict):
        raise ValidationError("body must be a JSON object")
    model = body.get("model")
    if not isinstance(model, str) or not model:
        raise ValidationError("model is required", "model")
    image = body.get("image")
    if not isinstance(image, str) or not image:
        raise ValidationError("image is required", "image")
    prompts = body.get("prompts")
    if not isinstance(prompts, list) or not 1 <= len(prompts) <= MAX_PROMPTS:
        raise ValidationError(f"prompts must be a list of 1..{MAX_PROMPTS}", "prompts")
    thr = body.get("threshold", 0.5)
    thr = _num(thr, "threshold")
    if not 0 <= thr <= 1:
        raise ValidationError("threshold must be 0..1", "threshold")
    mm = body.get("max_masks", 100)
    if isinstance(mm, bool) or not isinstance(mm, int) or not 1 <= mm <= 1000:
        raise ValidationError("max_masks must be an integer 1..1000", "max_masks")
    out = body.get("output", "rle")
    if out not in ("rle", "boxes"):
        raise ValidationError("output must be 'rle' or 'boxes'", "output")
    return Request(model=model, image=image, raw_prompts=prompts, threshold=thr, max_masks=mm, output=out)


def validate_prompts(raw: list, w: int, h: int) -> list:
    return [_prompt(p, i, w, h) for i, p in enumerate(raw)]


def _prompt(p, i: int, w: int, h: int):
    base = f"prompts[{i}]"
    if not isinstance(p, dict):
        raise ValidationError("prompt must be an object", base)
    t = p.get("type")
    if t == "concept":
        if "points" in p:
            raise ValidationError("concept prompts take text and boxes, not points", f"{base}.points")
        _only(p, {"type", "text", "boxes"}, base)
        text = p.get("text")
        if text is not None:
            if not isinstance(text, str) or not text.strip() or len(text) > MAX_TEXT:
                raise ValidationError(f"text must be a non-empty string up to {MAX_TEXT} chars", f"{base}.text")
            text = text.strip()
        raw_boxes = p.get("boxes", [])
        if not isinstance(raw_boxes, list) or len(raw_boxes) > MAX_EXEMPLARS:
            raise ValidationError(f"boxes must be a list of up to {MAX_EXEMPLARS}", f"{base}.boxes")
        boxes = []
        for j, b in enumerate(raw_boxes):
            bp = f"{base}.boxes[{j}]"
            if not isinstance(b, dict):
                raise ValidationError("exemplar must be {box, label}", bp)
            _only(b, {"box", "label"}, bp)
            boxes.append(Exemplar(_box(b.get("box"), w, h, f"{bp}.box"), _label(b.get("label", 1), f"{bp}.label")))
        if text is None and not boxes:
            raise ValidationError("concept prompt needs text and/or boxes", base)
        return ConceptPrompt(text, boxes)
    if t == "visual":
        for k in ("text", "boxes"):
            if k in p:
                raise ValidationError(f"visual prompts take points and one box, not {k}", f"{base}.{k}")
        _only(p, {"type", "points", "box", "multimask"}, base)
        raw_pts = p.get("points", [])
        if not isinstance(raw_pts, list) or len(raw_pts) > MAX_POINTS:
            raise ValidationError(f"points must be a list of up to {MAX_POINTS}", f"{base}.points")
        pts = []
        for j, q in enumerate(raw_pts):
            pp = f"{base}.points[{j}]"
            if not isinstance(q, dict):
                raise ValidationError("point must be {x, y, label}", pp)
            _only(q, {"x", "y", "label"}, pp)
            x = _num(q.get("x"), f"{pp}.x")
            y = _num(q.get("y"), f"{pp}.y")
            if not 0 <= x <= w:
                raise ValidationError(f"out of range 0..{w} (pixels)", f"{pp}.x")
            if not 0 <= y <= h:
                raise ValidationError(f"out of range 0..{h} (pixels)", f"{pp}.y")
            if "label" not in q:
                raise ValidationError("label is required (1 include, 0 exclude)", f"{pp}.label")
            pts.append(Point(x, y, _label(q["label"], f"{pp}.label")))
        box = _box(p["box"], w, h, f"{base}.box") if p.get("box") is not None else None
        if not pts and box is None:
            raise ValidationError("visual prompt needs at least one point or a box", base)
        mm = p.get("multimask", False)
        if not isinstance(mm, bool):
            raise ValidationError("multimask must be a boolean", f"{base}.multimask")
        return VisualPrompt(pts, box, mm)
    raise ValidationError("type must be 'concept' or 'visual'", f"{base}.type")


# ---- pixel -> SAM conversions -------------------------------------------------

def box_xyxy_to_cxcywh_norm(box, w: int, h: int) -> list[float]:
    """Pixel xyxy -> SAM 3 geometric-prompt format: normalized [cx, cy, w, h] in [0, 1]."""
    x0, y0, x1, y1 = box
    return [(x0 + x1) / 2 / w, (y0 + y1) / 2 / h, (x1 - x0) / w, (y1 - y0) / h]


def visual_to_sam1(p: VisualPrompt):
    """-> (point_coords [[x, y], ...] pixels, point_labels [..], box [x0, y0, x1, y1] pixels or None).

    SAM's predictor normalizes pixel coords itself (normalize_coords=True), so pixels pass through."""
    coords = [[pt.x, pt.y] for pt in p.points]
    labels = [pt.label for pt in p.points]
    return coords, labels, (list(p.box) if p.box else None)
