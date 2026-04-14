#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT_DIR/.." && pwd)"

GO_BIN="${GO:-go}"
GOOS_TARGET="${GOOS:-$(go env GOOS)}"
GOARCH_TARGET="${GOARCH:-$(go env GOARCH)}"
VERSION_INPUT="${1:-${BBCLAW_BUILD_TAG:-$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}}"
VERSION="${VERSION_INPUT#adapter/}"
BUILD_TIME="${BBCLAW_BUILD_TIME:-$(date -u +%Y%m%d-%H%M)}"
DIST_DIR="${DIST_DIR:-$ROOT_DIR/dist}"
TMP_BASE="$(mktemp -d "${TMPDIR:-/tmp}/bbclaw-adapter-release.XXXXXX")"
STAGE_DIR="$TMP_BASE/${VERSION}/${GOOS_TARGET}-${GOARCH_TARGET}"

case "$GOOS_TARGET" in
  windows) BIN_NAME="bbclaw-adapter.exe" ;;
  *) BIN_NAME="bbclaw-adapter" ;;
esac

ARCHIVE_BASENAME="bbclaw-adapter_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}"
ARCHIVE_PATH="$DIST_DIR/$ARCHIVE_BASENAME"
BIN_PATH="$STAGE_DIR/$BIN_NAME"
README_PATH="$STAGE_DIR/README.txt"
ENV_EXAMPLE_SRC="$ROOT_DIR/.env.example"

LDFLAGS="-X github.com/daboluocc/bbclaw/adapter/internal/buildinfo.Tag=${VERSION} -X github.com/daboluocc/bbclaw/adapter/internal/buildinfo.BuildTime=${BUILD_TIME}"

cleanup() {
  rm -rf "$TMP_BASE"
}

trap cleanup EXIT

mkdir -p "$DIST_DIR" "$STAGE_DIR"

(
  cd "$ROOT_DIR"
  CGO_ENABLED=0 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
    "$GO_BIN" build -trimpath -ldflags "$LDFLAGS" -o "$BIN_PATH" ./cmd/bbclaw-adapter
)

cp "$ENV_EXAMPLE_SRC" "$STAGE_DIR/.env.example"

cat > "$README_PATH" <<EOF
BBClaw Adapter
Version: $VERSION
Build time: $BUILD_TIME
Target: $GOOS_TARGET/$GOARCH_TARGET

Quick start:
1. Copy .env.example to .env and fill in your values.
2. Run the adapter binary.
3. Check /healthz after startup.
EOF

if [[ "$GOOS_TARGET" == "windows" ]]; then
  (
    cd "$STAGE_DIR"
    zip -rq "${ARCHIVE_PATH}.zip" .
  )
  printf '%s\n' "${ARCHIVE_PATH}.zip"
else
  tar -C "$STAGE_DIR" -czf "${ARCHIVE_PATH}.tar.gz" .
  printf '%s\n' "${ARCHIVE_PATH}.tar.gz"
fi
