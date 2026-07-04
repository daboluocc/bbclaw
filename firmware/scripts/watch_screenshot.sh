#!/bin/zsh
# 手表（Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板）专用拉屏脚本。
# 自动找手表的 devmon 协议口（TinyUSB CDC1），抓一帧 410x502 截图存成 PNG，
# 打印文件路径——给 AI/人肉检查渲染效果用。
#
# 用法:
#   scripts/watch_screenshot.sh              # 存到 .cache/watch-screenshot.png
#   scripts/watch_screenshot.sh /tmp/a.png   # 指定输出路径
set -u
SCRIPT_DIR="${0:A:h}"
OUT="${1:-$SCRIPT_DIR/../.cache/watch-screenshot.png}"
mkdir -p "$(dirname "$OUT")"

# 手表 TinyUSB 枚举为 usbmodem123456x：CDC0(=…1) 日志、CDC1(=…3) devmon 协议。
PORT=$(ls /dev/cu.usbmodem123456* 2>/dev/null | sort | tail -1)
if [ -z "$PORT" ]; then
  echo "watch not found: no /dev/cu.usbmodem123456* port (手表没插 / 固件没起来?)" >&2
  exit 1
fi

python3 "$SCRIPT_DIR/devmon_screenshot.py" --port "$PORT" -o "$OUT" --timeout 20 --retries 2 || exit $?
echo "$OUT"
