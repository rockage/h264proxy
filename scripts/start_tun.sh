#!/usr/bin/env bash
# Foreground macOS TUN client. SSH runs as the invoking user; only the client
# needs elevation. Ctrl-C lets the client restore DNS and routes before exiting.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/client_config.sh"
h264_load_client_config "$ROOT"
h264_require_client_target
REMOTE="${H264_SSH:-}"
ENDPOINT="$H264_ENDPOINT"
SOCKS="${H264_SOCKS:-127.0.0.1:1080}"
STUN="${H264_STUN-stun:stun.l.google.com:19302}"
KEY="${H264_KEY:-$ROOT/build/proxy-vps.key}"
SERVICE="${H264_TUN_SERVICE:-}"
DEVICE="${H264_TUN_DEVICE:-utun123}"
if [[ "$(uname -s)" != Darwin ]]; then echo '目前自动 TUN 配置仅支持 macOS。' >&2; exit 1; fi
if [[ -z "$SERVICE" ]]; then
  echo '请指定当前网络服务，例如：H264_TUN_SERVICE=Ethernet bash scripts/start_tun.sh' >&2
  /usr/sbin/networksetup -listallnetworkservices
  exit 1
fi
if [[ ! -x "$ROOT/build/proxy" || ! -x "$ROOT/build/tools/tun2socks" ]]; then
  echo '请先运行 bash scripts/build_tun.sh。' >&2; exit 1
fi
if [[ ! -f "$KEY" ]]; then echo "找不到共享密钥：$KEY" >&2; exit 1; fi
# The existing gateway accepts one client per session. Do not restart it if a
# local SOCKS client is still occupying the requested listener.
if /usr/bin/nc -z "${SOCKS%:*}" "${SOCKS##*:}" 2>/dev/null; then
  echo "SOCKS 端口 $SOCKS 已被使用，请先停止原客户端。" >&2; exit 1
fi
mkdir -p "$ROOT/build/logs"
sudo -v
if sudo test -f '/Library/Application Support/H264Proxy/tun-state.json'; then
  echo '检测到上次 TUN 的恢复记录，正在恢复网络配置…'
  sudo "$ROOT/build/proxy" -tun-recover
fi
if [[ "${H264_RESTART_GATEWAY:-1}" == 1 ]]; then
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" 'systemctl restart h264proxy.service'
fi
echo "启动 TUN：${DEVICE}，网络服务：${SERVICE}；Ctrl-C 停止并恢复网络。"
exec sudo "$ROOT/build/proxy" -role client -net -http "$ENDPOINT" -stun "$STUN" \
  -key "$KEY" -socks "$SOCKS" -connect-timeout 60s \
  -log "$ROOT/build/logs/tun-client.log" \
  -report "$ROOT/build/logs/tun-client-report.json" \
  -tun -tun-service "$SERVICE" -tun-device "$DEVICE" \
  -tun2socks "$ROOT/build/tools/tun2socks"
