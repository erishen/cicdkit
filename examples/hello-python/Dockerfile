# 纯 Python 示例：解释型语言，无需编译，直接运行源文件（无 build 阶段）。
# 锁定到完整补丁版本（而非滚动的 python:3.12）保证构建可复现。
FROM python:3.12.7-slim-bookworm
WORKDIR /app
# 零依赖，requirements.txt 仅含注释；安装步骤容错跳过。
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt || true
COPY server.py ./
ENV PORT=8080
ENV APP_VERSION=dev
EXPOSE 8080
# python:slim-bookworm 镜像不含预置非 root 用户，显式创建 appuser(uid 1000) 并授权目录后切换
RUN useradd -m -u 1000 appuser && chown -R appuser:appuser /app
USER appuser
CMD ["python", "server.py"]
