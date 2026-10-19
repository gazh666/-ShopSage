# ShopSage 后端多阶段构建：编译阶段用完整 Go 工具链，运行阶段用最小镜像 + 非 root 用户。
FROM golang:1.24-alpine AS build

WORKDIR /src

# 先复制依赖清单，利用 Docker 层缓存加速 go mod download。
COPY go.mod go.sum ./
RUN go mod download

# 复制全部源码，编译 linux 静态二进制（项目纯 Go 依赖，可关闭 CGO）。
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM alpine:3.20

# 创建非 root 用户运行服务，降低容器逃逸风险。
RUN addgroup -S app && adduser -S app -G app

COPY --from=build /out/api /usr/local/bin/api

USER app
EXPOSE 8080

# 健康检查走自带 /api/v1/health 端点（busybox 自带 wget）。
HEALTHCHECK --interval=30s --timeout=3s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/api/v1/health || exit 1

ENTRYPOINT ["/usr/local/bin/api"]