"""FitLab HTTP 接口测试（fastapi TestClient）。"""

from __future__ import annotations

import base64
import io
import pathlib
import sys
import unittest

from PIL import Image, ImageDraw

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from fastapi.testclient import TestClient  # noqa: E402

from app.main import app  # noqa: E402


def build_image(color, size=(240, 360)) -> Image.Image:
    image = Image.new("RGB", size, (240, 240, 240))
    draw = ImageDraw.Draw(image)
    draw.rectangle((24, 24, size[0] - 24, size[1] - 24), fill=color)
    return image


def png_bytes(image: Image.Image) -> bytes:
    buffer = io.BytesIO()
    image.save(buffer, format="PNG")
    return buffer.getvalue()


class ApiTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.client = TestClient(app)
        cls.person = png_bytes(build_image((210, 190, 170)))
        cls.garment = png_bytes(build_image((40, 90, 200)))

    def test_healthz(self) -> None:
        response = self.client.get("/healthz")
        self.assertEqual(response.status_code, 200)
        payload = response.json()
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["engine"], "geometry-v1")

    def test_tryon_multipart(self) -> None:
        response = self.client.post(
            "/v1/tryon",
            data={"category": "upper"},
            files={
                "person_image": ("person.png", self.person, "image/png"),
                "garment_image": ("garment.png", self.garment, "image/png"),
            },
        )
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.headers["content-type"], "image/png")
        self.assertTrue(response.content.startswith(b"\x89PNG"))

    def test_tryon_base64(self) -> None:
        response = self.client.post(
            "/v1/tryon/base64",
            json={
                "person_image_base64": base64.b64encode(self.person).decode("ascii"),
                "garment_image_base64": base64.b64encode(self.garment).decode("ascii"),
                "category": "lower",
            },
        )
        self.assertEqual(response.status_code, 200)
        payload = response.json()
        self.assertEqual(payload["content_type"], "image/png")
        self.assertEqual(payload["meta"]["category"], "lower")
        decoded = base64.b64decode(payload["image_base64"])
        self.assertTrue(decoded.startswith(b"\x89PNG"))

    def test_invalid_base64(self) -> None:
        response = self.client.post(
            "/v1/tryon/base64",
            json={
                "person_image_base64": "not base64!!!",
                "garment_image_base64": base64.b64encode(self.garment).decode("ascii"),
            },
        )
        self.assertEqual(response.status_code, 400)
        self.assertEqual(response.json()["code"], "invalid_input")

    def test_metrics(self) -> None:
        self.client.post(
            "/v1/tryon",
            files={
                "person_image": ("person.png", self.person, "image/png"),
                "garment_image": ("garment.png", self.garment, "image/png"),
            },
        )
        response = self.client.get("/v1/metrics")
        self.assertEqual(response.status_code, 200)
        payload = response.json()
        self.assertEqual(payload["service"], "fitlab")
        self.assertGreaterEqual(payload["requests"], 1)


if __name__ == "__main__":
    unittest.main()
