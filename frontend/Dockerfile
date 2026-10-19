# xzxg-shop 前端多阶段构建：Node 构建静态资源，非 root nginx 提供静态文件并反代后端 API。
FROM node:22-alpine AS build

WORKDIR /app

# 先复制依赖清单，利用层缓存（项目当前无 lockfile，使用 npm install）。
COPY package.json ./
RUN npm install

# 复制源码并构建生产包（tsc 类型检查 + vite build）。
COPY . .
RUN npm run build

# 运行阶段：非 root nginx，监听 8080 规避 80 端口特权。
FROM nginxinc/nginx-unprivileged:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1

CMD ["nginx", "-g", "daemon off;"]