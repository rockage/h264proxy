#!/usr/bin/env bash
# Foreground macOS TUN client. SSH runs as the invoking user; only the client
# needs elevation. Ctrl-C lets the client restore DNS and routes before exiting.
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
SERVICE="${H264_TUN_SERVICE:-}"
DEVICE="${H264_TUN_DEVICE:-utun123}"
BACKEND="${H264_TUN_BACKEND:-tun2socks}"
case "$BACKEND" in
  tun2socks) FRONTEND="$ROOT/build/tools/tun2socks" ;;
  sing-box) FRONTEND="$ROOT/build/tools/sing-box" ;;
  *) echo 'H264_TUN_BACKEND 仅支持 tun2socks 或 sing-box。' >&2; exit 2 ;;
esac
if [[ "$(uname -s)" != Darwin ]]; then echo '目前自动 TUN 配置仅支持 macOS。' >&2; exit 1; fi
if [[ -z "$SERVICE" ]]; then
  echo '请指定当前网络服务，例如：H264_TUN_SERVICE=Ethernet bash scripts/start_tun.sh' >&2
  /usr/sbin/networksetup -listallnetworkservices
  exit 1
fi
if [[ ! -x "$ROOT/build/proxy" || ! -x "$FRONTEND" ]]; then
  echo '请先运行 bash scripts/build_tun.sh；sing-box 模式还需 bash scripts/build_singbox.sh。' >&2; exit 1
fi
if [[ ! -f "$KEY" ]]; then echo "找不到共享密钥：$KEY" >&2; exit 1; fi
# The existing gateway accepts one client per session. Do not restart it if a
# local SOCKS client is still occupying the requested listener.
h264_check_socks_port "$SOCKS"
mkdir -p "$ROOT/build/logs"
sudo -v
if sudo test -f '/Library/Application Support/H264Proxy/tun-state.json'; then
  echo '检测到上次 TUN 的恢复记录，正在恢复网络配置…'
  sudo "$ROOT/build/proxy" -tun-recover
fi
h264_check_tun_path
if [[ "${H264_RESTART_GATEWAY:-1}" == 1 ]]; then
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" 'systemctl restart h264proxy.service'
fi
echo "启动 TUN：${DEVICE}，前端：${BACKEND}，网络服务：${SERVICE}，传输：${TRANSPORT}；Ctrl-C 停止并恢复网络。"
exec sudo "$ROOT/build/proxy" -role client -net -ice-transport "$TRANSPORT" -http "$ENDPOINT" -stun "$STUN" \
  "${H264_PERF_ARGS[@]}" \
  -key "$KEY" -socks "$SOCKS" -connect-timeout 60s \
  -log "$ROOT/build/logs/tun-${BACKEND}-client.log" \
  -report "$ROOT/build/logs/tun-${BACKEND}-client-report.json" \
  -tun -tun-service "$SERVICE" -tun-device "$DEVICE" \
  -tun-backend "$BACKEND" -sing-box "$ROOT/build/tools/sing-box" \
  -tun2socks "$ROOT/build/tools/tun2socks"
