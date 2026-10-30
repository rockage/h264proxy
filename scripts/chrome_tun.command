#!/bin/bash
# Separate Chrome profiles for TUN-only playback and optional QUIC comparison.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEVICE="${H264_TUN_DEVICE:-utun123}"
route_interface="$(/sbin/route -n get 8.8.8.8 | awk '$1 == "interface:" { print $2 }')"
if [[ "$route_interface" != "$DEVICE" ]]; then
  echo "未检测到 ${DEVICE} 的全局路由，请先启动 TUN 客户端。" >&2
  exit 1
fi
quic_arg=--enable-quic
profile="chrome-tun-quic-profile"
if [[ "${H264_TUN_QUIC:-0}" != 1 ]]; then
  quic_arg=--disable-quic
  profile="chrome-tun-tcp-profile"
fi
/usr/bin/open -na 'Google Chrome' --args \
  --user-data-dir="$ROOT/build/$profile" \
  --no-proxy-server "$quic_arg" \
  --disable-extensions --no-first-run --no-default-browser-check \
  --new-window 'https://www.youtube.com/'
if [[ "${H264_TUN_QUIC:-0}" == 1 ]]; then
  echo '已打开 TUN 对照窗口，允许浏览器尝试 QUIC。'
else
  echo '已打开 TUN 视频窗口，关闭 QUIC，直接使用 TCP。'
fi
