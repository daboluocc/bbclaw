#!/bin/sh
# 在 asr 目录下启动常驻服务（需先完成 .venv 与 pip install -r requirements.txt）
# 可用 ./run_funasr_server.sh 或 sh run_funasr_server.sh；自动激活 .venv 并用 venv 内 python3/python
set -eu
cd "$(dirname "$0")"
VENV_ROOT="$PWD/.venv"
if [ ! -d "$VENV_ROOT" ]; then
  echo "run_funasr_server.sh: missing $VENV_ROOT (create with: python3 -m venv .venv && pip install -r requirements.txt)" >&2
  exit 1
fi
# shellcheck source=/dev/null
. "$VENV_ROOT/bin/activate"
VENV_PY=""
for cand in "$VENV_ROOT/bin/python3" "$VENV_ROOT/bin/python"; do
  if [ -x "$cand" ]; then
    VENV_PY=$cand
    break
  fi
done
if [ -z "$VENV_PY" ]; then
  echo "run_funasr_server.sh: no python in $VENV_ROOT/bin (expected python3 or python)" >&2
  exit 1
fi
export FUNASR_LANGUAGE="${FUNASR_LANGUAGE:-auto}"
exec "$VENV_PY" funasr_server.py "$@"
