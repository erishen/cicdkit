# 纯 Rust 示例：编译型语言，需多阶段构建（编译与运行分离）。
# 构建发生在 Docker 容器内（Linux），与宿主机无关。
# 锁定到完整补丁版本（而非滚动的 rust:1.85）保证构建可复现、避免工具链漂移。
FROM rust:1.85.0-slim AS builder
WORKDIR /app
# 先拷依赖清单触发缓存层，再拷源码；无外部依赖时 Cargo.lock 可能不存在，用 * 通配。
COPY Cargo.toml Cargo.lock* ./
COPY src ./src
RUN cargo build --release

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=builder /app/target/release/hello-rust /app/hello-rust
RUN chmod 755 /app/hello-rust
ENV PORT=8080
ENV APP_VERSION=dev
EXPOSE 8080
# debian:bookworm-slim 自带 nobody 用户，避免以 root 运行
USER nobody
CMD ["/app/hello-rust"]
