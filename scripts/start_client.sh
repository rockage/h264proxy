#!/usr/bin/env bash
# Foreground client; SSH prepares a fresh session on the paired gateway.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/client_config.sh"
h264_load_client_config "$ROOT"
h264_require_client_target
h264_performance_args
REMOTE="${H264_SSH:-}"
ENDPOINT="$H264_ENDPOINT"
TRANSPORT="${H264_ICE_TRANSPORT:-udp}"
SOCKS="${H264_SOCKS:-127.0.0.1:1080}"
STUN="${H264_STUN-stun:stun.l.google.com:19302}"
KEY="${H264_KEY:-$ROOT/build/proxy-vps.key}"
BIN="$ROOT/build/proxy"
PIDFILE="$ROOT/build/proxy-client.pid"
LOCK="$ROOT/build/proxy-client.lock"
umask 077
mkdir -p "$ROOT/build/logs"
if [[ ! -x "$BIN" || ! -f "$KEY" ]]; then
  echo "缺少客户端程序或共享密钥，请先完成 VPS 部署。" >&2
  exit 1
fi
# Reject an already active TUN/SOCKS listener before restarting the shared VPS.
h264_check_socks_port "$SOCKS"
if ! mkdir "$LOCK" 2>/dev/null; then
  owner="$(cat "$LOCK/owner" 2>/dev/null || true)"
  if [[ "$owner" =~ ^[0-9]+$ ]] && kill -0 "$owner" 2>/dev/null; then
    echo "客户端已经运行或正在启动；SOCKS5：$SOCKS"
    echo "日志：$ROOT/build/logs/proxy-client-console.log"
    exit 0
  fi
  if [[ -z "$owner" ]]; then
    echo "客户端正在启动；如启动曾被强制中断，请删除 $LOCK 后重试。" >&2
    exit 1
  fi
  rm -f "$LOCK/owner"
  rmdir "$LOCK"
  mkdir "$LOCK"
fi
printf '%s\n' "$$" > "$LOCK/owner"
client_pid=""
cleanup() {
  if [[ -n "$client_pid" ]]; then
    kill -TERM "$client_pid" 2>/dev/null || true
    wait "$client_pid" 2>/dev/null || true
  fi
  rm -f "$PIDFILE" "$LOCK/owner"
  rmdir "$LOCK" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# The current gateway pairs with one client. Restart it for a fresh SDP session.
echo "准备 VPS 网关会话：$REMOTE"
if [[ "${H264_RESTART_GATEWAY:-1}" == 1 ]]; then
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" 'systemctl restart h264proxy.service'
fi
echo "启动旁路模式，SOCKS5：${SOCKS}（此窗口保持运行；Ctrl-C 停止）"
"$BIN" -role client -bypass -net -ice-transport "$TRANSPORT" -http "$ENDPOINT" -stun "$STUN" \
  "${H264_PERF_ARGS[@]}" \
  -key "$KEY" -socks "$SOCKS" -connect-timeout 60s \
  -log "$ROOT/build/logs/proxy-client.log" \
  -report "$ROOT/build/logs/proxy-client-report.json" &
client_pid=$!
printf '%s\n' "$client_pid" > "$PIDFILE"
wait "$client_pid"
client_pid=""
