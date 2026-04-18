#!/usr/bin/env bash
# Build cross-platform release tarballs.
# Output: dist/hermes-taskboard-<version>-<os>-<arch>.tar.gz (+ .zip for windows)
#
# Each archive contains:
#   hermes-taskboard(.exe)
#   README.md, CHANGELOG.md, LICENSE
#   config.example.yaml
#   data/                          (skeleton directory tree: db/, task/, attempt/)
#
# Usage: VERSION=v0.1.0 ./release.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST="${DIST:-dist}"
mkdir -p "$DIST"

# Platforms to cross-compile. Extend as needed.
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64}"

# Write a sample config that ships with the release.
CFG_SAMPLE="$ROOT/config.example.yaml"
cat > "$CFG_SAMPLE" <<'YAML'
# Hermes Task Board — example config
# On first run the binary writes `data/config.yaml` with sensible defaults; copy
# lines from here to customise. Secrets (api_key) get encrypted on save.

server:
  listen: "0.0.0.0:1900"
  cors_origins: []

hermes_servers:
  - id: "default"
    name: "Local Hermes"
    base_url: "http://127.0.0.1:8642"
    api_key: "CHANGE_ME_HERMES_API_SERVER_KEY"   # gets encrypted on save
    is_default: true
    max_concurrent: 10
    models:
      - name: "hermes-agent"
        is_default: true
        max_concurrent: 5

scheduler:
  scan_interval_seconds: 5
  global_max_concurrent: 50

archive:
  auto_purge_days: 30

preferences:
  language: ""
  sound:
    enabled: true
    volume: 0.7
    events:
      execute_start: true
      needs_input:   true
      done:          true

auth:
  enabled: false
YAML

# Ensure LICENSE exists so packages include a copy.
if [ ! -f "$ROOT/LICENSE" ]; then
  cat > "$ROOT/LICENSE" <<'EOF'
MIT License — see https://opensource.org/licenses/MIT
Copyright (c) 2026 Hermes Task Board contributors
EOF
fi

pack() {
  local os="$1" arch="$2"
  local name="hermes-taskboard-${VERSION}-${os}-${arch}"
  local stage="$DIST/$name"
  local bin="hermes-taskboard"
  [ "$os" = "windows" ] && bin="hermes-taskboard.exe"

  echo "━━━ $os/$arch"
  rm -rf "$stage" && mkdir -p "$stage/data/db" "$stage/data/task" "$stage/data/attempt"
  GOOS="$os" GOARCH="$arch" OUT_DIR="$stage" ./build.sh >/dev/null
  cp README.md CHANGELOG.md LICENSE config.example.yaml "$stage/"
  # helpful placeholder to make data/ survive tar
  echo "# data directory — runtime files live here. DO NOT commit." > "$stage/data/README.txt"
  touch "$stage/data/db/.keep" "$stage/data/task/.keep" "$stage/data/attempt/.keep"

  if [ "$os" = "windows" ]; then
    (cd "$DIST" && zip -qr "${name}.zip" "$name")
    echo "  → $DIST/${name}.zip"
  else
    (cd "$DIST" && tar -czf "${name}.tar.gz" "$name")
    echo "  → $DIST/${name}.tar.gz"
  fi
  rm -rf "$stage"
}

for p in $PLATFORMS; do
  os="${p%/*}"; arch="${p#*/}"
  pack "$os" "$arch"
done

# Checksums
(cd "$DIST" && sha256sum hermes-taskboard-${VERSION}-* > "SHA256SUMS-${VERSION}.txt" 2>/dev/null || true)

echo ""
echo "✓ release ${VERSION}"
ls -lh "$DIST/" | grep -E "${VERSION}"
