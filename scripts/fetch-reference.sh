#!/usr/bin/env bash
# 拉取用于「参考实现交叉验证」的第三方项目。它们不进仓库（见 .gitignore）。
#
# 用法: scripts/fetch-reference.sh
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p reference
cd reference

repos=(
  "genieacs/genieacs:协议参考（TypeScript）"
  "genieacs/genieacs-sim:独立的 CPE 模拟器（验收用）"
  "skydashnet/SKYACS:Go 实现的 ACS（结构参考）"
)

for entry in "${repos[@]}"; do
  repo="${entry%%:*}"
  desc="${entry#*:}"
  name=$(basename "$repo")
  if [ -d "$name" ]; then
    echo "已存在，跳过: $name"
    continue
  fi
  echo "拉取 $repo  ($desc)"
  git clone --depth 1 -q "https://github.com/$repo.git" "$name"
done

echo
echo "完成。跑互通性验证：scripts/verify-interop.sh"
