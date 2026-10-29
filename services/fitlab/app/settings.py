"""试衣服务运行参数。"""

from __future__ import annotations

import os


def env_int(key: str, fallback: int) -> int:
    value = os.getenv(key, "").strip()
    if not value:
        return fallback
    try:
        return int(value)
    except ValueError:
        return fallback


MAX_UPLOAD_BYTES = env_int("FITLAB_MAX_UPLOAD_BYTES", 6 * 1024 * 1024)
MAX_SIDE = env_int("FITLAB_MAX_SIDE", 1024)
SERVICE_NAME = os.getenv("FITLAB_SERVICE_NAME", "fitlab")
