#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PIDFILE="$ROOT/build/proxy-client.pid"
DOMAIN="gui/$(id -u)"
LABEL="com.roca.h264proxy.client"
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
  launchctl bootout "$DOMAIN/$LABEL"
  echo '已停止 macOS 后台客户端。'
  exit 0
fi
if [[ ! -f "$PIDFILE" ]]; then echo '客户端未运行。'; exit 0; fi
pid="$(cat "$PIDFILE")"
if [[ ! "$pid" =~ ^[0-9]+$ ]]; then echo 'PID 文件无效。' >&2; exit 1; fi
command_line="$(ps -p "$pid" -o command= 2>/dev/null || true)"
case "$command_line" in
  *"$ROOT/build/proxy -role client"*) kill -TERM "$pid"; echo '已停止本机客户端。' ;;
  *) echo '原客户端已退出。'; rm -f "$PIDFILE" ;;
esac
