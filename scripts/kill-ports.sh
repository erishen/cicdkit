#!/usr/bin/env bash
# kill-ports.sh — 按端口强杀占用进程，并轮询等待端口真正释放。
#
# 比按进程名 pkill 更可靠：能清掉用 `go run` / 裸 `./bin/server` / `vite` 等
# 任意方式起的残留进程，避免新进程因端口被占而启动失败（旧进程继续服务旧代码）。
#
# 用法: bash kill-ports.sh 8080 5173
set -u

for port in "$@"; do
  pids=$(lsof -ti tcp:"$port" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    echo "▶ 释放端口 $port (PID: $(echo "$pids" | tr '\n' ' '))"
    echo "$pids" | xargs -n1 kill -9 2>/dev/null || true
  else
    echo "▶ 端口 $port 空闲"
  fi
  # 轮询等待端口真正空闲（最多 ~5s），确保新进程能绑定。
  n=0
  while lsof -ti tcp:"$port" >/dev/null 2>&1; do
    n=$((n + 1))
    if [ "$n" -ge 10 ]; then
      echo "⚠️ 端口 $port 在 5s 内仍未释放，新进程可能无法绑定"
      break
    fi
    sleep 0.5
  done
done
echo "▶ 端口清理完成"
