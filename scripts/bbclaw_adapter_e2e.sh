#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC_DIR="${ROOT_DIR}/src"
BIN_PATH="${SRC_DIR}/bin/bbclaw-adapter"

ADAPTER_PORT="${ADAPTER_PORT:-18080}"
ADAPTER_BASE_URL="http://127.0.0.1:${ADAPTER_PORT}"
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN:-tangyuan}"

UPSTREAM_PID=""
ADAPTER_PID=""

cleanup() {
  if [[ -n "${ADAPTER_PID}" ]] && kill -0 "${ADAPTER_PID}" 2>/dev/null; then
    kill "${ADAPTER_PID}" 2>/dev/null || true
  fi
  if [[ -n "${UPSTREAM_PID}" ]] && kill -0 "${UPSTREAM_PID}" 2>/dev/null; then
    kill "${UPSTREAM_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "[1/6] Build adapter..."
(cd "${ROOT_DIR}" && make go-build >/dev/null)

echo "[2/6] Start local mock upstream..."
python3 "${ROOT_DIR}/scripts/mock_upstream.py" >/tmp/bbclaw-mock-upstream.log 2>&1 &
UPSTREAM_PID=$!

echo "[3/6] Start adapter with local e2e env..."
(
  export ADAPTER_ADDR=":${ADAPTER_PORT}"
  export ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN}"
  export ASR_PROVIDER="openai_compatible"
  export ASR_BASE_URL="http://127.0.0.1:19091"
  export ASR_API_KEY="mock-key"
  export ASR_MODEL="mock-asr-model"
  export OPENCLAW_RPC_URL="http://127.0.0.1:19091/rpc"
  export OPENCLAW_NODE_ID="bbclaw-adapter"
  export TTS_PROVIDER="mock"
  export MAX_STREAM_SECONDS="90"
  export MAX_AUDIO_BYTES="4194304"
  export MAX_CONCURRENT_STREAMS="16"
  export HTTP_TIMEOUT_SECONDS="10"
  "${BIN_PATH}" >/tmp/bbclaw-adapter-e2e.log 2>&1
) &
ADAPTER_PID=$!

echo "[4/6] Wait for adapter health..."
for i in $(seq 1 30); do
  if curl -fsS "${ADAPTER_BASE_URL}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
  if [[ "${i}" == "30" ]]; then
    echo "adapter health check timeout"
    exit 1
  fi
done

echo "[5/7] Run ASR stream smoke (pcm16)..."
ADAPTER_BASE_URL="${ADAPTER_BASE_URL}" \
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN}" \
"${ROOT_DIR}/scripts/bbclaw_adapter_smoke.sh"

echo "[6/7] Run ASR stream smoke (opus)..."
ADAPTER_BASE_URL="${ADAPTER_BASE_URL}" \
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN}" \
CODEC="opus" \
SAMPLE_RATE="16000" \
CHANNELS="1" \
"${ROOT_DIR}/scripts/bbclaw_adapter_smoke.sh"

echo "[7/7] Run TTS smoke..."
ADAPTER_BASE_URL="${ADAPTER_BASE_URL}" \
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN}" \
"${ROOT_DIR}/scripts/tts_adapter_smoke.sh"

echo "E2E closed loop passed."
