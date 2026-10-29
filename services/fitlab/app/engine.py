"""FitLab 试衣服务的确定性几何贴图引擎。

该引擎不依赖任何模型权重，只用 Pillow + numpy 做前景抠图、区域对齐和光照协调，
保证在无 GPU、无外网模型下载的环境里也能稳定给出结果。
"""

from __future__ import annotations

import io
import time
from dataclasses import dataclass, field
from typing import Dict, Tuple

import numpy as np
from PIL import Image, ImageFilter, ImageOps

ENGINE_NAME = "geometry-v1"
SUPPORTED_CATEGORIES = ("auto", "upper", "lower", "dress")

# 每个品类的目标区域：(left, top, width, height)，均为相对人体图的比例。
_REGIONS: Dict[str, Tuple[float, float, float, float]] = {
    "upper": (0.18, 0.16, 0.64, 0.42),
    "lower": (0.24, 0.46, 0.52, 0.42),
    "dress": (0.18, 0.16, 0.64, 0.74),
}

# 不同品类的垂直对齐偏好：0 顶部对齐，1 底部对齐。
_ALIGN: Dict[str, float] = {
    "upper": 0.15,
    "lower": 0.65,
    "dress": 0.2,
}


class FitlabError(ValueError):
    """输入不合法时抛出，由 HTTP 层转换为 400。"""


@dataclass
class TryOnResult:
    image: Image.Image
    meta: Dict[str, float] = field(default_factory=dict)


def normalize_category(value: str) -> str:
    value = (value or "auto").strip().lower()
    if value not in SUPPORTED_CATEGORIES:
        raise FitlabError("category 仅支持 auto/upper/lower/dress")
    return "upper" if value == "auto" else value


def _decode_image(data: bytes) -> Image.Image:
    if not data:
        raise FitlabError("图片内容为空")
    try:
        image = Image.open(io.BytesIO(data))
        image.load()
    except Exception as exc:  # pragma: no cover - Pillow 抛错类型不稳定
        raise FitlabError("图片格式无法识别") from exc
    image = ImageOps.exif_transpose(image).convert("RGB")
    if image.width < 16 or image.height < 16:
        raise FitlabError("图片尺寸过小")
    return image


def _limited_image(image: Image.Image, max_side: int) -> Image.Image:
    if max(image.size) <= max_side:
        return image
    copy = image.copy()
    copy.thumbnail((max_side, max_side), Image.LANCZOS)
    return copy


def _foreground_mask(image: Image.Image) -> Tuple[np.ndarray, float]:
    """用四周边框的中位色估计背景，按颜色距离抠出服装前景。"""
    array = np.asarray(image).astype(np.float32)
    height, width, _ = array.shape
    border = np.concatenate(
        [array[0, :, :], array[-1, :, :], array[:, 0, :], array[:, -1, :]], axis=0
    )
    background = np.median(border, axis=0)
    distance = np.sqrt(((array - background) ** 2).sum(axis=2))
    if float(distance.max()) <= 1.0:
        # 纯色图无法区分背景，退化为整图贴图。
        return np.ones((height, width), dtype=bool), 1.0
    threshold = max(30.0, float(np.percentile(distance, 75)) * 0.55)
    mask = distance >= threshold
    cleaned = Image.fromarray((mask.astype(np.uint8) * 255), "L")
    cleaned = cleaned.filter(ImageFilter.MaxFilter(3)).filter(ImageFilter.MinFilter(3))
    mask = np.asarray(cleaned) > 0
    coverage = float(mask.mean())
    if coverage < 0.10:
        # 浅色服装叠白底时颜色距离不足，用整图并靠边缘羽化弱化背景。
        mask = np.ones((height, width), dtype=bool)
        coverage = 1.0
    return mask, coverage


