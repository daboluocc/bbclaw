#!/usr/bin/env bash
# install-adapter.sh — 一键下载最新 bbclaw-adapter 二进制（自动识别系统/架构）
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/daboluocc/bbclaw/main/scripts/install-adapter.sh | bash
#
# 环境变量:
#   BBCLAW_INSTALL_DIR   安装目录（默认 $HOME/bbclaw-adapter）
#   BBCLAW_VERSION       指定版本 tag（默认 latest）

set -euo pipefail

REPO="daboluocc/bbclaw"
INSTALL_DIR="${BBCLAW_INSTALL_DIR:-$HOME/bbclaw-adapter}"
VERSION="${BBCLAW_VERSION:-latest}"

uname_s="$(uname -s)"
uname_m="$(uname -m)"

case "$uname_s" in
  Darwin) os="darwin" ;;
  Linux)  os="linux" ;;
  MINGW*|MSYS*|CYGWIN*)
    echo "错误: 请在 PowerShell 中运行 install-adapter.ps1：" >&2
    echo "  iwr -useb https://raw.githubusercontent.com/${REPO}/main/scripts/install-adapter.ps1 | iex" >&2
    exit 1
    ;;
  *)
    echo "错误: 不支持的操作系统: $uname_s" >&2
    exit 1
    ;;
esac

case "$uname_m" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "错误: 不支持的 CPU 架构: $uname_m" >&2
    exit 1
    ;;
esac

binary="bbclaw-adapter-${os}-${arch}"
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${binary}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${binary}"
fi

echo "==> 检测到平台: ${os}/${arch}"
echo "==> 下载 ${binary} (${VERSION}) 到 ${INSTALL_DIR}"

mkdir -p "$INSTALL_DIR"
tmp="$(mktemp "${TMPDIR:-/tmp}/bbclaw-adapter.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

if ! curl -fL --retry 3 --progress-bar -o "$tmp" "$url"; then
  echo "错误: 下载失败: $url" >&2
  exit 1
fi

install -m 0755 "$tmp" "$INSTALL_DIR/bbclaw-adapter"

cat <<EOF

安装完成: $INSTALL_DIR/bbclaw-adapter

下一步（参考 docs/skills/bbclaw-adapter-external-install.md）:
  1. 在 $INSTALL_DIR 下创建 .env，至少填好 ADAPTER_AUTH_TOKEN / OPENCLAW_WS_URL / ASR_* 等
  2. cd $INSTALL_DIR && set -a && source .env && set +a
  3. ./bbclaw-adapter

健康检查:
  curl -H "Authorization: Bearer \$ADAPTER_AUTH_TOKEN" http://127.0.0.1:18080/healthz
EOF
