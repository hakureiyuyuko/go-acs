#!/usr/bin/env bash
# 打发布包：linux/amd64 与 linux/arm64 两个 tar.gz + SHA256SUMS
#
#   scripts/build-release.sh v1.0.0        # 产物在 dist/
#   ARCHES="amd64" scripts/build-release.sh v1.0.0
#
# 包里 = acs 二进制 + install.sh / uninstall.sh / update.sh + acs.service + README + LICENSE + VERSION
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

VERSION=${1:-}
if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --abbrev=0 2>/dev/null || true)
fi
if [ -z "$VERSION" ]; then
  echo "用法: scripts/build-release.sh <版本，如 v1.0.0>" >&2
  exit 1
fi
VER=${VERSION#v}          # 文件名里不带 v
ARCHES=${ARCHES:-"amd64 arm64"}

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
command -v go >/dev/null 2>&1 || { echo "找不到 go" >&2; exit 1; }

DIST="$ROOT/dist"
STAGE="$DIST/stage"
rm -rf "$STAGE"
mkdir -p "$STAGE"

for arch in $ARCHES; do
  pkg="acs-${VER}-linux-${arch}"
  out="$STAGE/$pkg"
  mkdir -p "$out"
  echo "== 编译 $pkg"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" -o "$out/acs" ./cmd/acs

  install -m 0755 deploy/install.sh   "$out/install.sh"
  install -m 0755 deploy/uninstall.sh "$out/uninstall.sh"
  install -m 0755 deploy/update.sh    "$out/update.sh"
  install -m 0644 deploy/acs.service  "$out/acs.service"
  install -m 0644 deploy/README.md    "$out/README.md"
  install -m 0644 LICENSE             "$out/LICENSE"
  printf '%s\n' "$VER" > "$out/VERSION"

  tar czf "$DIST/$pkg.tar.gz" -C "$STAGE" "$pkg"
  rm -rf "$out"
  echo "   → dist/$pkg.tar.gz（$(du -h "$DIST/$pkg.tar.gz" | cut -f1)）"
done

rmdir "$STAGE" 2>/dev/null || true

# 校验和（update.sh 会拿它验证）
( cd "$DIST" && sha256sum acs-*.tar.gz > SHA256SUMS )
echo "== SHA256SUMS"
cat "$DIST/SHA256SUMS"
echo "== 内容示例（$(ls "$DIST" | grep -c 'tar.gz') 个包）"
tar tzf "$(ls "$DIST"/acs-*.tar.gz | head -1)" | sed 's/^/   /'
