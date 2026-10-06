#!/usr/bin/env bash
# Install bevdirect-serve as a system service. Run from an unpacked release tarball
# (tools/package.sh) or from a clone of the repo root (then it builds first).
#   PREFIX=/opt/bevdirect PORT=8787 ./install.sh
set -euo pipefail
PREFIX=${PREFIX:-/opt/bevdirect}; PORT=${PORT:-8787}; RUN_USER=${RUN_USER:-$(id -un)}
HERE=$(cd "$(dirname "$0")" && pwd)
if [ ! -x "$HERE/bevdirect-serve" ]; then
  echo "building bevdirect-serve…"; (cd "$HERE" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bevdirect-serve ./cmd/bevdirect-serve)
fi
sudo install -m 755 "$HERE/bevdirect-serve" "$PREFIX/bevdirect-serve"
sed -e "s|__PREFIX__|$PREFIX|g" -e "s|__PORT__|$PORT|g" -e "s|__USER__|$RUN_USER|g" \
  "$HERE/bevdirect-serve.service" | sudo tee /etc/systemd/system/bevdirect-serve.service >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now bevdirect-serve
sleep 1
curl -fsS "localhost:$PORT/health" && echo && echo "bevdirect-serve running on :$PORT (journalctl -u bevdirect-serve -f)"
