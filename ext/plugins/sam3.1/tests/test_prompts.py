import pytest

from sam3shim.prompts import (ConceptPrompt, ValidationError, VisualPrompt, box_xyxy_to_cxcywh_norm,
                              parse_request, prompt_for_image, visual_to_sam1)

W, H = 100, 60


def req(**kw):
    return parse_request({"model": "m", "image": "a", "prompt": "x", **kw})


def pfi(r=None, i=0, w=W, h=H, **kw):
    return prompt_for_image(r or req(**kw), i, w, h)


def bad(param, fn=None, **kw):
    with pytest.raises(ValidationError) as e:
        (fn or (lambda: pfi(**kw)))()
    assert e.value.param == param
    return e.value


def test_point_valid_edges():
    p = pfi(prompt=None, points=[{"x": 0, "y": 60, "label": 1}, {"x": 100.0, "y": 0.5, "label": 0}])
    assert isinstance(p, VisualPrompt) and [pt.label for pt in p.points] == [1, 0]


@pytest.mark.parametrize("x,y,param", [(-0.1, 1, "x"), (100.5, 1, "x"), (1, -1, "y"), (1, 60.01, "y")])
def test_point_out_of_range(x, y, param):
    bad(f"points[0].{param}", prompt=None, points=[{"x": x, "y": y, "label": 1}])


@pytest.mark.parametrize("label", [2, -1, True, 1.0, "1", None])
def test_point_label_not_0_1(label):
    bad("points[0].label", prompt=None, points=[{"x": 1, "y": 1, "label": label}])


def test_point_missing_label_nan_bool():
    bad("points[0].label", prompt=None, points=[{"x": 1, "y": 1}])
    bad("points[0].x", prompt=None, points=[{"x": float("nan"), "y": 1, "label": 1}])
    bad("points[0].x", prompt=None, points=[{"x": True, "y": 1, "label": 1}])
    bad("points[0].z", prompt=None, points=[{"x": 1, "y": 1, "label": 1, "z": 2}])


def test_box_full_image_ok_and_concept():
    p = pfi(prompt=None, boxes=[[0, 0, 100, 60]])
    assert isinstance(p, ConceptPrompt) and p.text is None and p.boxes[0].box == (0, 0, 100, 60)


@pytest.mark.parametrize("box,param", [
    ([10, 0, 10, 5], "boxes[0]"),   # x0 == x1
    ([20, 0, 10, 5], "boxes[0]"),   # x0 > x1
    ([0, 9, 5, 3], "boxes[0]"),     # y0 > y1
    ([0, 0, 101, 5], "boxes[0][2]"),
    ([-1, 0, 5, 5], "boxes[0][0]"),
    ([0, 0, 5, 61], "boxes[0][3]"),
    ([0, 0, 5], "boxes[0]"),
])
def test_box_invalid(box, param):
    bad(param, boxes=[box])


def test_prompt_required_unless_points_or_boxes():
    bad("prompt", fn=lambda: parse_request({"model": "m", "image": "a"}))
    bad("prompt", fn=lambda: parse_request({"model": "m", "image": "a", "points": [], "boxes": []}))
    bad("prompt", fn=lambda: parse_request({"model": "m", "image": "a", "prompt": "  "}))
    parse_request({"model": "m", "image": "a", "boxes": [[1, 1, 5, 5]]})


def test_points_not_with_prompt_or_boxes():
    pt = [{"x": 1, "y": 1, "label": 1}]
    bad("points", points=pt)  # prompt "x" is set by req()
    bad("points", prompt=None, points=pt, boxes=[[1, 1, 5, 5]])


def test_per_image_shapes():
    r = parse_request({"model": "m", "image": ["a", "b"], "prompt": "x",
                       "boxes": [[[1, 1, 5, 5]], [[2, 2, 6, 6], [3, 3, 9, 9]]]})
    assert [len(g) for g in r.boxes] == [1, 2] and not r.boxes_shared
    # flat list applies to every image
    r = parse_request({"model": "m", "image": ["a", "b"], "boxes": [[1, 1, 5, 5]]})
    assert r.boxes == [[[1, 1, 5, 5]], [[1, 1, 5, 5]]] and r.boxes_shared
    # per-image points; [] = none for that image
    r = parse_request({"model": "m", "image": ["a", "b"],
                       "points": [[{"x": 1, "y": 1, "label": 1}], []], "prompt": "p"})
    assert [len(g) for g in r.points] == [1, 0]
    # single image: both forms
    assert parse_request({"model": "m", "image": "a", "boxes": [[[1, 1, 5, 5]]]}).boxes == [[[1, 1, 5, 5]]]
    assert parse_request({"model": "m", "image": "a", "boxes": [[1, 1, 5, 5]]}).boxes == [[[1, 1, 5, 5]]]


