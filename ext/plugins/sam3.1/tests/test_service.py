import base64
import http.server
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
from sam3shim.fetch import make_fetcher
from sam3shim.prompts import ValidationError
from sam3shim.server import make_server
from sam3shim.service import segment

from conftest import png_b64

B = StubBackend()
ROUTE = "/v1/images/segmentations"


def body(img, **kw):
    return {"model": "sam3-segment", "image": img, **kw}


def pts(x, y, label=1):
    return [{"x": x, "y": y, "label": label}]


def test_point_mask_and_box_from_mask(scene):
    r = segment(body(png_b64(scene), points=pts(30, 20)), B)
    assert set(r) == {"created", "data", "usage"}
    d = r["data"][0]
    assert (d["index"], d["width"], d["height"]) == (0, 100, 60)
    s = d["segments"][0]
    assert s["box"] == [20.0, 10.0, 40.0, 30.0] and s["area"] == 400
    assert s["mask"]["format"] == "rle" and s["mask"]["size"] == [60, 100]
    m = rle.decode(s["mask"])
    assert m[10:30, 20:40].all() and m.sum() == 400
    assert r["usage"] == {"images": 1, "masks": 1}


def test_exif_rotated_frame_and_point_maps_to_same_object():
    # stored 60 wide x 100 high; EXIF 6 = rotate 90 CW on display -> upright 100 wide x 60 high.
    up = np.full((60, 100, 3), 255, np.uint8)
    up[10:30, 20:40] = (255, 0, 0)
    img = Image.fromarray(np.ascontiguousarray(np.rot90(up, k=1)))  # inverse of 90 CW
    exif = Image.Exif(); exif[0x0112] = 6
    buf = io.BytesIO(); img.save(buf, "JPEG", quality=100, exif=exif.tobytes())
    data = "data:image/jpeg;base64," + base64.b64encode(buf.getvalue()).decode()
    assert Image.open(io.BytesIO(buf.getvalue())).size == (60, 100)  # stored frame
    r = segment(body(data, points=pts(30, 20)), B)
    d = r["data"][0]
    assert (d["width"], d["height"]) == (100, 60)
    x0, y0, x1, y1 = d["segments"][0]["box"]
    assert abs(x0 - 20) <= 2 and abs(x1 - 40) <= 2 and abs(y0 - 10) <= 2 and abs(y1 - 30) <= 2  # JPEG blur
    # a point in the stored frame's coordinates would be out of range upright
    with pytest.raises(ValidationError) as e:
        segment(body(data, points=pts(10, 80)), B)
    assert e.value.param == "points[0].y"


def test_text_threshold_and_n(scene):
    img = png_b64(scene)
    segs = segment(body(img, prompt="thing"), B)["data"][0]["segments"]
    assert len(segs) == 2 and segs[0]["score"] >= segs[1]["score"]
    assert len(segment(body(img, prompt="thing", threshold=0.85), B)["data"][0]["segments"]) == 1
    assert len(segment(body(img, prompt="thing", n=1, threshold=0.0), B)["data"][0]["segments"]) == 1
    r = segment(body(img, prompt="thing", threshold=1.0), B)
    assert r["data"][0]["segments"] == [] and r["usage"] == {"images": 1, "masks": 0}


def test_box_exemplar_with_and_without_text(scene):
    for kw in ({}, {"prompt": "square"}):
        r = segment(body(png_b64(scene), boxes=[[58, 33, 92, 57]], **kw), B)
        assert r["data"][0]["segments"][0]["box"] == [60.0, 35.0, 90.0, 55.0]


def test_png_mask(scene):
    r = segment(body(png_b64(scene), points=pts(30, 20), response_format="png"), B)
    mk = r["data"][0]["segments"][0]["mask"]
    assert set(mk) == {"format", "b64_json"} and mk["format"] == "png"
    m = np.asarray(Image.open(io.BytesIO(base64.b64decode(mk["b64_json"]))))
    assert m.shape == (60, 100) and set(np.unique(m)) == {0, 255} and (m == 255).sum() == 400


def test_polygon_refused(scene):
    with pytest.raises(ValidationError) as e:
        segment(body(png_b64(scene), prompt="x", response_format="polygon"), B)
    assert e.value.param == "response_format"


