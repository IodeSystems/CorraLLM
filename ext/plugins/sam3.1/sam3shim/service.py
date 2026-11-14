"""Request -> response, independent of HTTP."""
import base64
import io
import time

from PIL import Image

from . import rle
from .backends import Backend
from .fetch import make_fetcher
from .orient import ImageError, decode_base64, is_url, load_upright, upright_size
from .prompts import ConceptPrompt, ValidationError, parse_request, prompt_for_image


def _png(mask) -> dict:
    buf = io.BytesIO()
    Image.fromarray(mask.astype("uint8") * 255, "L").save(buf, "PNG")
    return {"format": "png", "b64_json": base64.b64encode(buf.getvalue()).decode()}


def _mask_obj(mask, fmt: str) -> dict:
    if fmt == "png":
        return _png(mask)
    return {"format": "rle", **rle.encode(mask)}


def segment(body, backend: Backend, fetch=None) -> dict:
    """Raises ValidationError / ImageError for 400s. Images run one at a time.

    All images are fetched and their prompts validated (header-only size) before the first
    inference, so a bad request never spends GPU time."""
    fetch = fetch or make_fetcher()
    req = parse_request(body)
    key = "image[]" if "image[]" in body else "image"

    raws = []
    for i, im in enumerate(req.images):
        param = f"{key}[{i}]" if req.image_list else key
        try:
            if isinstance(im, str):
                im = fetch(im) if is_url(im) else decode_base64(im)
            raws.append((im, upright_size(im)))
        except ImageError as e:
            raise ValidationError(str(e), param, e.code) from None
    prompts = [prompt_for_image(req, i, *size) for i, (_, size) in enumerate(raws)]

    data = []
    total = 0
    for i, ((raw, _), p) in enumerate(zip(raws, prompts)):
        try:
            img = load_upright(raw)
        except ImageError as e:
            raise ValidationError(str(e), f"{key}[{i}]" if req.image_list else key, e.code) from None
        w, h = img.size
        cands = (backend.segment_concept if isinstance(p, ConceptPrompt) else backend.segment_visual)(img, p)
        cands = [(m, s) for m, s in cands if s >= req.threshold and m.any()]
        cands.sort(key=lambda c: -c[1])
        cands = cands[: 1 if not isinstance(p, ConceptPrompt) else req.n][: req.n]
        segs = [{"score": round(float(s), 6), "box": rle.bbox(m), "area": rle.area(m),
                 "mask": _mask_obj(m, req.response_format)} for m, s in cands]
        total += len(segs)
        data.append({"index": i, "width": w, "height": h, "segments": segs})
    return {"created": int(time.time()), "data": data,
            "usage": {"images": len(data), "masks": total}}
