#!/usr/bin/env bash
# 把 Go 核心编译成 Android 可执行文件（放进 jniLibs）。
#
# 为什么不是 gomobile/AAR：gomobile 依赖 cgo + NDK 的 clang，本机链路上会卡在
# cgo 探针；改用「纯 Go 静态可执行文件 + 本机回环 HTTP」后不需要 cgo/NDK，构建稳定。
# 详见 phone/README.md。
#
# 前提：
#   - Go 1.26+
#   - 无需 Android SDK / NDK（纯 Go 交叉编译）
#
# 用法：
#   phone/scripts/build-core.sh
#
# 产物：phone/android/app/src/main/jniLibs/arm64-v8a/libfstcored.so
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/phone/android/app/src/main/jniLibs/arm64-v8a"
mkdir -p "$OUT"

cd "$ROOT"

# 编译参数的两个硬约束（改动前务必读）：
#
# 1) 不能加 -buildmode=pie。Go 的 internal linker 会为 linux/arm64 的 PIE 写入
#    PT_INTERP = /lib/ld-linux-aarch64.so.1（glibc 的加载器路径）。该路径在
#    Android 上不存在，execve 直接返回 ENOENT，进程根本不会启动，且没有任何
#    日志——表现为「App 正常打开、但 8787 无监听、mDNS 零报文」。
#    默认 buildmode=exe 产出 ET_EXEC 静态文件：没有 .interp、没有动态段，
#    内核直接加载，不经过 bionic 加载器，因此在 Android 上可以正常 execve
#    （静态 busybox 同理）。经 llvm-readelf 校验：Type=EXEC、无 .interp。
#
# 2) 不能改用 GOOS=android。它会把解释器写成 /system/bin/linker64（这是对的），
#    但会启用 wlynxg/anet 的 Android 分支，该分支用
#    //go:linkname zoneCache net.zoneCache 引用 Go 内部符号，Go 1.27 已移除，
#    链接报 invalid reference to net.zoneCache；anet 最新版本即 v0.0.5，
#    上游无修复。纯 Go 静态可执行文件在 Android 上本就无需 android 目标平台。
#
# LOAD 段按 0x10000（64KB）对齐，同时满足 Android 15 起 16KB 页大小的要求。
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o "$OUT/libfstcored.so" ./phone/daemon

# 必须是 lib*.so 命名：只有这种命名的文件才会被系统解压到 nativeLibraryDir，
# 而 nativeLibraryDir 是 Android 10+ 仍允许 execve 的位置。
#
# 自检：出现 glibc 加载器路径说明误加了 -buildmode=pie，产物在 Android 上
# 一定无法 execve，宁可在打包前失败。
if grep -aq "ld-linux-aarch64" "$OUT/libfstcored.so"; then
  echo "构建产物含有 /lib/ld-linux-aarch64.so.1 解释器，Android 无法执行；" >&2
  echo "请去掉 -buildmode=pie（见本脚本顶部说明）。" >&2
  exit 1
fi

echo "已生成: $OUT/libfstcored.so"
ls -lh "$OUT/libfstcored.so"
