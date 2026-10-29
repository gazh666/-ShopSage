# FitLab 试衣服务

独立 HTTP 微服务，为 ShopSage 提供可选的虚拟试衣能力。服务本身不访问 ShopSage 的
MySQL / Redis / Milvus，也不参与 Agent 推理，只接收「人物图 + 服装图」并返回合成图。

## 引擎

- `geometry-v1`：确定性几何贴图引擎，只依赖 Pillow + numpy，无模型权重、无 GPU、无外网下载。
- 流程：背景色估计 → 服装前景抠图与羽化 → 按品类对齐到人体区域 → 光照协调 → PNG 输出。
- 适合作为可插拔演示能力；若后续接入 CatVTON / OOTDiffusion 等模型，只需替换 `app/engine.py`，
  HTTP 契约与后端代理无需改动。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查，返回 `status/engine/uptime` |
| POST | `/v1/tryon` | multipart：`person_image`、`garment_image`、`category=auto|upper|lower|dress`，返回 `image/png` |
| POST | `/v1/tryon/base64` | JSON base64 版本，返回 `image_base64` 与 `meta` |
| GET | `/v1/metrics` | 进程内请求数、成功/失败数、平均耗时 |

## 本地运行

```bash
cd services/fitlab
pip install -r requirements.txt
uvicorn app.main:app --host 0.0.0.0 --port 8000
python -m unittest discover -s tests -v
```

## 配置

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `FITLAB_MAX_UPLOAD_BYTES` | `6291456`（6MB） | 单张图片上限 |
| `FITLAB_MAX_SIDE` | `1024` | 处理前的最长边缩放上限 |
| `FITLAB_SERVICE_NAME` | `fitlab` | 健康检查中显示的服务名 |

## 与 ShopSage 的集成

后端通过 `EXT_FITLAB_ENABLED` 与 `FITLAB_BASE_URL` 代理调用本服务：

```text
前端 -> /api/v1/fitlab/tryon -> extensions 代理 -> FitLab /v1/tryon
```

FitLab 未启动、超时或返回非图片时，代理层返回 `503` + `degraded=true`，
前端继续走原有文字导购流程，不影响 Agent 主链路。