def test_per_image_errors():
    base = {"model": "m", "image": ["a", "b"], "prompt": "x"}
    bad("boxes", fn=lambda: parse_request({**base, "boxes": [[[1, 1, 5, 5]]]}))        # 1 group, 2 images
    bad("boxes", fn=lambda: parse_request({**base, "boxes": [[1, 1, 5, 5], [[1, 1, 5, 5]]]}))  # mixed
    bad("points", fn=lambda: parse_request({**base, "points": [[], [{"x": 1}], 3]}))
    bad("points", fn=lambda: parse_request({**base, "points": "x"}))
    bad("boxes[1]", fn=lambda: parse_request({**base, "boxes": [[], [[1, 1, 5, 5]] * 17]}))


def test_per_image_param_paths_and_own_size():
    r = parse_request({"model": "m", "image": ["a", "b"], "prompt": "x",
                       "boxes": [[[1, 1, 5, 5]], [[1, 1, 5, 500]]]})
    pfi(r, 0)
    e = bad("boxes[1][0][3]", fn=lambda: pfi(r, 1))
    assert "image 1" in str(e)
    # flat list is checked against each image's own size
    r = parse_request({"model": "m", "image": ["a", "b"], "prompt": "x", "boxes": [[1, 1, 150, 5]]})
    pfi(r, 0, w=200)
    bad("boxes[0][2]", fn=lambda: pfi(r, 1, w=100))
    # points per image
    r = parse_request({"model": "m", "image": ["a", "b"],
                       "points": [[{"x": 1, "y": 1, "label": 1}], [{"x": 1, "y": 1, "label": 1}, {"x": 999, "y": 1, "label": 1}]]})
    bad("points[1][1].x", fn=lambda: pfi(r, 1))


@pytest.mark.parametrize("body,param", [
    ({"image": "a", "prompt": "x"}, "model"),
    ({"model": "m", "prompt": "x"}, "image"),
    ({"model": "m", "image": "", "prompt": "x"}, "image"),
    ({"model": "m", "image": [], "prompt": "x"}, "image"),
    ({"model": "m", "image": ["a"] * 17, "prompt": "x"}, "image"),
    ({"model": "m", "image": ["a", 3], "prompt": "x"}, "image[1]"),
    ({"model": "m", "image": "a", "image[]": ["b"], "prompt": "x"}, "image[]"),
    ({"model": "m", "image": "a", "prompt": "x", "threshold": 1.5}, "threshold"),
    ({"model": "m", "image": "a", "prompt": "x", "n": 0}, "n"),
    ({"model": "m", "image": "a", "prompt": "x", "n": 1001}, "n"),
    ({"model": "m", "image": "a", "prompt": "x", "n": True}, "n"),
    ({"model": "m", "image": "a", "prompt": "x", "response_format": "boxes"}, "response_format"),
    ({"model": "m", "image": "a", "prompt": "x", "response_format": "polygon"}, "response_format"),
    ({"model": "m", "image": "a", "prompt": "x" * 257}, "prompt"),
])
def test_top_level(body, param):
    bad(param, fn=lambda: parse_request(body))


def test_image_array_key_and_defaults():
    r = parse_request({"model": "m", "image[]": ["a", "b"], "prompt": "x"})
    assert r.images == ["a", "b"] and r.image_list and (r.n, r.threshold, r.response_format) == (100, 0.5, "rle")


def test_conversions():
    # box [10,20,30,60] in 100x200 -> centre (20,40) -> (0.2, 0.2), size 20x40 -> (0.2, 0.2)
    assert box_xyxy_to_cxcywh_norm((10, 20, 30, 60), 100, 200) == pytest.approx([0.2, 0.2, 0.2, 0.2])
    p = VisualPrompt([pfi(prompt=None, points=[{"x": 5.5, "y": 6, "label": 0}]).points[0]], (1, 2, 3, 4), False)
    assert visual_to_sam1(p) == ([[5.5, 6.0]], [0], [1, 2, 3, 4])  # pixels pass through unchanged
