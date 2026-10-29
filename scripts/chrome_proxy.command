#!/bin/bash
# A separate Chrome profile with an explicit SOCKS5 proxy; no system changes.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
bash "$ROOT/scripts/start_client_background.sh"
python3 - <<'PY'
import socket, time
for _ in range(60):
    try:
        with socket.create_connection(('127.0.0.1', 1080), timeout=1):
            break
    except OSError:
        time.sleep(1)
else:
    raise SystemExit('客户端尚未连通，请查看 build/logs/proxy-client-console.log')
PY
/usr/bin/open -na 'Google Chrome' --args \
  --user-data-dir="$ROOT/build/chrome-proxy-profile" \
  --proxy-server='socks5://127.0.0.1:1080' \
  --disable-quic --disable-extensions --no-first-run --no-default-browser-check \
  --new-window 'https://api.ipify.org'
echo '已打开使用 SOCKS5 的独立 Chrome 窗口；本窗口可以关闭。'
