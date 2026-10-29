# ShopSage 工业结构包装：本地开发、测试、构建与 Docker 一键入口。
# 依赖：Go 1.24+、Node.js 22+、Docker Compose。
.PHONY: help dev-backend dev-frontend test-backend test-frontend build-backend build-frontend docker-build docker-up docker-down docker-logs docker-up-ext docker-down-ext

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

dev-backend: ## 启动后端（默认 http://localhost:8080）
	cd backend && go run ./cmd/api

dev-frontend: ## 启动前端开发服务器（/api 已代理到 8080）
	cd frontend && npm install && npm run dev

test-backend: ## 运行后端单元测试
	cd backend && go test ./...

test-frontend: ## 运行前端类型检查
	cd frontend && npm run lint

build-backend: ## 编译后端二进制到 backend/bin/api
	cd backend && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api

build-frontend: ## 构建前端静态资源到 frontend/dist
	cd frontend && npm install && npm run build

docker-build: ## 构建前后端镜像
	docker compose -f deployments/docker-compose.yml build backend frontend

docker-up: ## 一键启动全栈（中间件 + 后端 + 前端）
	docker compose -f deployments/docker-compose.yml up -d

docker-down: ## 停止并移除全栈容器
	docker compose -f deployments/docker-compose.yml down

docker-logs: ## 查看后端/前端日志
	docker compose -f deployments/docker-compose.yml logs -f backend frontend

docker-up-ext: ## 启动全栈并开启扩展能力（试衣服务 + 埋点 + 扩展评测）
	docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.ext.yml up -d --build

docker-down-ext: ## 停止扩展全栈
	docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.ext.yml down