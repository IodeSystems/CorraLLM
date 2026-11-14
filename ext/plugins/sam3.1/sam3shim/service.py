"""Request -> response, independent of HTTP."""
import secrets
import time

from . import rle
from .backends import Backend
from .orient import ImageError, decode_base64, load_upright
from .prompts import ConceptPrompt, ValidationError, parse_request, validate_prompts


def _new_id() -> str:
    return "seg_" + f"{int(time.time() * 1000):x}" + secrets.token_hex(5)


def segment(body, backend: Backend) -> dict:
    """Raises ValidationError / ImageError for 400s."""
    req = parse_request(body)
    try:
        img = load_upright(decode_base64(req.image))
    except ImageError as e:
        raise ValidationError(str(e), "image", e.code) from None
    w, h = img.size
    prompts = validate_prompts(req.raw_prompts, w, h)

    results = []
    total = 0
    for i, p in enumerate(prompts):
        cands = (backend.segment_concept if isinstance(p, ConceptPrompt) else backend.segment_visual)(img, p)
        cands = [(m, s) for m, s in cands if s >= req.threshold and m.any()]
        cands.sort(key=lambda c: -c[1])
        if p.type == "visual":
            cands = cands[: 3 if p.multimask else 1]
        cands = cands[: req.max_masks]
        preds = []
        for m, s in cands:
            pred = {"score": round(float(s), 6), "box": rle.bbox(m), "area": rle.area(m)}
            if req.output == "rle":
                pred["mask"] = rle.encode(m)
            preds.append(pred)
        total += len(preds)
        results.append({"prompt_index": i, "type": p.type, "predictions": preds})
    return {
        "id": _new_id(), "object": "image.segmentation", "created": int(time.time()), "model": req.model,
        "image": {"width": w, "height": h}, "results": results,
        "usage": {"images": 1, "prompts": len(prompts), "masks": total},
    }
