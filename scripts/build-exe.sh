#!/usr/bin/env bash
# 构建 Windows 桌面客户端（Wails 窗口 + 托盘）。
#
# 前提：
#   - Go 1.26+
#   - front/dist 已构建（Go 用 go:embed all:front/dist 打包前端，缺了编译直接失败）
#   - wails3 在 PATH 里（只用来生成带图标与 manifest 的 .syso；没有也能编，只是
#     exe 没有图标、没有 DPI/manifest 声明）
#
# 用法：
#   scripts/build-exe.sh
#
# 产物：
#   bin/fst.exe                 可直接运行
#   bin/wintun.dll              WAN 模式（TUN 代理）需要的驱动库，需与 exe 同目录
#   dist/fst-<版本>-windows-amd64.exe
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VER="$(sed -n 's/.*Version *= *"\([^"]*\)".*/\1/p' internal/proto/proto.go | head -1)"
[ -n "$VER" ] || { echo "无法从 internal/proto/proto.go 读取版本号" >&2; exit 1; }

if [ ! -f front/dist/index.html ]; then
  echo "缺少 front/dist，请先在 front/ 下构建前端（pnpm build）" >&2
  exit 1
fi

mkdir -p bin dist

# 生成 .syso：把 icon.ico / manifest / 版本资源链进 exe。资源文件路径相对 build/，
# 因此先切到 build 目录再输出到上一级。
if command -v wails3 >/dev/null 2>&1; then
  ( cd build && wails3 generate syso -arch amd64 \
      -icon windows/icon.ico \
      -manifest windows/wails.exe.manifest \
      -info windows/info.json \
      -out ../fst_windows_amd64.syso >/dev/null )
  echo "已生成 wails_windows_amd64.syso"
else
  echo "未找到 wails3，跳过图标与 manifest（exe 仍可用）" >&2
fi

# -H windowsgui：双击不弹控制台窗口；日志仍写数据目录下的文件。
go build -tags "desktop,production" -trimpath \
  -ldflags "-w -s -H windowsgui" \
  -o bin/fst.exe .

rm -f fst_windows_amd64.syso

# wintun.dll 是 WAN/TUN 代理的运行期依赖，缺失时只有跨网功能不可用。
if [ -f build/windows/wintun/wintun.dll ]; then
  cp -f build/windows/wintun/wintun.dll bin/wintun.dll
else
  echo "警告：build/windows/wintun/wintun.dll 不存在，WAN 模式将不可用" >&2
fi

cp -f bin/fst.exe "dist/fst-$VER-windows-amd64.exe"

echo "已生成:"
ls -lh bin/fst.exe "dist/fst-$VER-windows-amd64.exe"
