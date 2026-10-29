"""FitLab 引擎单元测试（仅依赖 Pillow/numpy）。"""

from __future__ import annotations

import io
import pathlib
import sys
import unittest

from PIL import Image, ImageDraw

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from app.engine import FitlabError, encode_png, normalize_category, try_on  # noqa: E402


def person_image(width: int = 360, height: int = 540) -> Image.Image:
    image = Image.new("RGB", (width, height), (214, 226, 238))
    draw = ImageDraw.Draw(image)
    draw.ellipse((width * 0.34, height * 0.06, width * 0.66, height * 0.28), fill=(232, 196, 168))
    draw.rectangle((width * 0.26, height * 0.28, width * 0.74, height * 0.92), fill=(52, 68, 96))
    return image


def garment_image(width: int = 240, height: int = 300) -> Image.Image:
    image = Image.new("RGB", (width, height), (255, 255, 255))
    draw = ImageDraw.Draw(image)
    draw.rectangle((24, 18, width - 24, height - 18), fill=(196, 42, 58))
    draw.rectangle((width * 0.42, 40, width * 0.58, height - 40), fill=(236, 214, 188))
    return image


def png_bytes(image: Image.Image) -> bytes:
    buffer = io.BytesIO()
    image.save(buffer, format="PNG")
    return buffer.getvalue()


class EngineTest(unittest.TestCase):
    def test_try_on_preserves_person_size(self) -> None:
        result = try_on(png_bytes(person_image()), png_bytes(garment_image()), category="upper")
        self.assertEqual(result.image.size, (360, 540))
        self.assertEqual(result.image.mode, "RGB")
        self.assertEqual(result.meta["engine"], "geometry-v1")
        self.assertEqual(result.meta["category"], "upper")
        self.assertGreater(result.meta["garment_coverage"], 0.0)
        self.assertTrue(encode_png(result.image).startswith(b"\x89PNG"))

    def test_try_on_is_deterministic(self) -> None:
        person = png_bytes(person_image())
        garment = png_bytes(garment_image())
        first = encode_png(try_on(person, garment, category="dress").image)
        second = encode_png(try_on(person, garment, category="dress").image)
        self.assertEqual(first, second)

    def test_category_normalization(self) -> None:
        self.assertEqual(normalize_category("auto"), "upper")
        self.assertEqual(normalize_category("LOWER"), "lower")
        with self.assertRaises(FitlabError):
            normalize_category("shoes")

    def test_invalid_image_rejected(self) -> None:
        with self.assertRaises(FitlabError):
            try_on(b"not-an-image", png_bytes(garment_image()))
        with self.assertRaises(FitlabError):
            try_on(png_bytes(person_image()), b"")


if __name__ == "__main__":
    unittest.main()
