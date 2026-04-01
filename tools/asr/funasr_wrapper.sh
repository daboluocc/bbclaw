#!/bin/sh
# FunASR 包装器 - 供 cloud / adapter 的 local_command provider 调用
set -eu

cd "$(dirname "$0")"
VENV_ROOT="$PWD/.venv"
if [ ! -d "$VENV_ROOT" ]; then
  echo "funasr_wrapper.sh: missing $VENV_ROOT" >&2
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
  echo "funasr_wrapper.sh: no python in $VENV_ROOT/bin" >&2
  exit 1
fi
exec "$VENV_PY" funasr_cli.py "$@"
