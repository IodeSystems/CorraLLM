"""multipart/form-data -> the same dict a JSON body gives parse_request.

Fields: model, prompt, n, threshold, response_format (text); points, boxes (text holding JSON);
image / image[] (file part = raw bytes; a part without a filename is text: data URL, base64 or URL).
Repeated image parts make a list. Numbers arrive as text and are converted here.
"""
import json
from email import policy
from email.parser import BytesParser

from .prompts import ValidationError

_JSON_FIELDS = ("points", "boxes")


def parse_multipart(content_type: str, raw: bytes) -> dict:
    msg = BytesParser(policy=policy.HTTP).parsebytes(
        b"Content-Type: " + content_type.encode("latin-1", "replace") + b"\r\nMIME-Version: 1.0\r\n\r\n" + raw)
    if not msg.is_multipart():
        raise ValidationError("body is not valid multipart/form-data")
    out: dict = {}
    images: list = []
    key = "image"
    for part in msg.iter_parts():
        name = part.get_param("name", header="content-disposition")
        if not name:
            continue
        data = part.get_payload(decode=True) or b""
        if name in ("image", "image[]"):
            key = name if not images else key
            images.append(data if part.get_filename() is not None else data.decode("utf-8", "replace").strip())
            continue
        out[name] = data.decode("utf-8", "replace").strip()
    if images:
        out[key] = images if (key == "image[]" or len(images) > 1) else images[0]
    for f in _JSON_FIELDS:
        if f in out:
            try:
                out[f] = json.loads(out[f])
            except ValueError:
                raise ValidationError(f"{f} must be JSON", f) from None
    if "n" in out:
        try:
            out["n"] = int(out["n"])
        except ValueError:
            raise ValidationError("n must be an integer", "n") from None
    if "threshold" in out:
        try:
            out["threshold"] = float(out["threshold"])
        except ValueError:
            raise ValidationError("threshold must be a number", "threshold") from None
    return out
