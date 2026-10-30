#!/bin/bash
# Double-clickable launcher for the real-network performance test.
# Runs in your own shell, so your ~/.ssh key / agent does the SSH auth.
cd "$(dirname "$0")/.." || exit 1
ROOT="$(pwd)"
source "$ROOT/scripts/client_config.sh"
h264_load_client_config "$ROOT" || exit 1
export SSH="${SSH:-${H264_SSH:-}}"
export PUBLIC_IP="${PUBLIC_IP:-${H264_PUBLIC_IP:-}}"
if [[ -z "$SSH" || -z "$PUBLIC_IP" ]]; then
  echo '请显式设置 SSH 和 PUBLIC_IP（或配置 H264_SSH、H264_PUBLIC_IP），不再默认选择历史测试 VPS。' >&2
  exit 1
fi
export SIG_PORT="9300"
export UDP_PORT="9301"
echo "=== H264Proxy VPS performance test ==="
echo "server: $SSH   ports: $SIG_PORT/tcp $UDP_PORT/udp"
echo
bash scripts/vps_perf.sh
status=$?
echo
echo "=== finished (exit $status) — you can close this window ==="
read -r -p "Press Return to close..." _
