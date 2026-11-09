#!/usr/bin/env bash
# 构建可安装的 Android APK。
#
# 前提：
#   - 已运行 build-core.sh（否则 jniLibs 里没有 libfstcored.so，App 启动会报「缺少核心二进制」）
#   - Gradle 8.9+（AGP 8.7.3 要求；本仓库验证使用 8.13）
#   - JAVA_HOME 指向 JDK 17 或 21
#   - local.properties 里的 sdk.dir 指向本机 Android SDK
#
# 用法：
#   phone/scripts/build-apk.sh            # debug，自动用 debug key 签名，可直接安装
#   phone/scripts/build-apk.sh release    # release，未配置 keystore 时使用 debug 签名
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-debug}"

if [[ ! -f "$ROOT/phone/android/app/src/main/jniLibs/arm64-v8a/libfstcored.so" ]]; then
  echo "缺少核心二进制，请先运行 phone/scripts/build-core.sh" >&2
  exit 1
fi

GRADLE="${GRADLE:-gradle}"
if ! command -v "$GRADLE" >/dev/null 2>&1; then
  echo "找不到 gradle，可用 GRADLE=/path/to/gradle 指定" >&2
  exit 1
fi

cd "$ROOT/phone/android"

case "$MODE" in
  debug)
    "$GRADLE" --no-daemon --console=plain assembleDebug
    echo "APK: phone/android/app/build/outputs/apk/debug/app-debug.apk"
    ;;
  release)
    "$GRADLE" --no-daemon --console=plain assembleRelease
    echo "APK: phone/android/app/build/outputs/apk/release/app-release.apk"
    ;;
  *)
    echo "用法: build-apk.sh [debug|release]" >&2
    exit 2
    ;;
esac
