#!/bin/bash
# A separate Chrome profile with SOCKS5 and local target DNS disabled.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Reuse a manually started client, including one connected to a custom VPS.
# Require a running client; never restart an unrelated default VPS here.
if ! python3 - <<'PY'
import socket
try:
    with socket.create_connection(('127.0.0.1', 1080), timeout=1): pass
except OSError:
    raise SystemExit(1)
PY
then
  echo '未检测到 127.0.0.1:1080 客户端。请先按 INSTALL.md 连接自己的 VPS。' >&2
  exit 1
fi
/usr/bin/open -na 'Google Chrome' --args \
  --user-data-dir="$ROOT/build/chrome-remote-dns-profile" \
  --proxy-server='socks5://127.0.0.1:1080' \
  --host-resolver-rules='MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost' \
  --disable-quic --disable-extensions --no-first-run --no-default-browser-check \
  --new-window 'https://api.ipify.org'
echo '已打开远端 DNS 专用 Chrome 窗口（SOCKS5，本地目标域名解析关闭）；本窗口可以关闭。'
