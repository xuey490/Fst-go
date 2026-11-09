#!/usr/bin/env bash
# 交叉编译 Linux 服务版并打成 deb。
#
# 为什么是「服务版」：桌面窗口版链接 GTK4 + WebKitGTK 6.0，必须用 CGO 在 Linux
# 主机上编译（Windows 上没有 x86_64-linux-gnu 交叉工具链与 GTK 头文件）。
# 服务版用 -tags server,production + CGO_ENABLED=0，纯 Go 静态二进制，任意平台
# 都能交叉编译；装好后 `fst serve` 起服务，浏览器打开与桌面版一致的界面。
# 需要 GTK 窗口版的 deb，请在 Linux 主机上改用 build/linux/nfpm/nfpm.yaml。
#
# 前提：
#   - Go 1.26+
#   - nfpm：go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
#
# 用法：
#   scripts/build-deb.sh
#
# 产物：
#   bin/fst-linux-amd64
#   dist/fst_<版本>_amd64.deb
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VER="$(sed -n 's/^version: *"\([^"]*\)".*/\1/p' build/linux/nfpm/nfpm-headless.yaml | head -1)"
[ -n "$VER" ] || { echo "无法从 nfpm-headless.yaml 读取版本号" >&2; exit 1; }

if ! command -v nfpm >/dev/null 2>&1; then
  echo "未找到 nfpm，请先安装：" >&2
  echo "  go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest" >&2
  exit 1
fi

mkdir -p bin dist

echo "编译 linux/amd64 服务版 (版本 $VER)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -tags "server,production" -trimpath -ldflags="-w -s" \
  -o bin/fst-linux-amd64 .

# 自检：服务版必须是静态二进制。出现动态段说明 CGO 没关干净，装到精简的
# Linux 上会因缺 glibc 起不来。
if command -v file >/dev/null 2>&1; then
  file bin/fst-linux-amd64
fi

nfpm package --config build/linux/nfpm/nfpm-headless.yaml --packager deb --target dist/

echo "已生成:"
ls -lh "dist/fst_${VER}_amd64.deb"
