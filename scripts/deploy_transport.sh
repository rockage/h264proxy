#!/usr/bin/env bash
# Build and deploy the download-send-buffer trial. No installer/firewall reset.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/client_config.sh"
h264_load_client_config "$ROOT"
h264_require_client_target
if [[ -z "${H264_SSH:-}" ]]; then echo '部署需要显式 H264_SSH。' >&2; exit 2; fi
CHECK=()
if [[ $# == 1 && "$1" == --check ]]; then CHECK=(--check)
elif [[ $# != 0 ]]; then echo '用法：bash scripts/deploy_transport.sh [--check]' >&2; exit 2; fi
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=10)
ARCH="$("${SSH[@]}" "$H264_SSH" uname -m)"
case "$ARCH" in x86_64) GOARCH_TARGET=amd64;; aarch64|arm64) GOARCH_TARGET=arm64;; *) echo '不支持的 VPS 架构。' >&2; exit 1;; esac
OUT="$ROOT/build/transport-deploy"
mkdir -p "$OUT"
cd "$ROOT"
GOOS=linux GOARCH="$GOARCH_TARGET" CGO_ENABLED=0 go build -o "$OUT/proxy-linux-$GOARCH_TARGET" ./cmd/proxy
SHA="$(shasum -a 256 "$OUT/proxy-linux-$GOARCH_TARGET" | awk '{print $1}')"
STAGE="$("${SSH[@]}" "$H264_SSH" 'mktemp -d /tmp/h264-transport.XXXXXXXX')"
[[ "$STAGE" =~ ^/tmp/h264-transport\.[A-Za-z0-9]+$ ]] || { echo '无效的远程暂存目录。' >&2; exit 1; }
cleanup() { "${SSH[@]}" "$H264_SSH" "rm -rf -- $STAGE" || true; }
trap cleanup EXIT
scp -C -q -o BatchMode=yes -o ConnectTimeout=10 "$OUT/proxy-linux-$GOARCH_TARGET" "$H264_SSH:$STAGE/proxy"
scp -q -o BatchMode=yes -o ConnectTimeout=10 "$ROOT/scripts/deploy/update_transport.py" "$H264_SSH:$STAGE/update_transport.py"
"${SSH[@]}" "$H264_SSH" "chmod 755 $STAGE/proxy"
# Always do read-only validation before the mutation/restart stage.
"${SSH[@]}" "$H264_SSH" "python3 $STAGE/update_transport.py $STAGE/proxy $SHA --check"
if [[ ${#CHECK[@]} == 0 ]]; then
  echo '重启网关并应用发送水位 1 MiB / 512 KiB；原客户端需要重新连接。'
  "${SSH[@]}" "$H264_SSH" "python3 $STAGE/update_transport.py $STAGE/proxy $SHA"
fi
