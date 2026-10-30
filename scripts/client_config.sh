#!/usr/bin/env bash
# Shared explicit client settings. Environment values override the local file.
# The file is parsed as data: shell commands and variable expansion are not run.
h264_load_client_config() {
  local root="$1" config values name value
  config="${H264_CONFIG:-$root/build/client.env}"
  if [[ ! -f "$config" ]]; then
    if [[ -n "${H264_CONFIG:-}" ]]; then
      echo "找不到客户端配置：$config" >&2
      return 1
    fi
    return 0
  fi
  values="$(python3 - "$config" <<'PY'
import pathlib, shlex, sys
allowed = {
    'H264_ENDPOINT', 'H264_SSH', 'H264_KEY', 'H264_SOCKS', 'H264_STUN', 'H264_ICE_TRANSPORT',
    'H264_TUN_SERVICE', 'H264_TUN_DEVICE', 'H264_TUN_BACKEND', 'H264_RESTART_GATEWAY', 'H264_PUBLIC_IP',
    'H264_DC_HIGH_WATER', 'H264_DC_LOW_WATER', 'H264_STREAM_RECEIVE_WINDOW',
}

path = pathlib.Path(sys.argv[1])
try:
    for number, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        if line.startswith('export '):
            line = line[7:].lstrip()
        key, separator, raw = line.partition('=')
        key = key.strip()
        if not separator or key not in allowed:
            raise ValueError(f'{path}:{number}: 不支持的配置项')
        parts = shlex.split(raw, comments=True, posix=True)
        if len(parts) > 1:
            raise ValueError(f'{path}:{number}: 含空格的值需要引号')
        value = parts[0] if parts else ''
        if any(c in value for c in '\t\r\n\x00'):
            raise ValueError(f'{path}:{number}: 不支持多行配置值')
        print(key + '\t' + value)
except (OSError, ValueError) as error:
    print(error, file=sys.stderr)
    sys.exit(1)
PY
)" || return 1
  while IFS=$'\t' read -r name value; do
    [[ -n "$name" ]] || continue
    if [[ "${!name+x}" != x ]]; then
      printf -v "$name" '%s' "$value"
      export "$name"
    fi
  done <<< "$values"
}

h264_performance_args() {
  local high="${H264_DC_HIGH_WATER:-0}" low="${H264_DC_LOW_WATER:-0}" window="${H264_STREAM_RECEIVE_WINDOW:-0}"
  python3 - "$high" "$low" "$window" <<'PYPERF' || return 1
import sys
try:
    if any(not x.isascii() or not x.isdigit() for x in sys.argv[1:]): raise ValueError()
    high, low, window = map(int, sys.argv[1:])
    if (high or low) and not (32768 <= high <= 8388608 and 0 <= low < high): raise ValueError()
    if window and not 262144 <= window <= 4194304: raise ValueError()
except ValueError:
    sys.exit('性能参数无效：high 为 32 KiB..8 MiB、low < high；窗口为 256 KiB..4 MiB，0 表示默认。')
PYPERF
  H264_PERF_ARGS=(-dc-high-water "$high" -dc-low-water "$low" -stream-receive-window "$window")
}

h264_require_client_target() {
 case "${H264_ICE_TRANSPORT:-udp}" in udp|tcp) ;; *) echo "H264_ICE_TRANSPORT 仅支持 udp 或 tcp。" >&2; return 1;; esac
  if [[ -z "${H264_ENDPOINT:-}" ]]; then
    echo '未设置 VPS 地址。请在 build/client.env 中填写 H264_ENDPOINT 和 H264_SSH，或通过环境变量指定。' >&2
    echo '配置示例：scripts/client.env.example；不再默认连接任何历史测试 VPS。' >&2
    return 1
  fi
  if [[ "${H264_RESTART_GATEWAY:-1}" == 1 && -z "${H264_SSH:-}" ]]; then
    echo '未设置 H264_SSH；自动重启网关需要显式 SSH 目标。无需重启时设置 H264_RESTART_GATEWAY=0。' >&2
    return 1
  fi
	python3 - "$H264_ENDPOINT" "${H264_SSH:-}" <<'PY'
import re, sys, urllib.parse
endpoint, ssh = sys.argv[1:]
try:
    u = urllib.parse.urlsplit('http://' + endpoint)
    if (not u.hostname or u.port is None or not 1 <= u.port <= 65535 or
            u.path or u.query or u.fragment or u.username is not None or
            any(c.isspace() for c in endpoint) or 'YOUR_' in endpoint):
        raise ValueError()
    if ssh and (not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.@:\[\]-]*', ssh) or 'YOUR_' in ssh):
        raise ValueError()
except ValueError:
    print('客户端目标配置无效：请填写真实的 H264_ENDPOINT=主机:端口，以及 H264_SSH=用户@主机。', file=sys.stderr)
    sys.exit(1)
PY
	if [[ "$?" != 0 ]]; then return 1; fi
  printf '连接目标：%s；网关 SSH：%s\n' "$H264_ENDPOINT" "${H264_SSH:-未配置（不重启）}"
}

# Test binding, not merely connectability: an occupied socket need not accept
# connections yet. The proxy itself reserves the listener atomically at startup.
h264_check_socks_port() {
  python3 - "$1" <<'PYPORT'
import socket, sys
address = sys.argv[1]
try:
    host, port = address.rsplit(':', 1)
    host = host.strip('[]')
    family = socket.AF_INET6 if ':' in host else socket.AF_INET
    with socket.socket(family, socket.SOCK_STREAM) as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind((host, int(port)))
except (OSError, ValueError) as error:
    print(f'SOCKS 监听地址 {address} 不可用：{error}。请先停止占用该地址的客户端，或显式设置 H264_SOCKS 使用其他空闲端口。', file=sys.stderr)
    sys.exit(1)
PYPORT
}

# Read-only preflight, before sudo or a shared gateway restart. A comparison
# through another VPN is neither a physical-path test nor safe auto routing.
h264_check_tun_path() {
  python3 - "$H264_ENDPOINT" <<'PYROUTE'
import re, socket, subprocess, sys
def interface(target):
    r = subprocess.run(['/sbin/route', '-n', 'get', target], capture_output=True, text=True, check=True)
    m = re.search(r'^\s*interface:\s*(\S+)', r.stdout, re.M)
    if not m: raise RuntimeError('无法识别出口网卡')
    return m[1]
try:
    host = sys.argv[1].rsplit(':', 1)[0].strip('[]')
    physical = interface('default')
    targets = {'8.8.8.8'} | {x[4][0] for x in socket.getaddrinfo(host, None, socket.AF_INET)}
    for target in targets:
        got = interface(target)
        if physical.startswith('utun') or got != physical:
            raise RuntimeError(f'{target} 使用 {got}，物理默认出口为 {physical}；请先停止其他 VPN/TUN，再启动。尚未重启 VPS 或修改网络。')
except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
    print(error, file=sys.stderr)
    sys.exit(1)
PYROUTE
}
