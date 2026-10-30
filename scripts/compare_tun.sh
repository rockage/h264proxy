#!/usr/bin/env bash
# Run interactively as the normal user; SSH keeps the user's identity.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/client_config.sh"
h264_load_client_config "$ROOT"
h264_require_client_target
if [[ "$(id -u)" == 0 ]]; then echo '请以普通用户运行本脚本；它会单独请求 sudo。' >&2; exit 1; fi
python3 "$ROOT/scripts/compare_tun.py" --preflight "$@"
sudo -v
python3 "$ROOT/scripts/compare_tun.py" "$@"
