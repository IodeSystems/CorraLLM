import base64
import io

import numpy as np
import pytest
from PIL import Image


def png_b64(arr: np.ndarray) -> str:
    buf = io.BytesIO()
    Image.fromarray(arr).save(buf, "PNG")
    return "data:image/png;base64," + base64.b64encode(buf.getvalue()).decode()


@pytest.fixture
def scene():
    """100 wide x 60 high, white; red square x20..40 y10..30; blue rect x60..90 y35..55."""
    a = np.full((60, 100, 3), 255, np.uint8)
    a[10:30, 20:40] = (255, 0, 0)
    a[35:55, 60:90] = (0, 0, 255)
    return a
