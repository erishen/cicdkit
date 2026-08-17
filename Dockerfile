# 多阶段构建：前端(Vite) -> golang 编译(嵌入 dist) -> 轻量运行时镜像
# 运行时包含 docker CLI 与 kubectl，用于构建镜像并向 k3s 发布。
FROM node:20-alpine AS web
WORKDIR /web
COPY cmd/server/web/package.json ./
RUN npm install --registry=https://registry.npmmirror.com || npm install
COPY cmd/server/web/ ./
RUN npm run build

FROM golang:1.22-alpine AS builder
WORKDIR /src
# 仅依赖标准库，无需 go mod download；拷贝即编译（会把 web/dist 一并嵌入）
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl docker-cli \
    && curl -fsSLo /usr/local/bin/kubectl \
       "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl
WORKDIR /app
COPY --from=builder /out/server /app/server
# 运行时数据（store.json）挂到命名卷；docker 构建通过挂载宿主 /var/run/docker.sock 复用宿主 daemon
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/server"]
CMD ["-addr", ":8080", "-config", "/app/data/config.json"]
