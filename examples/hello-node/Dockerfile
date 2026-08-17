# 纯 Node.js 示例：解释型语言，无需编译，直接运行源文件（无 build 阶段）。
# 构建发生在 Docker 容器内（Linux），与宿主机 macOS 的 LC_UUID 问题无关。
# 锁定到完整补丁版本（而非滚动的 node:24-alpine），保证构建可复现、避免 Node 小版本漂移导致镜像体积/行为变化。
FROM node:24.19.0-alpine
WORKDIR /app
ARG APP_VERSION=dev
ENV APP_VERSION=$APP_VERSION
# 零依赖应用：无需 npm install，也无需把 package.json 拷进镜像（运行时只读 PORT/APP_VERSION）。
# 若后续添加依赖，在此改为：
#   COPY package*.json ./
#   RUN npm install --omit=dev
#   COPY server.js ./
COPY server.js ./
RUN chown -R node:node /app
ENV PORT=8080
EXPOSE 8080
# node:alpine 镜像自带非 root 的 node 用户，直接切换避免以 root 运行
USER node
CMD ["node", "server.js"]
