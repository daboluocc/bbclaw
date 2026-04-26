#!/bin/bash
# Minimal local TTS wrapper for CLOUD_TTS_PROVIDER=local_command on macOS.
# Usage: macos_say.sh "text" /path/to/output.wav [speed_ratio]
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <text> <output.wav> [speed_ratio]" >&2
  exit 1
fi

TEXT="$1"
OUT="$2"
SPEED="${3:-1.00}"

# Use a temp directory so the AIFF path is always unique and cleanup is atomic.
# Plain `mktemp /tmp/name-XXXXXX.aiff` can fail with "File exists" if a stale
# literal /tmp/name-XXXXXX.aiff survives from a previously killed run.
TMP_DIR="$(mktemp -d)"
TMP_AIFF="${TMP_DIR}/out.aiff"
trap 'rm -rf "$TMP_DIR"' EXIT

RATE="185"
if [[ "$SPEED" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  RATE="$(python3 - "$SPEED" <<'PY'
import sys
speed = max(0.5, min(2.0, float(sys.argv[1])))
print(int(round(185 * speed)))
PY
)"
fi

mkdir -p "$(dirname "$OUT")"
say -r "$RATE" -o "$TMP_AIFF" -- "$TEXT"
ffmpeg -hide_banner -loglevel error -y -i "$TMP_AIFF" -ar 16000 -ac 1 "$OUT"
