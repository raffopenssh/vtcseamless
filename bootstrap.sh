#!/usr/bin/env bash
# Install bevdirect-serve on a fresh VM from the public repo — no credentials needed.
#
#   curl -fsSL https://raw.githubusercontent.com/raffopenssh/vtcseamless/main/bootstrap.sh \
#     | PREFIX=/opt/bevdirect PORT=8787 bash
#
# Downloads the latest GitHub Release tarball (static binary) and falls back to a
# source build (installing Go if missing).
# Env: PREFIX=/opt/bevdirect PORT=8787 VERSION=v0.3.0 (default: latest release) FROM_SOURCE=1
set -euo pipefail
REPO=raffopenssh/vtcseamless
PREFIX=${PREFIX:-/opt/bevdirect}; PORT=${PORT:-8787}
WORK=$(mktemp -d); cd "$WORK"
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; esac

fetch_release() {
  local api="https://api.github.com/repos/$REPO/releases/latest"
  [ -n "${VERSION:-}" ] && api="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
  local json asset_url
  json=$(curl -fsSL -H 'Accept: application/vnd.github+json' "$api") || return 1
  asset_url=$(printf '%s' "$json" | python3 -c '
import json,sys; d=json.load(sys.stdin); a=sys.argv[1]
print(next((x["browser_download_url"] for x in d.get("assets",[]) if x["name"].endswith("linux_%s.tar.gz"%a)),""))' "$ARCH") || return 1
  [ -n "$asset_url" ] || return 1
  echo "downloading release $(printf '%s' "$json" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tag_name"])') ($ARCH)"
  curl -fsSL "$asset_url" -o rel.tar.gz
  tar xzf rel.tar.gz && cd bevdirect-serve_*/
}

build_source() {
  if ! command -v go >/dev/null; then
    echo "installing Go…"; GOV=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
    curl -fsSL "https://go.dev/dl/${GOV}.linux-${ARCH}.tar.gz" | sudo tar -C /usr/local -xzf -
    export PATH=$PATH:/usr/local/go/bin
  fi
  echo "building from source ($REPO)…"
  git clone -q "https://github.com/$REPO.git" src
  cd src
  [ -n "${VERSION:-}" ] && git checkout -q "$VERSION"
  # Same version string as tools/package.sh (release tarballs): "v0.3.0" on a tag, "v0.3.0-3-gabc1234"
  # past it — so /viewport's bevdirect_version and downstream "bevdirect@<tag>" source pins match.
  VER=$(git describe --tags --match "v*" --always --dirty 2>/dev/null)
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VER" -o bevdirect-serve ./cmd/bevdirect-serve
}

if [ "${FROM_SOURCE:-0}" = 1 ] || ! fetch_release; then build_source; fi
PREFIX="$PREFIX" PORT="$PORT" ./install.sh
