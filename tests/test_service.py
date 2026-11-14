import base64
import io
import json
import threading
import urllib.error
import urllib.request

import numpy as np
import pytest
from PIL import Image

from sam3shim import rle
from sam3shim.backends import StubBackend
from sam3shim.prompts import ValidationError
from sam3shim.server import make_server
from sam3shim.service import segment

from conftest import png_b64

B = StubBackend()


def body(img, prompts, **kw):
    return {"model": "sam3-segment", "image": img, "prompts": prompts, **kw}


def pt(x, y, label=1):
    return {"type": "visual", "points": [{"x": x, "y": y, "label": label}]}


def test_visual_point_mask_and_box_from_mask(scene):
    r = segment(body(png_b64(scene), [pt(30, 20)]), B)
    assert r["image"] == {"width": 100, "height": 60}
    p = r["results"][0]["predictions"][0]
    assert p["box"] == [20.0, 10.0, 40.0, 30.0]
    assert p["area"] == 400
    assert p["mask"]["size"] == [60, 100]
    m = rle.decode(p["mask"])
    assert m[10:30, 20:40].all() and m.sum() == 400
    assert r["usage"] == {"images": 1, "prompts": 1, "masks": 1}


def test_exif_rotated_frame_and_point_maps_to_same_object():
    # stored 60 wide x 100 high; EXIF 6 = rotate 90 CW on display -> upright 100 wide x 60 high.
    # Draw in the upright frame, then invert the rotation to store it.
    up = np.full((60, 100, 3), 255, np.uint8)
    up[10:30, 20:40] = (255, 0, 0)
    stored = np.rot90(up, k=1)  # inverse of 90 CW
    img = Image.fromarray(np.ascontiguousarray(stored))
    exif = Image.Exif(); exif[0x0112] = 6
    buf = io.BytesIO(); img.save(buf, "JPEG", quality=100, exif=exif.tobytes())
    data = "data:image/jpeg;base64," + base64.b64encode(buf.getvalue()).decode()
    assert Image.open(io.BytesIO(buf.getvalue())).size == (60, 100)  # stored frame
    r = segment(body(data, [pt(30, 20)]), B)
    assert r["image"] == {"width": 100, "height": 60}
    p = r["results"][0]["predictions"][0]
    assert p["mask"]["size"] == [60, 100]
    x0, y0, x1, y1 = p["box"]
    assert abs(x0 - 20) <= 2 and abs(x1 - 40) <= 2 and abs(y0 - 10) <= 2 and abs(y1 - 30) <= 2  # JPEG blur
    # a point in the stored frame's coordinates of the same object would be out of range upright
    with pytest.raises(ValidationError) as e:
        segment(body(data, [pt(10, 80)]), B)
    assert e.value.param == "prompts[0].points[0].y"


def test_box_prompt(scene):
    r = segment(body(png_b64(scene), [{"type": "visual", "box": [10, 0, 50, 40]}]), B)
    assert r["results"][0]["predictions"][0]["box"] == [20.0, 10.0, 40.0, 30.0]


def test_multimask_ranked(scene):
    r = segment(body(png_b64(scene), [{**pt(30, 20), "multimask": True}], threshold=0.0), B)
    sc = [p["score"] for p in r["results"][0]["predictions"]]
    assert len(sc) == 3 and sc == sorted(sc, reverse=True)


def test_concept_text_threshold_and_max_masks(scene):
    c = {"type": "concept", "text": "thing"}
    r = segment(body(png_b64(scene), [c]), B)
    preds = r["results"][0]["predictions"]
    assert len(preds) == 2 and preds[0]["score"] >= preds[1]["score"]
    r = segment(body(png_b64(scene), [c], threshold=0.85), B)
    assert len(r["results"][0]["predictions"]) == 1
    r = segment(body(png_b64(scene), [c], max_masks=1, threshold=0.0), B)
    assert len(r["results"][0]["predictions"]) == 1
    r = segment(body(png_b64(scene), [c], threshold=1.0), B)
    assert r["results"][0]["predictions"] == []


def test_concept_box_exemplar(scene):
    r = segment(body(png_b64(scene), [{"type": "concept", "boxes": [{"box": [58, 33, 92, 57], "label": 1}]}]), B)
    assert r["results"][0]["predictions"][0]["box"] == [60.0, 35.0, 90.0, 55.0]


def test_output_boxes_has_no_mask(scene):
    r = segment(body(png_b64(scene), [pt(30, 20)], output="boxes"), B)
    assert "mask" not in r["results"][0]["predictions"][0]


def test_image_errors(scene):
    for img, code in (("http://x/y.png", None), ("!!!", None), (base64.b64encode(b"nope").decode(), None)):
        with pytest.raises(ValidationError) as e:
            segment(body(img, [pt(1, 1)]), B)
        assert e.value.param == "image"
    big = Image.new("RGB", (4097, 8)); buf = io.BytesIO(); big.save(buf, "PNG")
    with pytest.raises(ValidationError) as e:
        segment(body(base64.b64encode(buf.getvalue()).decode(), [pt(1, 1)]), B)
    assert e.value.code == "image_too_large"


@pytest.fixture
def http_server():
    srv = make_server(B, "127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def call(url, payload=None, raw=None):
    req = urllib.request.Request(url, data=raw if raw is not None else json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)


def test_http(http_server, scene):
    with urllib.request.urlopen(http_server + "/health") as r:
        assert json.load(r)["status"] == "ok"
    s, j = call(http_server + "/v1/images/segmentations", body(png_b64(scene), [pt(30, 20)]))
    assert s == 200 and j["object"] == "image.segmentation" and j["id"].startswith("seg_")
    s, j = call(http_server + "/v1/images/segmentations", body(png_b64(scene), [pt(300, 20)]))
    assert s == 400 and j["error"]["param"] == "prompts[0].points[0].x" and j["error"]["type"] == "invalid_request_error"
    s, j = call(http_server + "/v1/images/segmentations", raw=b"{")
    assert s == 400
    s, j = call(http_server + "/nope", {})
    assert s == 404
