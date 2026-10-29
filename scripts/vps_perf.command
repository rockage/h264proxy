#!/bin/bash
# Double-clickable launcher for the real-network performance test.
# Runs in your own shell, so your ~/.ssh key / agent does the SSH auth.
cd "$(dirname "$0")/.." || exit 1
export SSH="root@152.53.13.153"
export PUBLIC_IP="152.53.13.153"
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
