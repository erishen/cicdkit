# 纯 Ruby 示例：解释型语言，无需编译，直接运行源文件（无 build 阶段）。
# 锁定到完整补丁版本保证构建可复现、避免 Ruby 小版本漂移。
FROM ruby:3.3.8-alpine
WORKDIR /app
COPY server.rb ./
ENV PORT=8080
ENV APP_VERSION=dev
EXPOSE 8080
# alpine 基础镜像无 useradd（仅 BusyBox adduser），显式创建非 root 用户 appuser，
# 不依赖镜像是否自带 ruby 用户，规避 USER ruby 在部分变体不存在导致的 CreateContainerConfigError。
# -D 不分配密码，-u 指定 uid；随后把 /app 归属给 appuser。
RUN adduser -D -u 1000 appuser && chown -R appuser:appuser /app
USER appuser
CMD ["ruby", "server.rb"]
