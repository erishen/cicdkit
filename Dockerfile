# 多阶段构建：前端(Vite) -> golang 编译(嵌入 dist) -> 轻量运行时镜像
# 运行时包含 docker CLI 与 kubectl，用于构建镜像并向 k3s 发布。
FROM node:24-alpine AS web
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
# main.go 的 go:embed all:web/dist 需要 dist；宿主的 dist 已被 .dockerignore
# 排除（构建产物不进上下文），必须显式从 web 阶段接上，否则 go build 报
# "pattern all:web/dist: no matching files found"。
COPY --from=web /web/dist ./cmd/server/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
ARG TARGETARCH
# git: 构建镜像时 tag_strategy=git-sha 需在构建上下文(宿主挂载的工作区)取
# commit sha，容器无 git 会退化为时间戳 tag，降低可追溯性。
RUN apk add --no-cache ca-certificates curl git docker-cli docker-cli-buildx openssh-client
# kubectl 由宿主机预取到仓库根（构建容器出口网络拉 dl.k8s.io / google storage 不稳定，
# 容器内 25s 仅下到 7.8/54MB 超时），此处直接 COPY 进镜像。当前为 linux/arm64；
# 若在 amd64 机器构建，请预取对应架构二进制并改下面的文件名。
COPY kubectl-linux-arm64 /usr/local/bin/kubectl
RUN chmod +x /usr/local/bin/kubectl
WORKDIR /app
COPY --from=builder /out/server /app/server
# 容器入口：启动前把宿主 kubeconfig 的 127.0.0.1 改写为容器内可达的 host.docker.internal
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
# 运行时数据（store.json）挂到命名卷；docker 构建通过挂载宿主 /var/run/docker.sock 复用宿主 daemon
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/docker-entrypoint.sh"]
# 不传 -config：无 config.json 时服务端回落内置默认值（loopback=false 由 -addr :8080
# 决定，API_TOKEN 经入口脚本从 /app/secrets/.env export 注入）。store 默认落在 ./data
# （即挂载的 /app/data），与本地 make run 共用同一份 store.json。
CMD ["-addr", ":8080"]
