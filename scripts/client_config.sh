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
    'H264_ENDPOINT', 'H264_SSH', 'H264_KEY', 'H264_SOCKS', 'H264_STUN',
    'H264_TUN_SERVICE', 'H264_TUN_DEVICE', 'H264_RESTART_GATEWAY', 'H264_PUBLIC_IP',
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

h264_require_client_target() {
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
