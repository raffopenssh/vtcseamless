#!/usr/bin/env bash
# Build a self-contained release tarball (static linux binary, no Go needed on the target).
#   tools/package.sh [GOARCH]   → dist/bevdirect-serve_<version>_linux_<arch>.tar.gz
set -euo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd); cd "$HERE"
ARCH=${1:-amd64}
VER=$(git describe --tags --match "v*" --always --dirty 2>/dev/null)
OUT=dist/bevdirect-serve_${VER}_linux_${ARCH}; rm -rf "$OUT"; mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags="-s -w -X main.version=$VER" -o "$OUT/bevdirect-serve" ./cmd/bevdirect-serve
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags='-s -w' -o "$OUT/bevdirect" ./cmd/bevdirect
cp install.sh bevdirect-serve.service README.md DEPLOY.md LICENSE "$OUT/"
tar -C dist -czf "$OUT.tar.gz" "$(basename "$OUT")"
rm -rf "$OUT"
ls -la "$OUT.tar.gz"
