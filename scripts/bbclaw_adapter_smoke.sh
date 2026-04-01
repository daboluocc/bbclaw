#!/usr/bin/env bash
set -euo pipefail

# Smoke test for bbclaw-adapter HTTP API:
# start -> chunk -> finish
#
# Usage:
#   ADAPTER_BASE_URL=http://127.0.0.1:18080 \
#   ADAPTER_AUTH_TOKEN=token \
#   ./scripts/bbclaw_adapter_smoke.sh

ADAPTER_BASE_URL="${ADAPTER_BASE_URL:-http://127.0.0.1:18080}"
ADAPTER_AUTH_TOKEN="${ADAPTER_AUTH_TOKEN:-}"
DEVICE_ID="${DEVICE_ID:-bbclaw-dev-1}"
SESSION_KEY="${SESSION_KEY:-agent:main:main}"
STREAM_ID="${STREAM_ID:-smoke-$(date +%s)}"
CODEC="${CODEC:-pcm16}"
SAMPLE_RATE="${SAMPLE_RATE:-16000}"
CHANNELS="${CHANNELS:-1}"

AUTH_HEADER=()
if [[ -n "${ADAPTER_AUTH_TOKEN}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${ADAPTER_AUTH_TOKEN}")
fi

request() {
  local method="$1"
  local path="$2"
  local body="$3"
  curl -sS -X "${method}" \
    "${ADAPTER_BASE_URL}${path}" \
    -H "Content-Type: application/json" \
    "${AUTH_HEADER[@]}" \
    -d "${body}"
}

echo "[1/4] Health check..."
curl -sS "${ADAPTER_BASE_URL}/healthz" "${AUTH_HEADER[@]}" || {
  echo "health check failed"
  exit 1
}
echo

echo "[2/4] Start stream..."
START_BODY="$(cat <<JSON
{
  "deviceId": "${DEVICE_ID}",
  "sessionKey": "${SESSION_KEY}",
  "streamId": "${STREAM_ID}",
  "codec": "${CODEC}",
  "sampleRate": ${SAMPLE_RATE},
  "channels": ${CHANNELS}
}
JSON
)"
START_RESP="$(request POST "/v1/stream/start" "${START_BODY}")"
echo "${START_RESP}"
if ! echo "${START_RESP}" | grep -q '"ok":true'; then
  echo "start failed: expected ok=true"
  exit 1
fi
echo

echo "[3/4] Send chunk..."
if [[ "${CODEC}" == "opus" ]]; then
  if command -v ffmpeg >/dev/null 2>&1; then
    AUDIO_BASE64="$(
      ffmpeg -hide_banner -loglevel error \
        -f lavfi -i "sine=frequency=440:duration=0.2:sample_rate=${SAMPLE_RATE}" \
        -ac "${CHANNELS}" \
        -c:a libopus -f opus pipe:1 | base64
    )"
  else
    # Adapter accepts BBPCM16 envelope on codec=opus path for firmware bring-up.
    AUDIO_BASE64="$(printf 'BBPCM16\nhello-bbclaw' | base64)"
  fi
else
  AUDIO_BASE64="$(printf 'hello-bbclaw' | base64)"
fi
CHUNK_BODY="$(cat <<JSON
{
  "deviceId": "${DEVICE_ID}",
  "sessionKey": "${SESSION_KEY}",
  "streamId": "${STREAM_ID}",
  "seq": 1,
  "timestampMs": 1000,
  "audioBase64": "${AUDIO_BASE64}"
}
JSON
)"
CHUNK_RESP="$(request POST "/v1/stream/chunk" "${CHUNK_BODY}")"
echo "${CHUNK_RESP}"
if ! echo "${CHUNK_RESP}" | grep -q '"ok":true'; then
  echo "chunk failed: expected ok=true"
  exit 1
fi
echo

echo "[4/4] Finish stream..."
FINISH_BODY="$(cat <<JSON
{
  "deviceId": "${DEVICE_ID}",
  "sessionKey": "${SESSION_KEY}",
  "streamId": "${STREAM_ID}"
}
JSON
)"
FINISH_RESP="$(request POST "/v1/stream/finish" "${FINISH_BODY}")"
echo "${FINISH_RESP}"
if ! echo "${FINISH_RESP}" | grep -q '"ok":true'; then
  echo "finish failed: expected ok=true"
  exit 1
fi
echo

cat <<EOF
Done.
streamId=${STREAM_ID}

If finish failed with ASR_FAILED or OPENCLAW_DELIVERY_FAILED, check:
1) ASR envs in src/.env.example
2) OPENCLAW_RPC_URL reachability
EOF
