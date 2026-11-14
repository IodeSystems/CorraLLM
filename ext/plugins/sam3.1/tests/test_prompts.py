import pytest

from sam3shim.prompts import (ValidationError, box_xyxy_to_cxcywh_norm, parse_request,
                              validate_prompts, visual_to_sam1)

W, H = 100, 60


def val(p):
    return validate_prompts([p], W, H)


def bad(p, param):
    with pytest.raises(ValidationError) as e:
        val(p)
    assert e.value.param == param


def test_point_valid_edges():
    p = val({"type": "visual", "points": [{"x": 0, "y": 60, "label": 1}, {"x": 100.0, "y": 0.5, "label": 0}]})[0]
    assert [pt.label for pt in p.points] == [1, 0]


@pytest.mark.parametrize("x,y,param", [(-0.1, 1, "x"), (100.5, 1, "x"), (1, -1, "y"), (1, 60.01, "y")])
def test_point_out_of_range(x, y, param):
    bad({"type": "visual", "points": [{"x": x, "y": y, "label": 1}]}, f"prompts[0].points[0].{param}")


@pytest.mark.parametrize("label", [2, -1, True, 1.0, "1", None])
def test_point_label_not_0_1(label):
    bad({"type": "visual", "points": [{"x": 1, "y": 1, "label": label}]}, "prompts[0].points[0].label")


def test_point_nan_and_bool_coord():
    bad({"type": "visual", "points": [{"x": float("nan"), "y": 1, "label": 1}]}, "prompts[0].points[0].x")
    bad({"type": "visual", "points": [{"x": True, "y": 1, "label": 1}]}, "prompts[0].points[0].x")


def test_box_full_image_ok():
    assert val({"type": "visual", "box": [0, 0, 100, 60]})[0].box == (0, 0, 100, 60)


@pytest.mark.parametrize("box,param", [
    ([10, 0, 10, 5], "prompts[0].box"),   # x0 == x1
    ([20, 0, 10, 5], "prompts[0].box"),   # x0 > x1
    ([0, 9, 5, 3], "prompts[0].box"),     # y0 > y1
    ([0, 0, 101, 5], "prompts[0].box[2]"),
    ([-1, 0, 5, 5], "prompts[0].box[0]"),
    ([0, 0, 5, 61], "prompts[0].box[3]"),
    ([0, 0, 5], "prompts[0].box"),
])
def test_box_invalid(box, param):
    bad({"type": "visual", "box": box}, param)


def test_visual_needs_point_or_box():
    bad({"type": "visual"}, "prompts[0]")
    bad({"type": "visual", "points": []}, "prompts[0]")


def test_points_in_concept_refused():
    bad({"type": "concept", "text": "x", "points": [{"x": 1, "y": 1, "label": 1}]}, "prompts[0].points")


def test_concept_needs_text_or_boxes():
    bad({"type": "concept"}, "prompts[0]")
    assert val({"type": "concept", "boxes": [{"box": [1, 1, 5, 5], "label": 0}]})[0].boxes[0].label == 0
    bad({"type": "concept", "boxes": [{"box": [1, 1, 5, 5], "label": 3}]}, "prompts[0].boxes[0].label")
    bad({"type": "concept", "boxes": [{"box": [1, 1, 500, 5]}]}, "prompts[0].boxes[0].box[2]")


def test_unknown_type_and_param_path_index():
    bad({"type": "x"}, "prompts[0].type")
    with pytest.raises(ValidationError) as e:
        validate_prompts([{"type": "concept", "text": "a"}, {"type": "visual", "points": [{"x": 1, "y": 1, "label": 1}, {"x": 999, "y": 1, "label": 1}]}], W, H)
    assert e.value.param == "prompts[1].points[1].x"


@pytest.mark.parametrize("body,param", [
    ({"image": "a", "prompts": [{}]}, "model"),
    ({"model": "m", "prompts": [{}]}, "image"),
    ({"model": "m", "image": "a", "prompts": []}, "prompts"),
    ({"model": "m", "image": "a", "prompts": [{}] * 17}, "prompts"),
    ({"model": "m", "image": "a", "prompts": [{}], "threshold": 1.5}, "threshold"),
    ({"model": "m", "image": "a", "prompts": [{}], "max_masks": 0}, "max_masks"),
    ({"model": "m", "image": "a", "prompts": [{}], "max_masks": 1001}, "max_masks"),
    ({"model": "m", "image": "a", "prompts": [{}], "output": "png"}, "output"),
])
def test_top_level(body, param):
    with pytest.raises(ValidationError) as e:
        parse_request(body)
    assert e.value.param == param


def test_conversions():
    # box [10,20,30,60] in 100x200 -> centre (20,40) -> (0.2, 0.2), size 20x40 -> (0.2, 0.2)
    assert box_xyxy_to_cxcywh_norm((10, 20, 30, 60), 100, 200) == pytest.approx([0.2, 0.2, 0.2, 0.2])
    p = val({"type": "visual", "points": [{"x": 5.5, "y": 6, "label": 0}], "box": [1, 2, 3, 4]})[0]
    assert visual_to_sam1(p) == ([[5.5, 6.0]], [0], [1, 2, 3, 4])  # pixels pass through unchanged
