#!/usr/bin/env bash
# Build the existing client plus a pinned, independent tun2socks executable.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
mkdir -p build/tools
go build -o build/proxy ./cmd/proxy
GOBIN="$ROOT/build/tools" go install github.com/xjasonlyu/tun2socks/v2@v2.6.0
echo "Built: $ROOT/build/proxy"
echo "Built: $ROOT/build/tools/tun2socks (v2.6.0)"