def test_multi_image_per_image_prompts(scene):
    other = np.full((40, 80, 3), 255, np.uint8)
    other[5:15, 10:30] = (0, 255, 0)
    r = segment({"model": "m", "image": [png_b64(scene), png_b64(other)],
                 "boxes": [[[8, 8, 42, 32]], [[8, 3, 32, 17]]]}, B)
    d0, d1 = r["data"]
    assert (d0["index"], d0["width"], d0["height"]) == (0, 100, 60)
    assert (d1["index"], d1["width"], d1["height"]) == (1, 80, 40)
    assert d0["segments"][0]["box"] == [20.0, 10.0, 40.0, 30.0]
    assert d1["segments"][0]["box"] == [10.0, 5.0, 30.0, 15.0]
    assert r["usage"] == {"images": 2, "masks": 2}


def test_multi_image_shared_box_validated_per_image(scene):
    small = np.full((20, 30, 3), 255, np.uint8)
    with pytest.raises(ValidationError) as e:
        segment({"model": "m", "image": [png_b64(scene), png_b64(small)], "prompt": "x",
                 "boxes": [[1, 1, 50, 10]]}, B)
    assert e.value.param == "boxes[0][2]" and "image 1" in str(e.value)


def test_image_errors(scene):
    for img in ("!!!", base64.b64encode(b"nope").decode()):
        with pytest.raises(ValidationError) as e:
            segment(body(img, prompt="x"), B)
        assert e.value.param == "image"
    with pytest.raises(ValidationError) as e:
        segment(body([png_b64(scene), "!!!"], prompt="x"), B)
    assert e.value.param == "image[1]"
    big = Image.new("RGB", (4097, 8)); buf = io.BytesIO(); big.save(buf, "PNG")
    with pytest.raises(ValidationError) as e:
        segment(body(base64.b64encode(buf.getvalue()).decode(), prompt="x"), B)
    assert e.value.code == "image_too_large"


def test_validation_runs_before_inference(scene):
    class Counting(StubBackend):
        calls = 0

        def segment_concept(self, image, p):
            Counting.calls += 1
            return super().segment_concept(image, p)

    with pytest.raises(ValidationError):
        segment({"model": "m", "image": [png_b64(scene), png_b64(scene)], "prompt": "x",
                 "boxes": [[[1, 1, 5, 5]], [[1, 1, 500, 5]]]}, Counting())
    assert Counting.calls == 0


# ---- URL images -------------------------------------------------------------------------------

@pytest.fixture
def image_host(scene):
    buf = io.BytesIO(); Image.fromarray(scene).save(buf, "PNG")
    png = buf.getvalue()

    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/redir":
                self.send_response(302); self.send_header("Location", "/a.png"); self.end_headers()
            elif self.path == "/a.png":
                self.send_response(200); self.send_header("Content-Length", str(len(png))); self.end_headers()
                self.wfile.write(png)
            else:
                self.send_response(404); self.end_headers()

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def test_url_image(image_host):
    f = make_fetcher(allow_private=True)
    r = segment(body(image_host + "/a.png", points=pts(30, 20)), B, f)
    assert r["data"][0]["segments"][0]["area"] == 400
    assert segment(body([image_host + "/redir"], points=pts(30, 20)), B, f)["usage"]["masks"] == 1
    with pytest.raises(ValidationError) as e:
        segment(body(image_host + "/nope.png", points=pts(30, 20)), B, f)
    assert e.value.param == "image" and e.value.code == "image_fetch_failed"


def test_url_private_and_scheme_refused(image_host):
    with pytest.raises(ValidationError) as e:  # default fetcher: loopback is not public
        segment(body(image_host + "/a.png", points=pts(30, 20)), B)
    assert e.value.param == "image" and e.value.code == "image_url_forbidden"
    for u in ("file:///etc/passwd", "ftp://example.com/a.png"):
        with pytest.raises(ValidationError) as e:
            segment(body(u, points=pts(1, 1)), B)
        assert e.value.param == "image"


# ---- HTTP -------------------------------------------------------------------------------------

