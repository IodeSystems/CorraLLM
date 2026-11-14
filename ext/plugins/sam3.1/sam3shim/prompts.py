"""Request validation and pixel -> SAM prompt conversion.

Frame: pixels of the EXIF-upright image, origin top-left, continuous coordinates
(pixel (i, j) covers [i, i+1) x [j, j+1)). Out-of-range values are refused, never clamped.

Request fields (OpenAI /v1/images/edits style; JSON or multipart/form-data):
  model, image | image[] (1..16), prompt, points, boxes, n, response_format, threshold.
points / boxes apply to every image, or per image (see _per_image).
"""
import math
from dataclasses import dataclass

MAX_IMAGES = 16
MAX_POINTS = 64   # per image
MAX_BOXES = 16    # per image
MAX_TEXT = 256
MAX_N = 1000
FORMATS = ("rle", "png", "polygon")


class ValidationError(ValueError):
    def __init__(self, message: str, param: str | None = None, code: str | None = None):
        super().__init__(message)
        self.param = param
        self.code = code


@dataclass
class Exemplar:
    box: tuple[float, float, float, float]  # xyxy pixels
    label: int = 1


@dataclass
class Point:
    x: float
    y: float
    label: int


@dataclass
class ConceptPrompt:
    text: str | None
    boxes: list[Exemplar]


@dataclass
class VisualPrompt:
    points: list[Point]
    box: tuple[float, float, float, float] | None
    multimask: bool


@dataclass
class Request:
    model: str
    images: list            # each str (data URL, base64, http(s) URL) or bytes (multipart file)
    image_list: bool        # image given as a list (errors say image[i], not image)
    text: str | None
    points: list            # raw, shape-checked: list of per-image raw lists, length == len(images)
    boxes: list
    points_shared: bool     # one flat list given for all images (affects `param` paths only)
    boxes_shared: bool
    n: int = 100
    response_format: str = "rle"
    threshold: float = 0.5


def _num(v, param: str) -> float:
    if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v):
        raise ValidationError("must be a finite number", param)
    return float(v)


def _label(v, param: str) -> int:
    if isinstance(v, bool) or not isinstance(v, int) or v not in (0, 1):
        raise ValidationError("label must be the integer 0 or 1", param)
    return v


def _only(d: dict, allowed: set, param: str):
    extra = sorted(set(d) - allowed)
    if extra:
        raise ValidationError(f"unknown or not allowed field '{extra[0]}'", f"{param}.{extra[0]}")


def _per_image(v, count: int, param: str, kind: str):
    """-> (list of per-image raw lists, shared). `kind` is 'points' (items are objects) or
    'boxes' (items are [x0,y0,x1,y1]).

    Flat form: a list of items, applied to EVERY image.
      points [{x,y,label}, ...]      boxes [[x0,y0,x1,y1], ...]
    Per-image form: a list of per-image lists, exactly one per image (use [] for none).
      points [[{...}], [{...}, {...}]]   boxes [[[...]], [[...], [...]]]
    The two are told apart by the element type, so `[]` alone is 'none for all'. A list mixing
    the two forms is refused."""
    if v is None:
        return [[] for _ in range(count)], True
    if not isinstance(v, list):
        raise ValidationError(f"{param} must be a list", param)

    def is_item(e):
        if kind == "points":
            return isinstance(e, dict)
        return isinstance(e, list) and len(e) > 0 and not any(isinstance(c, list) for c in e)

    def is_group(e):
        if not isinstance(e, list):
            return False
        return all(isinstance(c, dict) for c in e) if kind == "points" else all(isinstance(c, list) for c in e)

    if all(is_item(e) for e in v):  # includes []
        return [list(v) for _ in range(count)], True
    if all(is_group(e) for e in v):
        if len(v) != count:
            raise ValidationError(
                f"per-image {param} needs exactly one list per image ({count}), got {len(v)}", param)
        return [list(g) for g in v], False
    raise ValidationError(
        f"{param} must be one flat list for all images or one list per image, not a mix", param)


def _text_value(v, param: str, what: str):
    if not isinstance(v, str) or not v.strip() or len(v) > MAX_TEXT:
        raise ValidationError(f"{what} must be a non-empty string up to {MAX_TEXT} chars", param)
    return v.strip()


