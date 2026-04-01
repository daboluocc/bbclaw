#!/bin/bash
# Minimal local TTS wrapper for Linux.
# Prefers espeak-ng, falls back to espeak, then uses Python stdlib to normalize to
# either 16k mono WAV or raw PCM16LE depending on output path suffix.
# Usage: linux_espeak.sh "text" /path/to/output.{wav|pcm16} [speed_ratio]
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <text> <output.wav> [speed_ratio]" >&2
  exit 1
fi

TEXT="$1"
OUT="$2"
SPEED="${3:-1.00}"

if command -v espeak-ng >/dev/null 2>&1; then
  ESPEAK_BIN="espeak-ng"
elif command -v espeak >/dev/null 2>&1; then
  ESPEAK_BIN="espeak"
else
  echo "missing local TTS engine: install espeak-ng or espeak" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TMP_WAV="$(mktemp "${SCRIPT_DIR}/linux-espeak-XXXXXX.wav")"
trap 'rm -f "$TMP_WAV"' EXIT

RATE="175"
if [[ "$SPEED" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  RATE="$(python3 - "$SPEED" <<'PY'
import sys
speed = max(0.5, min(2.0, float(sys.argv[1])))
print(int(round(175 * speed)))
PY
)"
fi

mkdir -p "$(dirname "$OUT")"
"$ESPEAK_BIN" -s "$RATE" -w "$TMP_WAV" -- "$TEXT"

python3 - "$TMP_WAV" "$OUT" <<'PY'
import audioop
import os
import struct
import sys
import wave

src_path, out_path = sys.argv[1], sys.argv[2]

with wave.open(src_path, "rb") as wf:
    channels = wf.getnchannels()
    sample_width = wf.getsampwidth()
    sample_rate = wf.getframerate()
    frames = wf.readframes(wf.getnframes())

if channels != 1:
    frames = audioop.tomono(frames, sample_width, 0.5, 0.5)
if sample_width != 2:
    frames = audioop.lin2lin(frames, sample_width, 2)
    sample_width = 2
if sample_rate != 16000:
    frames, _ = audioop.ratecv(frames, sample_width, 1, sample_rate, 16000, None)

lower = out_path.lower()
if lower.endswith(".pcm") or lower.endswith(".pcm16"):
    with open(out_path, "wb") as f:
        f.write(frames)
else:
    with wave.open(out_path, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(16000)
        wf.writeframes(frames)
PY
