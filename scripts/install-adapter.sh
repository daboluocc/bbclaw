#!/usr/bin/env bash
# install-adapter.sh — 一键下载最新 bbclaw-adapter 二进制（自动识别系统/架构）
#
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/daboluocc/bbclaw/main/scripts/install-adapter.sh | bash
#
# 环境变量:
#   BBCLAW_INSTALL_DIR   程序/配置目录（默认 $HOME/bbclaw-adapter，.env 放这里）
#   BBCLAW_BIN_DIR       放进 PATH 的可执行目录（默认 /usr/local/bin 可写时用它，
#                        否则 $HOME/.local/bin）
#   BBCLAW_VERSION       指定版本 tag（默认 latest）

set -euo pipefail

REPO="daboluocc/bbclaw"
INSTALL_DIR="${BBCLAW_INSTALL_DIR:-$HOME/bbclaw-adapter}"
VERSION="${BBCLAW_VERSION:-latest}"

# 可执行文件必须进系统 PATH：butler 会让 AI 直接敲 `bbclaw-adapter device ...`
# 来配置设备（如语音调音量），不在 PATH 里会 command not found 失败。
# 优先 /usr/local/bin（多数系统默认在 PATH 里）；不可写就退回 ~/.local/bin。
if [ -n "${BBCLAW_BIN_DIR:-}" ]; then
  BIN_DIR="$BBCLAW_BIN_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  BIN_DIR="/usr/local/bin"
else
  BIN_DIR="$HOME/.local/bin"
fi

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
echo "==> 检测到平台: ${os}/${arch}"

# 最新的 firmware-only release 不带 adapter 二进制，不能直接用 /releases/latest/download/。
# 从 releases.atom（公开 RSS，不受 API 限流影响）拿 tag 列表，再对每个 tag
# HEAD 一次资产 URL，第一个返回 200/302 的就是最新可用版本。
if [ "$VERSION" = "latest" ]; then
  echo "==> 查询最新带 adapter 二进制的 release"
  atom_url="https://github.com/${REPO}/releases.atom"
  tags="$(curl -fsSL "$atom_url" \
    | grep -o "/${REPO}/releases/tag/[^\"<]*" \
    | sed "s#/${REPO}/releases/tag/##" \
    || true)"
  if [ -z "$tags" ]; then
    echo "错误: 无法从 $atom_url 获取 release 列表" >&2
    exit 1
  fi

  url=""
  for tag in $tags; do
    candidate="https://github.com/${REPO}/releases/download/${tag}/${binary}"
    code="$(curl -sI -o /dev/null -w '%{http_code}' "$candidate" || echo 000)"
    if [ "$code" = "200" ] || [ "$code" = "302" ]; then
      url="$candidate"
      echo "==> 命中 $tag"
      break
    fi
  done

  if [ -z "$url" ]; then
    echo "错误: 在最近的 release 中找不到 ${binary}" >&2
    echo "      请到 https://github.com/${REPO}/releases 手动下载，或用 BBCLAW_VERSION=vX.Y.Z 指定版本" >&2
    exit 1
  fi
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${binary}"
fi

echo "==> 下载 ${url} 到 ${INSTALL_DIR}"

mkdir -p "$INSTALL_DIR"
tmp="$(mktemp "${TMPDIR:-/tmp}/bbclaw-adapter.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

if ! curl -fL --retry 3 --progress-bar -o "$tmp" "$url"; then
  echo "错误: 下载失败: $url" >&2
  exit 1
fi

install -m 0755 "$tmp" "$INSTALL_DIR/bbclaw-adapter"

# 在 PATH 目录放一个 symlink，使任意位置都能 `bbclaw-adapter ...`。
# 二进制本体仍留在 $INSTALL_DIR（.env 也在那里），symlink 只负责进 PATH。
mkdir -p "$BIN_DIR"
if ln -sf "$INSTALL_DIR/bbclaw-adapter" "$BIN_DIR/bbclaw-adapter" 2>/dev/null; then
  echo "==> 已链接到 PATH: $BIN_DIR/bbclaw-adapter -> $INSTALL_DIR/bbclaw-adapter"
else
  echo "警告: 无法写入 $BIN_DIR，跳过 PATH 链接（可设 BBCLAW_BIN_DIR 指定可写目录）" >&2
fi

# 检查 BIN_DIR 是否真的在 PATH 里，不在就提示用户加。
on_path=0
case ":${PATH}:" in
  *":${BIN_DIR}:"*) on_path=1 ;;
esac

cat <<EOF

安装完成:
  二进制: $INSTALL_DIR/bbclaw-adapter
  PATH 链接: $BIN_DIR/bbclaw-adapter
EOF

if [ "$on_path" -ne 1 ]; then
  cat <<EOF

⚠ $BIN_DIR 不在 PATH 中——设备配置（语音调音量等）需要 \`bbclaw-adapter\` 全局可用。
  把下面这行加到 ~/.zshrc 或 ~/.bashrc，然后重开终端:
    export PATH="$BIN_DIR:\$PATH"
EOF
fi

cat <<EOF

下一步（参考 docs/skills/bbclaw-adapter-external-install.md）:
  1. 在 $INSTALL_DIR 下创建 .env，至少填好 ADAPTER_AUTH_TOKEN / OPENCLAW_WS_URL / ASR_* 等
  2. cd $INSTALL_DIR && set -a && source .env && set +a
  3. bbclaw-adapter          # 已在 PATH，无需 ./ 前缀

验证 CLI 可被找到:
  command -v bbclaw-adapter

健康检查:
  curl -H "Authorization: Bearer \$ADAPTER_AUTH_TOKEN" http://127.0.0.1:18080/healthz
EOF
