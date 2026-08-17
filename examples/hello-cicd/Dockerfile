# 多阶段构建：golang:1.22 编译 → alpine 运行
# 注意：此处构建发生在 Docker 容器内（Linux），与宿主机 macOS 的 LC_UUID 问题无关。
FROM golang:1.22-alpine AS build
WORKDIR /src
ARG APP_VERSION=dev
ENV APP_VERSION=$APP_VERSION
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/hello .

FROM alpine:3.20
WORKDIR /app
RUN adduser -D -u 10001 appuser
COPY --from=build --chown=appuser:appuser /out/hello /app/hello
USER appuser
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/hello"]
