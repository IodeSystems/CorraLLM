"""COCO RLE encode/decode, compressed counts string compatible with pycocotools.

Runs are column-major (Fortran order), alternate starting with a zero-run.
size is [height, width].
"""
import numpy as np


def _runs(mask: np.ndarray) -> list[int]:
    flat = np.asarray(mask, dtype=bool).ravel(order="F")
    n = flat.size
    if n == 0:
        return []
    change = np.flatnonzero(flat[1:] != flat[:-1]) + 1
    bounds = np.concatenate(([0], change, [n]))
    runs = np.diff(bounds).tolist()
    if flat[0]:  # must start with a run of zeros
        runs.insert(0, 0)
    return runs


def _compress(runs: list[int]) -> str:
    out = []
    for i, x in enumerate(runs):
        if i > 2:
            x -= runs[i - 2]
        more = True
        while more:
            c = x & 0x1F
            x >>= 5  # arithmetic shift, as in the C code
            more = (x != -1) if (c & 0x10) else (x != 0)
            if more:
                c |= 0x20
            out.append(chr(c + 48))
    return "".join(out)


def _decompress(s: str) -> list[int]:
    runs: list[int] = []
    p = 0
    while p < len(s):
        x = 0
        k = 0
        more = True
        while more:
            c = ord(s[p]) - 48
            x |= (c & 0x1F) << (5 * k)
            more = bool(c & 0x20)
            p += 1
            k += 1
            if not more and (c & 0x10):
                x |= -1 << (5 * k)
        if len(runs) > 2:
            x += runs[len(runs) - 2]
        runs.append(x)
    return runs


def encode(mask: np.ndarray) -> dict:
    """mask: 2-D bool array [H, W] -> {"size": [H, W], "counts": str}."""
    mask = np.asarray(mask, dtype=bool)
    if mask.ndim != 2:
        raise ValueError("mask must be 2-D")
    h, w = mask.shape
    return {"size": [h, w], "counts": _compress(_runs(mask))}


def decode(rle: dict) -> np.ndarray:
    h, w = rle["size"]
    counts = rle["counts"]
    runs = _decompress(counts.decode() if isinstance(counts, bytes) else counts)
    flat = np.zeros(h * w, dtype=bool)
    pos = 0
    val = False
    for r in runs:
        if val:
            flat[pos:pos + r] = True
        pos += r
        val = not val
    return flat.reshape((h, w), order="F")


def area(mask: np.ndarray) -> int:
    return int(np.count_nonzero(mask))


def bbox(mask: np.ndarray) -> list[float] | None:
    """[x0, y0, x1, y1] pixel edges (x1, y1 exclusive); None for an empty mask."""
    ys, xs = np.nonzero(mask)
    if xs.size == 0:
        return None
    return [float(xs.min()), float(ys.min()), float(xs.max() + 1), float(ys.max() + 1)]
