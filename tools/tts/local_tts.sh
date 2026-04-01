#!/bin/bash
# Cross-platform local TTS dispatcher for CLOUD_TTS_PROVIDER=local_command.
# Usage: local_tts.sh "text" /path/to/output.wav [speed_ratio]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OS_NAME="$(uname -s)"

if [[ -n "${BBCLAW_LOCAL_TTS_IMPL:-}" ]]; then
  IMPL="${BBCLAW_LOCAL_TTS_IMPL}"
else
  case "$OS_NAME" in
    Darwin)
      IMPL="${SCRIPT_DIR}/macos_say.sh"
      ;;
    Linux)
      IMPL="${SCRIPT_DIR}/linux_espeak.sh"
      ;;
    *)
      echo "unsupported local TTS platform: ${OS_NAME}" >&2
      exit 1
      ;;
  esac
fi

if [[ ! -x "$IMPL" ]]; then
  echo "local TTS implementation is not executable: ${IMPL}" >&2
  exit 1
fi

exec "$IMPL" "$@"
