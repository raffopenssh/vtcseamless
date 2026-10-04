#!/usr/bin/env bash
# Tag + publish a GitHub Release with linux amd64/arm64 tarballs.  tools/release.sh v0.3.0
set -euo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd); cd "$HERE"
V=${1:?version, e.g. v0.3.0}; TAG=$V
git tag -a "$TAG" -m "vtcseamless / bevdirect-serve $V" 2>/dev/null || true
git push origin "$TAG"
rm -rf dist; tools/package.sh amd64; tools/package.sh arm64
gh release create "$TAG" dist/*.tar.gz --title "vtcseamless $V" \
  --notes "Seamless polygons from a vector-tile cache; BEV preset (bevdirect-serve) assembles the Austrian cadastre straight from kataster.bev.gv.at tiles (CC BY 4.0). Install: see DEPLOY.md — \`curl -fsSL https://raw.githubusercontent.com/raffopenssh/vtcseamless/main/bootstrap.sh | bash\`. Admin table: BEV Verwaltungsgrenzen (VGD) 1:50 000, Stichtag 2026-04-01, CC BY 4.0"
