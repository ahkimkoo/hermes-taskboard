#!/usr/bin/env bash
# Build a single-binary hermes-taskboard for the current platform.
# Output: ./bin/hermes-taskboard
#
# Usage:
#   ./build.sh              # host platform
#   GOOS=linux GOARCH=arm64 ./build.sh
#   VERSION=v0.1.0 ./build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OS="${GOOS:-$(go env GOOS)}"
ARCH="${GOARCH:-$(go env GOARCH)}"
OUT_DIR="${OUT_DIR:-bin}"
BIN_NAME="hermes-taskboard"
[ "$OS" = "windows" ] && BIN_NAME="${BIN_NAME}.exe"

mkdir -p "$OUT_DIR"

echo "→ building ${BIN_NAME} (${OS}/${ARCH}) version=${VERSION}"
CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" \
  go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
  -o "${OUT_DIR}/${BIN_NAME}" ./cmd/taskboard

ls -lh "${OUT_DIR}/${BIN_NAME}"
echo "✓ build ok"
