#!/bin/sh
# 把示例项目定义导入正在运行的 cicdkit 服务。
# 用法：
#   ./scripts/seed-example.sh                       # 默认 http://localhost:8080
#   CICD_URL=http://10.0.0.5:8080 ./scripts/seed-example.sh
#   ./scripts/seed-example.sh examples/hello-cicd/project.json
set -e

BASE="${CICD_URL:-http://localhost:8080}"
PROJ="${1:-examples/hello-cicd/project.json}"

if [ ! -f "$PROJ" ]; then
  echo "找不到项目定义文件: $PROJ" >&2
  exit 1
fi

echo "➡  导入 $PROJ → $BASE/api/projects"
curl -s -m 10 -X POST "$BASE/api/projects" \
  -H 'Content-Type: application/json' \
  --data-binary "@$PROJ" \
  | (command -v jq >/dev/null 2>&1 && jq . || cat)

echo
echo "✅ 完成后在浏览器打开 $BASE 即可看到 hello-cicd 项目卡片；点击 Pipeline 触发构建+发布。"
