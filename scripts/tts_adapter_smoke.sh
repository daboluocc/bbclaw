#!/usr/bin/env bash
set -euo pipefail

ADAPTER_BASE_URL="${ADAPTER_BASE_URL:-http://127.0.0.1:18080}"
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN:-}"
TTS_TEXT="${TTS_TEXT:-你好，这是 TTS 联调测试。}"

AUTH_HEADER=()
if [[ -n "${ADAPTER_AUTH_TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${ADAPTER_AUTH_TOKEN}")
fi

echo "[1/2] Health check..."
curl -sS "${ADAPTER_BASE_URL}/healthz" "${AUTH_HEADER[@]}" >/dev/null
echo "ok"

echo "[2/2] TTS synthesize..."
RESP="$(curl -sS -X POST "${ADAPTER_BASE_URL}/v1/tts/synthesize" \
  -H "Content-Type: application/json" \
  "${AUTH_HEADER[@]}" \
  -d "{\"text\":\"${TTS_TEXT}\"}")"

echo "${RESP}"

if ! echo "${RESP}" | grep -q '"ok":true'; then
  echo "tts smoke failed: expected ok=true"
  exit 1
fi
if ! echo "${RESP}" | grep -q '"audioBase64":"'; then
  echo "tts smoke failed: audioBase64 missing"
  exit 1
fi

echo "Done."
