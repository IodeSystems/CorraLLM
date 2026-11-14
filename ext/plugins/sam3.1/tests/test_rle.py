import numpy as np
import pytest
from pycocotools import mask as cm

from sam3shim import rle


def cases():
    rng = np.random.default_rng(1)
    yield np.zeros((5, 7), bool)
    yield np.ones((5, 7), bool)
    m = np.zeros((6, 9), bool); m[0, 0] = True; yield m
    m = np.zeros((6, 9), bool); m[-1, -1] = True; yield m
    yield rng.random((37, 53)) > 0.5
    yield rng.random((300, 200)) > 0.97
    m = np.zeros((1000, 1500), bool); m[10:900, 20:1400] = True; yield m


@pytest.mark.parametrize("m", list(cases()))
def test_matches_pycocotools(m):
    mine = rle.encode(m)
    ref = cm.encode(np.asfortranarray(m.astype(np.uint8)))
    assert mine["size"] == [m.shape[0], m.shape[1]]
    assert mine["counts"] == ref["counts"].decode()
    assert (cm.decode({"size": mine["size"], "counts": mine["counts"].encode()}).astype(bool) == m).all()
    assert (rle.decode(mine) == m).all()
    assert (rle.decode({"size": ref["size"], "counts": ref["counts"]}) == m).all()
    assert cm.area(ref) == rle.area(m)


def test_bbox_exclusive_edges():
    m = np.zeros((10, 20), bool); m[2:5, 3:8] = True
    assert rle.bbox(m) == [3.0, 2.0, 8.0, 5.0]
    assert rle.bbox(np.zeros((3, 3), bool)) is None
