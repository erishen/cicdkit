#!/usr/bin/env bash
# 一键启用 cicdkit 的版本化 git hooks（含 gitleaks pre-commit 密钥扫描）。
#
# 用法：
#   ./scripts/setup-hooks.sh
#
# 作用：
#   1) git config core.hooksPath hooks  —— 让 git 从仓库内的 hooks/ 目录取钩子
#      （core.hooksPath 写在本地 .git/config，不随仓库提交，故每个 clone 都需跑一次本脚本）
#   2) 检查 gitleaks 是否已安装；未装则提示安装命令（钩子会"警告后放行"，不阻断工作流）
set -euo pipefail

# 切到仓库根目录（兼容从任意子目录调用）
ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

git config core.hooksPath hooks
echo "✓ core.hooksPath = hooks  (已启用版本化 git hooks)"

if command -v gitleaks >/dev/null 2>&1; then
  echo "✓ gitleaks 已安装 ($(gitleaks version 2>/dev/null | head -1))，pre-commit 会对暂存文件做密钥扫描"
else
  echo "⚠ gitleaks 未安装：pre-commit 当前会警告后放行（不扫描）。"
  echo "  安装："
  echo "    macOS  : brew install gitleaks"
  echo "    通用   : go install github.com/gitleaks/gitleaks/v8/cmd/gitleaks@latest"
  echo "  装完后再 commit 即自动生效，无需重跑本脚本。"
fi

echo "完成。提交前如需跳过扫描（不推荐）：git commit --no-verify"
