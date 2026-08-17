'use strict';

// 零依赖 Node.js HTTP 服务，用于演示 cicdkit 的构建 / 发布 / 探针闭环。
// 读取 PORT 与 APP_VERSION 环境变量（由平台 Dockerfile 的 build-arg 注入）。
const http = require('http');
const os = require('os');

const PORT = parseInt(process.env.PORT || '8080', 10);
const VERSION = process.env.APP_VERSION || 'dev';
const HOSTNAME = os.hostname();

const server = http.createServer((req, res) => {
  // 健康检查端点（可选，平台探针默认探测根路径）
  if (req.url === '/health' || req.url === '/healthz') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'healthy', version: VERSION }));
    return;
  }
  res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' });
  res.end(`Hello from hello-node (version ${VERSION})\n`);
});

server.listen(PORT, () => {
  console.log(`hello-node ${VERSION} listening on :${PORT} (pod ${HOSTNAME})`);
});
