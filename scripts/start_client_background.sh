#!/usr/bin/env bash
# Keep the manual-test client alive independently of its launching terminal.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOMAIN="gui/$(id -u)"
LABEL="com.roca.h264proxy.client"
PLIST="$ROOT/build/deploy/$LABEL.plist"
job_state="$(launchctl print "$DOMAIN/$LABEL" 2>/dev/null || true)"
if [[ "$job_state" == *'state = running'* ]]; then
  echo '客户端已经运行或正在启动。SOCKS5：127.0.0.1:1080'
  exit 0
fi
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
  # A just-stopped job can disappear between print and bootout.
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
fi
# Allow the old launcher to finish its EXIT cleanup before the new job starts.
owner="$(cat "$ROOT/build/proxy-client.lock/owner" 2>/dev/null || true)"
if [[ "$owner" =~ ^[0-9]+$ ]]; then
  for ((attempt=0; attempt<50; attempt++)); do
    if ! kill -0 "$owner" 2>/dev/null; then break; fi
    sleep 0.1
  done
  if kill -0 "$owner" 2>/dev/null; then
    echo '原客户端仍在停止或前台运行，请稍后重试。' >&2
    exit 1
  fi
fi
mkdir -p "$ROOT/build/deploy" "$ROOT/build/logs"
python3 - "$ROOT" <<'PY'
import pathlib, plistlib, sys
root = pathlib.Path(sys.argv[1])
config = {
    'Label': 'com.roca.h264proxy.client',
    'ProgramArguments': ['/bin/bash', str(root / 'scripts/start_client.sh')],
    'WorkingDirectory': str(root),
    'RunAtLoad': True,
    'KeepAlive': False,
    'ProcessType': 'Interactive',
    'EnvironmentVariables': {'HOME': str(pathlib.Path.home()), 'PATH': '/usr/bin:/bin:/usr/sbin:/sbin'},
    'StandardOutPath': str(root / 'build/logs/proxy-client-console.log'),
    'StandardErrorPath': str(root / 'build/logs/proxy-client-console.log'),
}
with (root / 'build/deploy/com.roca.h264proxy.client.plist').open('wb') as f:
    plistlib.dump(config, f)
PY
launchctl bootstrap "$DOMAIN" "$PLIST"
echo '客户端已交给 macOS 后台运行，连接后 SOCKS5 为 127.0.0.1:1080。'
echo "日志：$ROOT/build/logs/proxy-client-console.log"