@pytest.fixture
def http_server():
    srv = make_server(B, "127.0.0.1", 0)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def call(url, payload=None, raw=None, ctype="application/json"):
    req = urllib.request.Request(url, data=raw if raw is not None else json.dumps(payload).encode(),
                                 headers={"Content-Type": ctype})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)


def multipart(fields, files=()):
    b = "XbOuNdArYx"
    out = b""
    for k, v in fields.items():
        out += f'--{b}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
    for k, name, data in files:
        out += (f'--{b}\r\nContent-Disposition: form-data; name="{k}"; filename="{name}"\r\n'
                f'Content-Type: image/png\r\n\r\n').encode() + data + b"\r\n"
    return out + f"--{b}--\r\n".encode(), f"multipart/form-data; boundary={b}"


def png_bytes(arr):
    buf = io.BytesIO(); Image.fromarray(arr).save(buf, "PNG"); return buf.getvalue()


def test_http_json(http_server, scene):
    with urllib.request.urlopen(http_server + "/health") as r:
        assert json.load(r)["status"] == "ok"
    s, j = call(http_server + ROUTE, body(png_b64(scene), points=pts(30, 20)))
    assert s == 200 and j["data"][0]["segments"][0]["mask"]["format"] == "rle" and j["usage"] == {"images": 1, "masks": 1}
    s, j = call(http_server + ROUTE, body(png_b64(scene), points=pts(300, 20)))
    assert s == 400 and j["error"]["param"] == "points[0].x" and j["error"]["type"] == "invalid_request_error"
    assert call(http_server + ROUTE, raw=b"{")[0] == 400
    assert call(http_server + "/nope", {})[0] == 404
    # the old shape is gone
    s, j = call(http_server + ROUTE, {"model": "m", "image": png_b64(scene), "prompts": [{"type": "concept", "text": "x"}]})
    assert s == 400 and j["error"]["param"] == "prompt"


def test_http_multipart(http_server, scene):
    other = np.full((40, 80, 3), 255, np.uint8)
    other[5:15, 10:30] = (0, 255, 0)
    raw, ct = multipart({"model": "m", "boxes": json.dumps([[[8, 8, 42, 32]], [[8, 3, 32, 17]]]),
                         "n": "5", "threshold": "0.5", "response_format": "png"},
                        [("image[]", "a.png", png_bytes(scene)), ("image[]", "b.png", png_bytes(other))])
    s, j = call(http_server + ROUTE, raw=raw, ctype=ct)
    assert s == 200, j
    assert [d["index"] for d in j["data"]] == [0, 1] and j["usage"] == {"images": 2, "masks": 2}
    assert j["data"][1]["segments"][0]["box"] == [10.0, 5.0, 30.0, 15.0]
    assert j["data"][0]["segments"][0]["mask"]["format"] == "png"
    # single `image` file part + text prompt
    raw, ct = multipart({"model": "m", "prompt": "thing"}, [("image", "a.png", png_bytes(scene))])
    s, j = call(http_server + ROUTE, raw=raw, ctype=ct)
    assert s == 200 and len(j["data"][0]["segments"]) == 2
    # a data-URL text field works too
    raw, ct = multipart({"model": "m", "prompt": "thing", "image": png_b64(scene)})
    assert call(http_server + ROUTE, raw=raw, ctype=ct)[0] == 200


def test_http_multipart_errors(http_server, scene):
    f = [("image", "a.png", png_bytes(scene))]
    raw, ct = multipart({"model": "m", "boxes": "[[1,1,500,5]]"}, f)
    s, j = call(http_server + ROUTE, raw=raw, ctype=ct)
    assert s == 400 and j["error"]["param"] == "boxes[0][2]"
    raw, ct = multipart({"model": "m", "prompt": "x", "boxes": "not json"}, f)
    assert call(http_server + ROUTE, raw=raw, ctype=ct)[1]["error"]["param"] == "boxes"
    raw, ct = multipart({"model": "m", "prompt": "x", "n": "abc"}, f)
    assert call(http_server + ROUTE, raw=raw, ctype=ct)[1]["error"]["param"] == "n"
    raw, ct = multipart({"model": "m", "prompt": "x"})
    assert call(http_server + ROUTE, raw=raw, ctype=ct)[1]["error"]["param"] == "image"
