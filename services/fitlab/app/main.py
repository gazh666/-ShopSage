"""FitLab 试衣微服务入口。

只提供独立 HTTP 能力，不读写 ShopSage 的数据库和消息队列，
由后端 extensions 层按需代理调用，调用失败时由后端降级。
"""

from __future__ import annotations

import base64
import binascii
import threading
import time
from typing import Any, Dict, Optional

from fastapi import FastAPI, File, Form, UploadFile
from fastapi.responses import JSONResponse, Response
from pydantic import BaseModel

from .engine import ENGINE_NAME, FitlabError, encode_png, try_on
from .settings import MAX_SIDE, MAX_UPLOAD_BYTES, SERVICE_NAME

app = FastAPI(title="FitLab Try-On Service", version="1.0.0")

_STARTED_AT = time.time()
_METRICS_LOCK = threading.Lock()
_METRICS: Dict[str, Any] = {
    "requests": 0,
    "successes": 0,
    "failures": 0,
    "total_ms": 0.0,
}


def _record(success: bool, duration_ms: float) -> None:
    with _METRICS_LOCK:
        _METRICS["requests"] += 1
        _METRICS["total_ms"] += duration_ms
        if success:
            _METRICS["successes"] += 1
        else:
            _METRICS["failures"] += 1


def _validate_size(label: str, payload: bytes) -> None:
    if not payload:
        raise FitlabError("%s 图片为空" % label)
    if len(payload) > MAX_UPLOAD_BYTES:
        raise FitlabError("%s 图片超过大小限制" % label)


def _run(person: bytes, garment: bytes, category: str) -> Any:
    started = time.perf_counter()
    try:
        _validate_size("person_image", person)
        _validate_size("garment_image", garment)
        result = try_on(person, garment, category=category, max_side=MAX_SIDE)
    except Exception:
        _record(False, (time.perf_counter() - started) * 1000.0)
        raise
    _record(True, (time.perf_counter() - started) * 1000.0)
    return result


@app.exception_handler(FitlabError)
async def handle_fitlab_error(_request, exc: FitlabError) -> JSONResponse:
    return JSONResponse(status_code=400, content={"code": "invalid_input", "message": str(exc)})


@app.exception_handler(Exception)
async def handle_unexpected_error(_request, exc: Exception) -> JSONResponse:
    return JSONResponse(
        status_code=500,
        content={"code": "internal_error", "message": "试衣服务内部错误"},
    )


@app.get("/healthz")
async def healthz() -> Dict[str, Any]:
    with _METRICS_LOCK:
        snapshot = dict(_METRICS)
    return {
        "status": "ok",
        "service": SERVICE_NAME,
        "engine": ENGINE_NAME,
        "uptime_seconds": round(time.time() - _STARTED_AT, 1),
        "requests": snapshot["requests"],
    }


@app.get("/v1/metrics")
async def metrics() -> Dict[str, Any]:
    with _METRICS_LOCK:
        snapshot = dict(_METRICS)
    average = snapshot["total_ms"] / snapshot["requests"] if snapshot["requests"] else 0.0
    return {
        "service": SERVICE_NAME,
        "engine": ENGINE_NAME,
        "requests": snapshot["requests"],
        "successes": snapshot["successes"],
        "failures": snapshot["failures"],
        "average_ms": round(average, 2),
        "uptime_seconds": round(time.time() - _STARTED_AT, 1),
    }


@app.post("/v1/tryon")
async def tryon(
    person_image: UploadFile = File(...),
    garment_image: UploadFile = File(...),
    category: str = Form("auto"),
) -> Response:
    person = await person_image.read()
    garment = await garment_image.read()
    result = _run(person, garment, category)
    return Response(
        content=encode_png(result.image),
        media_type="image/png",
        headers={
            "X-Fitlab-Engine": ENGINE_NAME,
            "X-Fitlab-Category": str(result.meta.get("category", "upper")),
            "X-Fitlab-Duration-Ms": str(result.meta.get("duration_ms", 0)),
            "Cache-Control": "no-store",
        },
    )


class Base64TryOnRequest(BaseModel):
    person_image_base64: str
    garment_image_base64: str
    category: str = "auto"


@app.post("/v1/tryon/base64")
async def tryon_base64(request: Base64TryOnRequest) -> Dict[str, Any]:
    try:
        person = base64.b64decode(request.person_image_base64, validate=True)
        garment = base64.b64decode(request.garment_image_base64, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise FitlabError("base64 图片解码失败") from exc
    result = _run(person, garment, request.category)
    return {
        "image_base64": base64.b64encode(encode_png(result.image)).decode("ascii"),
        "content_type": "image/png",
        "meta": result.meta,
    }
