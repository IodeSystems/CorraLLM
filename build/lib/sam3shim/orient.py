"""Image decode with EXIF orientation applied. All coordinates are in the upright frame."""
import base64
import binascii
import io
import re

from PIL import Image, ImageOps

MAX_SIDE = 4096
_DATA_URL = re.compile(r"^data:image/[A-Za-z0-9.+-]+;base64,", re.I)
FORMATS = {"JPEG", "PNG", "WEBP"}


class ImageError(ValueError):
    def __init__(self, message: str, code: str | None = None):
        super().__init__(message)
        self.code = code


def decode_base64(value: str) -> bytes:
    if re.match(r"^[a-z][a-z0-9+.-]*://", value, re.I):
        raise ImageError("image URLs are not fetched; send a data URL or base64")
    value = _DATA_URL.sub("", value, count=1)
    if value.lower().startswith("data:"):
        raise ImageError("image data URL must be base64 encoded image/*")
    try:
        return base64.b64decode(value, validate=False if "\n" in value else True)
    except (binascii.Error, ValueError):
        raise ImageError("image is not valid base64") from None


def load_upright(data: bytes) -> Image.Image:
    """Decode JPEG/PNG/WebP, apply EXIF orientation, return RGB. Size checked before pixel load."""
    try:
        img = Image.open(io.BytesIO(data))
    except Exception:
        raise ImageError("image is not a decodable JPEG, PNG or WebP") from None
    if img.format not in FORMATS:
        raise ImageError("image must be JPEG, PNG or WebP")
    w, h = img.size  # header only; rotation swaps sides but not the cap
    if w > MAX_SIDE or h > MAX_SIDE:
        raise ImageError(f"image {w}x{h} exceeds {MAX_SIDE}x{MAX_SIDE}", "image_too_large")
    try:
        img.load()
        img = ImageOps.exif_transpose(img)
        return img.convert("RGB")
    except Exception:
        raise ImageError("image is corrupt") from None