def parse_request(body) -> Request:
    """Checks that do not need the image sizes."""
    if not isinstance(body, dict):
        raise ValidationError("body must be a JSON object")
    model = body.get("model")
    if not isinstance(model, str) or not model:
        raise ValidationError("model is required", "model")

    if "image" in body and "image[]" in body:
        raise ValidationError("give image or image[], not both", "image[]")
    key = "image[]" if "image[]" in body else "image"
    raw = body.get(key)
    image_list = isinstance(raw, list)
    images = raw if image_list else [raw]
    if raw is None or raw == "" or raw == b"" or not images:
        raise ValidationError("image is required", key)
    if len(images) > MAX_IMAGES:
        raise ValidationError(f"at most {MAX_IMAGES} images per request", key)
    for i, im in enumerate(images):
        if not isinstance(im, (str, bytes)) or not im:
            raise ValidationError("image must be a data URL, base64, http(s) URL or file",
                                  f"{key}[{i}]" if image_list else key)
    count = len(images)

    text = body.get("prompt")
    if text is not None:
        text = _text_value(text, "prompt", "prompt")
    points, p_shared = _per_image(body.get("points"), count, "points", "points")
    boxes, b_shared = _per_image(body.get("boxes"), count, "boxes", "boxes")
    for i, g in enumerate(points):
        if len(g) > MAX_POINTS:
            raise ValidationError(f"at most {MAX_POINTS} points per image", "points" if p_shared else f"points[{i}]")
    for i, g in enumerate(boxes):
        if len(g) > MAX_BOXES:
            raise ValidationError(f"at most {MAX_BOXES} boxes per image", "boxes" if b_shared else f"boxes[{i}]")
    if text is None and not any(points) and not any(boxes):
        raise ValidationError("prompt is required unless points or boxes are given", "prompt")

    thr = _num(body.get("threshold", 0.5), "threshold")
    if not 0 <= thr <= 1:
        raise ValidationError("threshold must be 0..1", "threshold")
    n = body.get("n", 100)
    if isinstance(n, bool) or not isinstance(n, int) or not 1 <= n <= MAX_N:
        raise ValidationError(f"n must be an integer 1..{MAX_N}", "n")
    fmt = body.get("response_format", "rle")
    if fmt not in FORMATS:
        raise ValidationError("response_format must be 'rle', 'png' or 'polygon'", "response_format")
    if fmt == "polygon":
        raise ValidationError("response_format 'polygon' is not implemented; use 'rle' or 'png'",
                              "response_format", "unsupported_value")
    return Request(model=model, images=images, image_list=image_list, text=text, points=points,
                   boxes=boxes, points_shared=p_shared, boxes_shared=b_shared, n=n,
                   response_format=fmt, threshold=thr)


def _path(base: str, shared: bool, i: int, j: int, sub: str = "") -> str:
    """param path of item j: `points[j].x` when flat, `points[i][j].x` when per image."""
    return f"{base}[{j}]{sub}" if shared else f"{base}[{i}][{j}]{sub}"


def _point(q, w: int, h: int, param: str) -> Point:
    if not isinstance(q, dict):
        raise ValidationError("point must be {x, y, label}", param)
    _only(q, {"x", "y", "label"}, param)
    x = _num(q.get("x"), f"{param}.x")
    y = _num(q.get("y"), f"{param}.y")
    if not 0 <= x <= w:
        raise ValidationError(f"out of range 0..{w} (pixels)", f"{param}.x")
    if not 0 <= y <= h:
        raise ValidationError(f"out of range 0..{h} (pixels)", f"{param}.y")
    if "label" not in q:
        raise ValidationError("label is required (1 include, 0 exclude)", f"{param}.label")
    return Point(x, y, _label(q["label"], f"{param}.label"))


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


def prompt_for_image(req: Request, i: int, w: int, h: int):
    """Validate image i's points/boxes against its own upright size -> ConceptPrompt | VisualPrompt.

    prompt and/or boxes -> concept (every instance; boxes are positive exemplars).
    points alone        -> visual (one object).
    points with prompt or boxes -> 400: the model's concept path takes no points."""
    where = f" (image {i})" if len(req.images) > 1 else ""
    try:
        pts = [_point(q, w, h, _path("points", req.points_shared, i, j)) for j, q in enumerate(req.points[i])]
        boxes = [Exemplar(_box(b, w, h, _path("boxes", req.boxes_shared, i, j))) for j, b in enumerate(req.boxes[i])]
    except ValidationError as e:
        if where:
            e.args = (e.args[0] + where,)
        raise
    if pts and (req.text or boxes):
        raise ValidationError("points cannot be combined with prompt or boxes; send points alone "
                              "(one object) or prompt/boxes (every instance)" + where, "points")
    if pts:
        return VisualPrompt(pts, None, False)
    if not req.text and not boxes:
        raise ValidationError("prompt is required unless points or boxes are given" + where, "prompt")
    return ConceptPrompt(req.text, boxes)


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
