#!/usr/bin/env bash
#
# import-to-k3s.sh — 方案 B（纯本地、无 registry）必备步骤
#
# 平台执行 Build 后，镜像只存在于本机 docker（如 hello-cicd:20260101-120405）。
# k3s 的 containerd 看不到 docker 的镜像，所以必须把镜像 save 出来再 import 进
# k3s 的容器运行时。本脚本：
#   1. 从 examples/hello-cicd/project.json 读取 image_repo
#   2. 从平台数据文件 data/store.json 取「最近一次成功构建」的 tag（无则回退 local）
#   3. docker save <image> | k3s ctr images import -  把镜像塞进 k3s
#
# 用法（平台已 Build 过至少一次之后）：
#   ./scripts/import-to-k3s.sh
#   make k3s-import        # 等价
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROJ="$ROOT/examples/hello-cicd/project.json"
STORE="$ROOT/data/store.json"

if [ ! -f "$PROJ" ]; then
  echo "✗ 找不到 $PROJ" >&2
  exit 1
fi

IMAGE_REPO=$(python3 - "$PROJ" <<'PY'
import json, sys
print(json.load(open(sys.argv[1]))["build"]["image_repo"])
PY
)

# 取最近一次成功构建的 tag；没有则回退到 local（首次 apply 前的占位）
if [ -f "$STORE" ]; then
  TAG=$(python3 - "$STORE" "$IMAGE_REPO" <<'PY'
import json, sys
store, repo = sys.argv[1], sys.argv[2]
d = json.load(open(store))
runs = [r for r in d.get("runs", []) if r.get("status") == "success" and r.get("image_ref","").startswith(repo+":")]
if runs:
    # ListRuns 已按时间倒序，但稳妥起见再排一次
    runs.sort(key=lambda r: r.get("created_at",""), reverse=True)
    print(runs[0]["image_tag"])
else:
    print("local")
PY
)
else
  TAG=local
fi

IMAGE="$IMAGE_REPO:$TAG"

echo "▶ 检查本机 docker 是否已有镜像 $IMAGE ..."
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "✗ 本机没有 $IMAGE。" >&2
  echo "  请先在平台点 Build（或手动 docker build -t $IMAGE examples/hello-cicd）后再执行本脚本。" >&2
  exit 1
fi

echo "▶ 导入 $IMAGE 到 k3s containerd ..."
# k3s ctr 通常需要 root；若当前已是 root 可去掉 sudo
docker save "$IMAGE" | sudo k3s ctr images import - || {
  echo "✗ 导入失败。常见原因：当前用户无权访问 k3s containerd socket。" >&2
  echo "  试试直接用 root 运行：sudo $0" >&2
  exit 1
}

echo "✓ 已导入 $IMAGE 到 k3s。"
echo
echo "下一步：在平台点「Pipeline」或「Deploy」，平台会执行："
echo "  kubectl set image deployment/hello-cicd hello-cicd=$IMAGE"
echo "（k3s 因 imagePullPolicy=IfNotPresent 会直接使用刚导入的本地镜像）"
