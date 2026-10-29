# 睿购 ShopSage —— AI 导购智能体

> 面向真实电商交易场景的多模态 AI 导购系统：把大模型接入商品库、购物车、订单、优惠、评价、知识库、风控与质量测评，让 Agent 分步规划、检索、校验并完成导购闭环。
>
> 本项目在既有电商底座之上完成二次开发，重点增强 **Agent 任务规划与容错、上下文分层管理、导购任务规划流水线、工业级工程化与效果评估体系**。

[查看作品集页面](./portfolio.html)

---

## 一、项目简介

睿购 ShopSage 是一个「Agent-first」的电商导购系统，覆盖用户侧 AI 导购、真实商品召回、加购下单闭环、商家履约、管理员观测与质量测评全链路：

- **用户端**：Android 原生客户端 + Web 后台，支持文本导购、图片找同款、语音输入 / TTS 播报。
- **Agent 核心**：意图路由 → 动态工具编排（文本检索 / 图片向量检索 / 评价查询 / 购物车操作）→ 流式输出商品卡。
- **平台能力**：Prompt 统一落库与版本管理、链路追踪、质量测评、风控、商家与管理员后台。

## 二、二次开发亮点

相对原始电商底座，本次二次开发补齐了以下工程化能力：

| 模块 | 说明 |
|---|---|
| **Agent 任务规划与容错** | 引入 ReAct / Plan-and-Solve，实现任务拆分、子任务结果校验、工具调用重试与降级 |
| **上下文分层管理** | 拆分为任务状态、短期对话、用户长期记忆、外部检索信息四类独立存储，并落地用户偏好持久化 |
| **导购任务规划流水线** | TaskPlanner / TaskExecutor / ResultVerifier / TaskStateManager / TraceAdapter 五模块，支持「Plan → Verify & Execute → 断点恢复」，复杂导购请求自动进入流水线，简单 query 走原单轮 Agent |
| **链路追踪可视化** | 管理员后台新增「导购任务计划」视图，可视化解构每个子任务的规划、执行、校验与重试过程 |
| **效果评估体系** | 内置数据集离线评测 + 任务维度指标（任务完成率、子任务成功率、平均重试次数、中断恢复成功率、平均耗时） |
| **工业级工程化** | 前后端独立多阶段 Dockerfile、docker-compose 全栈编排、统一 Makefile、GitHub Actions CI |

### 任务规划流水线状态机

导购任务（`agent_task`）与子任务（`agent_sub_task`）的状态流转：

```text
用户 query ──> 任务初始化(INIT) ──> Plan(PLANNING)
                    │                    │
                    │           生成子任务清单(PENDING)
                    ▼                    ▼
              Verify & Execute        串行调度子任务
                    │              ┌────────┴────────┐
                    │          校验通过(SUCCESS)   校验失败(重试/FAILED)
                    ▼              └────────┬────────┘
          全部成功(COMPLETED)              降级/询问用户放宽条件
          中断后可断点恢复(PAUSED → RUNNING)
```

## 三、快速体验

### 演示账号

| 角色 | 账号 | 密码 | 主要体验内容 |
|---|---|---|---|
| 管理员 | `admin` | `admin123456` | 商品、订单、Prompt、配置、链路追踪、质量测评、风控 |
| 商家 | `merchant` | `merchant123456` | 商品管理、资料上传、订单查看、发货履约 |
| 用户 | `user` | `user123456` | Android 导购、加购、下单、图片找同款、语音/TTS |

推荐测试问题（可触发任务规划流水线）：

```text
我主要写代码和做演示，华为电脑和苹果电脑怎么选？
帮我找一双适合夏天通勤的运动鞋，不要太厚重
推荐一款适合敏感肌的面霜，预算 300 左右，对比两款评价，把最合适的加入购物车
把刚才推荐的第一个商品加入购物车
```

## 四、部署

### 一键 Docker 全栈（推荐）

```bash
# 仓库根目录
make docker-up
# 等价于
docker compose -f deployments/docker-compose.yml up -d --build
```

启动后：

| 服务 | 地址 | 说明 |
|---|---|---|
| 前端 Web 后台 | `http://localhost:8081` | 非 root nginx，反代 `/api` 到后端 |
| 后端 API | `http://localhost:8080` | 健康检查 `http://localhost:8080/api/v1/health` |

启用真实 AI 导购需在 `deployments/.env` 或当前 shell 设置模型密钥（如 `DASHSCOPE_API_KEY`）。`APP_ENV` 默认 `development`，自动迁移数据库并在无密钥时本地规则降级启动；上生产设 `APP_ENV=production`，后端会做生产配置 fail-fast 校验。

### 常用 Makefile 命令

```bash
make dev-backend      # 本地启动后端
make dev-frontend     # 本地启动前端（Vite proxy /api -> 8080）
make test-backend     # go test ./...
make test-frontend    # 前端类型检查
make build-backend    # 编译后端二进制
make build-frontend   # 构建前端 dist
make docker-build     # 构建前后端镜像
make docker-up        # 一键全栈
make docker-down      # 停止
make docker-logs      # 查看后端/前端日志
```

### 本地开发

```bash
# 1. 启动中间件（MySQL/Redis/Nacos/Milvus/MinIO）
docker compose -f deployments/docker-compose.yml up -d

# 2. 后端
cd backend
go run ./cmd/api        # http://localhost:8080

# 3. Web 后台
cd frontend
npm install
npm run dev -- --port 5173   # http://localhost:5173

# 4. Android 客户端
cd android-native
sh ./gradlew :app:assembleDebug
```

