#!/usr/bin/env bash
# Explicit client mode selection; each existing launcher owns its lifecycle.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
usage() {
  cat <<'HELP'
用法：bash scripts/start_proxy.sh --bypass | --tun | --sing-box

  --bypass  旁路模式：仅启动 SOCKS5，由浏览器/应用选择使用；不修改系统路由和 DNS。
  --tun     TUN 模式：接管系统流量和 DNS；需要 sudo，退出时恢复网络。
  --sing-box  sing-box TUN/DNS/自动路由前端；复用同一 WebRTC 传输，需要 sudo。
  --help    显示帮助。

各模式互斥；切换前请先停止原客户端。
共用 build/client.env 中的服务器、密钥和 TCP/UDP 传输配置。
TUN 的 H264_TUN_SERVICE 必须是当前实际联网的网络服务名称。
HELP
}
if [[ $# != 1 ]]; then usage >&2; exit 2; fi
case "$1" in
  --bypass) exec /bin/bash "$ROOT/scripts/start_client.sh" ;;
  --tun) H264_TUN_BACKEND=tun2socks exec /bin/bash "$ROOT/scripts/start_tun.sh" ;;
  --sing-box) H264_TUN_BACKEND=sing-box exec /bin/bash "$ROOT/scripts/start_tun.sh" ;;
  --help|-h) usage ;;
  *) usage >&2; exit 2 ;;
esac