def _crop_mask(mask: np.ndarray) -> Tuple[int, int, int, int]:
    rows = np.any(mask, axis=1)
    cols = np.any(mask, axis=0)
    if not rows.any() or not cols.any():
        return 0, 0, mask.shape[1], mask.shape[0]
    y0, y1 = np.where(rows)[0][[0, -1]]
    x0, x1 = np.where(cols)[0][[0, -1]]
    height, width = mask.shape
    pad_x = max(1, int(width * 0.01))
    pad_y = max(1, int(height * 0.01))
    return (
        max(0, int(x0) - pad_x),
        max(0, int(y0) - pad_y),
        min(width, int(x1) + pad_x + 1),
        min(height, int(y1) + pad_y + 1),
    )


def _alpha_from_mask(mask: np.ndarray) -> Image.Image:
    alpha = Image.fromarray((mask.astype(np.uint8) * 255), "L")
    alpha = alpha.filter(ImageFilter.GaussianBlur(1.0))
    alpha = alpha.point(lambda value: 0 if value < 26 else min(255, int(value * 1.12)))
    return alpha


def try_on(
    person_bytes: bytes,
    garment_bytes: bytes,
    category: str = "auto",
    max_side: int = 1024,
) -> TryOnResult:
    started = time.perf_counter()
    person = _limited_image(_decode_image(person_bytes), max_side)
    garment = _limited_image(_decode_image(garment_bytes), max_side)
    normalized = normalize_category(category)

    mask, coverage = _foreground_mask(garment)
    garment = garment.crop(_crop_mask(mask))
    alpha_source = _alpha_from_mask(mask).crop(_crop_mask(mask))

    person_width, person_height = person.size
    left_ratio, top_ratio, width_ratio, height_ratio = _REGIONS[normalized]
    target_left = int(person_width * left_ratio)
    target_top = int(person_height * top_ratio)
    target_width = max(1, int(person_width * width_ratio))
    target_height = max(1, int(person_height * height_ratio))

    garment_width, garment_height = garment.size
    scale = min(target_width / garment_width, target_height / garment_height)
    draw_width = max(1, int(garment_width * scale))
    draw_height = max(1, int(garment_height * scale))
    garment = garment.resize((draw_width, draw_height), Image.LANCZOS)
    alpha = alpha_source.resize((draw_width, draw_height), Image.LANCZOS)

    offset_x = target_left + max(0, (target_width - draw_width) // 2)
    offset_y = target_top + int(max(0, target_height - draw_height) * _ALIGN[normalized])

    person_array = np.asarray(person).astype(np.float32)
    garment_array = np.asarray(garment).astype(np.float32)
    alpha_array = (np.asarray(alpha).astype(np.float32) / 255.0)[:, :, None] * 0.96

    # 用人体区域亮度做简单光照协调，避免衣服比人物明显偏亮。
    region = person_array[offset_y : offset_y + draw_height, offset_x : offset_x + draw_width]
    if region.size:
        luminance = float(np.mean(region.mean(axis=2))) / 255.0
    else:
        luminance = 0.7
    shade = min(1.02, max(0.72, 0.68 + 0.38 * luminance))
    garment_array = np.clip(garment_array * shade, 0, 255)

    output = person_array.copy()
    target = output[offset_y : offset_y + draw_height, offset_x : offset_x + draw_width]
    if target.shape[:2] == alpha_array.shape[:2]:
        output[offset_y : offset_y + draw_height, offset_x : offset_x + draw_width] = (
            target * (1.0 - alpha_array) + garment_array * alpha_array
        )

    result = Image.fromarray(np.clip(output, 0, 255).astype(np.uint8), "RGB")
    duration_ms = (time.perf_counter() - started) * 1000.0
    meta = {
        "engine": ENGINE_NAME,
        "category": normalized,
        "person_width": person_width,
        "person_height": person_height,
        "garment_coverage": round(coverage, 4),
        "alpha_mean": round(float(alpha_array.mean()), 4),
        "shade": round(shade, 4),
        "duration_ms": round(duration_ms, 2),
    }
    return TryOnResult(image=result, meta=meta)


def encode_png(image: Image.Image) -> bytes:
    buffer = io.BytesIO()
    image.save(buffer, format="PNG", optimize=True)
    return buffer.getvalue()