## 五、目录结构

```text
backend/           Go 后端（Agent/RAG/store/httpapi/configcenter/risk/ingest 等）
backend/src/extensions/  可选扩展层（试衣代理、追踪埋点、扩展评测，默认关闭）
services/fitlab/   独立试衣 HTTP 服务（FastAPI + Pillow/numpy，无模型权重依赖）
frontend/          React 19 + Vite + TS 管理/商家后台
android-native/    Java 原生 Android 客户端（本次未修改）
deployments/       docker-compose 全栈编排（含可选扩展叠加文件）
quality/           内置评测数据集与离线评估脚本
docs/、.ai/        设计文档、PRD、API 契约与技术方案
Makefile           统一开发/测试/构建入口
.github/workflows/ CI（go vet/test + 前端 lint/build）
```

## 六、效果评估

管理员后台「质量评测」页提供两类评估维度：

- **业务指标**：任务完成率、工具成功率、P95 时延、估算 Token/成本、加购转化率。
- **任务流水线指标**：导购任务完成率、子任务执行成功率、工具平均重试次数、任务中断恢复成功率、复杂请求平均耗时。

离线评估脚本复用线上诊断/分诊标准 schema，输出结构化报告（诊断命中率、分诊一致率、平均置信度）。

## 七、可选扩展能力（非侵入式）

在既有电商底座与 Agent 推理之上追加三项能力：**试衣 HTTP 工具、追踪埋点、扩展评测后台**。三者全部通过 `backend/src/extensions` 外层包装接入，不修改原有电商业务、Agent 推理与工具业务代码，Android 客户端零改动。

### 7.1 开关与默认行为

| 开关 | 默认值 | 说明 |
|---|---|---|
| `EXT_ENABLED` | `false` | 扩展总开关。关闭时 `extensions.Wrap` 原样返回 `server.Routes()`，不注册扩展路由、不启动埋点协程、不创建埋点表，行为与原始版本完全一致 |
| `EXT_FITLAB_ENABLED` | `true` | 试衣 HTTP 工具开关（总开关开启后生效） |
| `FITLAB_BASE_URL` | `http://fitlab:8000,http://127.0.0.1:8000` | 试衣服务候选地址，按顺序探测与回退 |
| `FITLAB_TIMEOUT_MS` | `8000` | 试衣代理超时，失败即降级 |
| `EXT_TELEMETRY_ENABLED` | `true` | 追踪埋点开关 |
| `EXT_TELEMETRY_WINDOW_HOURS` | `24` | 后台概览默认统计窗口 |
| `EXT_TELEMETRY_MAX_BODY_KB` | `64` | 前端埋点上报体上限 |
| `EXT_EVAL_DIR` | `quality/reports` | 扩展评测后台读取的报告目录 |
| `VITE_EXT_UI` | 未设置（关闭） | 前端「扩展能力」入口的构建开关 |

### 7.2 启用方式

```bash
# Docker：基础编排 + 可选扩展叠加文件（不改动原有服务定义）
make docker-up-ext
# 等价于
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.ext.yml up -d --build

# 本地后端
EXT_ENABLED=true FITLAB_BASE_URL=http://127.0.0.1:8000 go run ./cmd/api

# 本地前端（带扩展入口）
cd frontend && VITE_EXT_UI=true npm run dev

# 单独启动试衣服务
cd services/fitlab && pip install -r requirements.txt
uvicorn app.main:app --host 0.0.0.0 --port 8000
```

关闭扩展时直接使用 `docker compose -f deployments/docker-compose.yml up -d` 或不设置 `EXT_ENABLED` 启动后端即可。

### 7.3 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/v1/fitlab/tryon` | multipart：`person_image`、`garment_image`、`category`（auto/upper/lower/dress），成功返回 `image/png` |
| `GET` | `/api/v1/fitlab/health` | 试衣服务健康探测，公开接口，始终返回 200 JSON |
| `POST` | `/api/v1/ext/telemetry/events` | 前端埋点批量上报（≤50 条），异步批量落库 |
| `GET` | `/api/v1/admin/ext/overview` | 管理员：开关状态、试衣健康度、埋点统计、评测摘要 |
| `GET` | `/api/v1/admin/ext/eval/reports` | 管理员：评测报告列表，支持 `?kind=` 过滤 |
| `GET` | `/api/v1/admin/ext/eval/reports/detail?id=` | 管理员：查看原始评测报告 |

试衣服务自身还提供 `/healthz`、`/v1/tryon`、`/v1/tryon/base64`、`/v1/metrics`，便于独立部署与压测。

### 7.4 异常降级

- 试衣服务未开启、连接失败、超时、返回非图片或损坏图片时，后端统一返回 `503` JSON：`code=fitlab_unavailable|fitlab_disabled`、`degraded=true`，前端提示后继续走原有文字导购流程。
- 埋点采用非阻塞队列 + 5 秒批量刷盘，队列满时丢弃事件；建表或写库失败只记日志，不影响任何业务请求与 Agent 主链路。
- 扩展接口自行复刻原路由的 Bearer 鉴权、角色策略、CORS 白名单、请求体上限与限流，避免形成旁路。

### 7.5 验证

```bash
cd backend && go build ./... && go vet ./... && go test ./...
cd services/fitlab && python -m unittest discover -s tests -v
cd frontend && npm run lint && npm run build
```

## 八、免责声明

本项目的 AI 导购建议仅作辅助参考，实际购物决策请以商品详情、官方说明与个人实际需求为准。