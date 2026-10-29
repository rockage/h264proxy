#!/usr/bin/env bash
# H264Proxy native Linux gateway installer. No credentials are bundled.
# curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh | bash
set -Eeuo pipefail
export LC_ALL=C
umask 077

REPOSITORY=rockage/h264proxy
SERVICE=h264proxy.service
APP_USER=h264proxy
INSTALL_DIR=/opt/h264proxy
CONFIG_DIR=/etc/h264proxy
STATE_DIR=/var/lib/h264proxy
UNIT_DIR=/etc/systemd/system
LOCK_FILE=/run/lock/h264proxy-install.lock
WORK_DIR=""
ROLLBACK_NEEDED=0
WAS_ACTIVE=0
WAS_ENABLED=0
FIREWALL_UNDO=()
PUBLIC_IP="${PUBLIC_IP:-}"
PORT="${PORT:-}"
REF=main
MANAGE_FIREWALL=1

info() { printf '[h264proxy] %s\n' "$*"; }
warn() { printf '[h264proxy] WARNING: %s\n' "$*" >&2; }
die() { printf '[h264proxy] ERROR: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'HELP'
H264Proxy gateway installer (Linux + systemd, root required)

Usage: bash install.sh [options]
  --public-ip IPv4   Public IPv4 advertised by ICE (otherwise preserve/detect)
  --port PORT        TCP signaling and UDP media port (otherwise preserve/9300)
  --ref REF          GitHub branch, tag or commit to build (default: main)
  --no-firewall      Do not change host firewall rules
  -h, --help         Print help without changing the system

Existing 32-byte keys are never replaced. Re-running installs/updates the
service and restarts its current session. Cloud security groups must also
allow the TCP and UDP port. Go is reused or installed privately for this app.
HELP
}
parse_args() {
  while (($#)); do
    case "$1" in
      --public-ip) [[ $# -ge 2 && -n "$2" ]] || die '--public-ip requires a value'; PUBLIC_IP="$2"; shift 2 ;;
      --public-ip=*) PUBLIC_IP="${1#*=}"; [[ -n "$PUBLIC_IP" ]] || die '--public-ip requires a value'; shift ;;
      --port) [[ $# -ge 2 && -n "$2" ]] || die '--port requires a value'; PORT="$2"; shift 2 ;;
      --port=*) PORT="${1#*=}"; [[ -n "$PORT" ]] || die '--port requires a value'; shift ;;
      --ref) [[ $# -ge 2 && -n "$2" ]] || die '--ref requires a value'; REF="$2"; shift 2 ;;
      --ref=*) REF="${1#*=}"; [[ -n "$REF" ]] || die '--ref requires a value'; shift ;;
      --no-firewall) MANAGE_FIREWALL=0; shift ;;
      *) die "Unknown option: $1 (use --help)" ;;
    esac
  done
  [[ "$REF" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ && "$REF" != *..* && ${#REF} -le 128 ]] || die 'Invalid source ref'
  if [[ -n "$PORT" ]]; then validate_port; fi
}
validate_port() {
  [[ "$PORT" =~ ^[0-9]{1,5}$ ]] || die 'Port must be an integer from 1 to 65535'
  PORT=$((10#$PORT))
  ((PORT >= 1 && PORT <= 65535)) || die 'Port must be from 1 to 65535'
}
check_system() {
  [[ "$(uname -s)" == Linux ]] || die 'This installer requires Linux'
  [[ "$EUID" == 0 ]] || die 'Run as root, or pipe the script to sudo bash'
  command -v systemctl >/dev/null || die 'systemd is required'
  systemctl show --property=Version --value >/dev/null 2>&1 || die 'systemd must be running (ordinary containers/OpenRC are not supported)'
  case "$(uname -m)" in
    x86_64) GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) die 'Supported CPUs: x86_64/amd64 and aarch64/arm64' ;;
  esac
}
ensure_dependencies() {
  local missing=0 command
  for command in curl tar gzip python3 ss ip flock getent useradd groupadd sha256sum install; do
    command -v "$command" >/dev/null || missing=1
  done
  if [[ ! -f /etc/ssl/certs/ca-certificates.crt && ! -f /etc/pki/tls/certs/ca-bundle.crt ]]; then missing=1; fi
  if ((missing == 0)); then return; fi
  info 'Installing required system packages ...'
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends curl ca-certificates tar gzip python3 iproute2 util-linux passwd coreutils
  elif command -v dnf >/dev/null; then
    dnf install -y curl ca-certificates tar gzip python3 iproute util-linux shadow-utils coreutils
  elif command -v yum >/dev/null; then
    yum install -y curl ca-certificates tar gzip python3 iproute util-linux shadow-utils coreutils
  else
    die 'Install curl, CA certificates, tar/gzip, python3, iproute2, util-linux and shadow/passwd tools first'
  fi
}
acquire_lock() {
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec 9>"$LOCK_FILE"
  flock -n 9 || die 'Another installer is running'
}
fetch() {
  curl --proto '=https' --tlsv1.2 -fLsS --retry 2 --connect-timeout 15 --max-time 240 "$1" -o "$2"
}
# Parse EnvironmentFile values without executing a root-owned configuration.
read_setting() {
  python3 - "$CONFIG_DIR/gateway.env" "$1" <<'PY'
import pathlib, shlex, sys
p = pathlib.Path(sys.argv[1])
if p.is_file():
    for line in reversed(p.read_text().splitlines()):
        if '=' not in line or line.lstrip().startswith('#'): continue
        key, value = line.split('=', 1)
        if key.strip() == sys.argv[2]:
            values = shlex.split(value, comments=True)
            if len(values) != 1: raise SystemExit('Invalid gateway.env value')
            print(values[0])
            break
PY
}
validate_ip() {
  python3 - "$1" <<'PY'
import ipaddress, sys
try:
    ip = ipaddress.IPv4Address(sys.argv[1])
    if not ip.is_global or ip.is_multicast: raise ValueError()
except ValueError:
    raise SystemExit('A globally routable public IPv4 is required; use --public-ip')
PY
}
resolve_configuration() {
  [[ ! -L "$CONFIG_DIR" && ! -L "$CONFIG_DIR/key.bin" && ! -L "$CONFIG_DIR/gateway.env" ]] || die 'Managed config/key paths must not be symbolic links'
  if [[ -f "$CONFIG_DIR/key.bin" ]]; then
    [[ "$(wc -c < "$CONFIG_DIR/key.bin")" -eq 32 ]] || die 'Existing key is not 32 bytes; refusing to replace it'
  fi
  if [[ -z "$PUBLIC_IP" ]]; then PUBLIC_IP="$(read_setting PUBLIC_IP)"; fi
  if [[ -z "$PORT" ]]; then PORT="$(read_setting UDP_PORT)"; fi
  PORT="${PORT:-9300}"
  validate_port
  # Keep custom signaling binds rather than silently changing an old install.
  local old_http old_udp
  old_http="$(read_setting HTTP_ADDR)"
  old_udp="$(read_setting UDP_PORT)"
  if [[ -n "$old_http" && "$old_http" != "0.0.0.0:${old_udp}" ]]; then
    die 'Custom HTTP_ADDR detected. This one-port installer expects HTTP_ADDR=0.0.0.0:UDP_PORT; use manual deployment for custom binds'
  fi
  if [[ -z "$PUBLIC_IP" ]]; then
    local endpoint candidate
    for endpoint in https://api.ipify.org https://ipv4.icanhazip.com; do
      if candidate="$(curl -4 --proto '=https' --tlsv1.2 -fLsS --connect-timeout 5 --max-time 10 "$endpoint")" && validate_ip "$candidate" 2>/dev/null; then
        PUBLIC_IP="$candidate"; break
      fi
    done
  fi
  [[ -n "$PUBLIC_IP" ]] || die 'Could not detect public IPv4; pass --public-ip YOUR_IP'
  validate_ip "$PUBLIC_IP" || die 'Invalid public IPv4'
}
check_ports() {
  local pid=0 line sockets
  if systemctl is-active --quiet "$SERVICE"; then pid="$(systemctl show "$SERVICE" --property=MainPID --value)"; fi
  sockets="$(ss -H -lntp "sport = :$PORT"; ss -H -lnup "sport = :$PORT")"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    if [[ "$pid" == 0 || "$line" != *"pid=$pid,"* ]]; then
      die "Port $PORT is already occupied by another service. Choose --port OTHER_PORT"
    fi
  done <<< "$sockets"
}
download_source() {
  if [[ "$REF" =~ ^[0-9a-fA-F]{40}$ ]]; then
    SOURCE_SHA="$(printf '%s' "$REF" | tr 'A-F' 'a-f')"
  else
    local quoted
    quoted="$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$REF")"
    fetch "https://api.github.com/repos/$REPOSITORY/commits/$quoted" "$WORK_DIR/commit.json"
    SOURCE_SHA="$(python3 - "$WORK_DIR/commit.json" <<'PY'
import json, re, sys
s = json.load(open(sys.argv[1])).get('sha', '')
if not re.fullmatch('[0-9a-f]{40}', s): raise SystemExit('GitHub did not return a commit SHA')
print(s)
PY
)"
  fi
  info "Downloading $REPOSITORY at $SOURCE_SHA ..."
  fetch "https://codeload.github.com/$REPOSITORY/tar.gz/$SOURCE_SHA" "$WORK_DIR/source.tar.gz"
  mkdir "$WORK_DIR/source"
  tar --no-same-owner --no-same-permissions -xzf "$WORK_DIR/source.tar.gz" --strip-components=1 -C "$WORK_DIR/source"
  REQUIRED_GO="$(awk '$1 == "go" {print $2;exit}' "$WORK_DIR/source/go.mod")"
  [[ "$REQUIRED_GO" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'Missing/invalid minimum Go version in downloaded source'
}
version_satisfies() {
  python3 - "$1" "$2" <<'PY'
import re, sys
try:
    values = [tuple(map(int, re.fullmatch(r'(?:go)?(\d+)\.(\d+)\.(\d+)', s).groups())) for s in sys.argv[1:]]
except (ValueError, AttributeError): raise SystemExit(1)
raise SystemExit(0 if values[0] >= values[1] else 1)
PY
}
usable_go() {
  local candidate="$1" version host
  [[ -x "$candidate" ]] || return 1
  version="$(GOTOOLCHAIN=local "$candidate" version 2>/dev/null)" || return 1
  version="$(printf '%s\n' "$version" | awk '{print $3}')"
  version_satisfies "$version" "$REQUIRED_GO" || return 1
  host="$(GOTOOLCHAIN=local "$candidate" env GOHOSTOS GOHOSTARCH 2>/dev/null)" || return 1
  [[ "$host" == "$(printf 'linux\n%s' "$GO_ARCH")" ]] || return 1
  GO_BIN="$candidate"
  return 0
}
ensure_go() {
  local candidate
  candidate="$(command -v go || true)"
  if usable_go "$candidate" || usable_go /usr/local/go/bin/go || usable_go "$INSTALL_DIR/toolchains/current/bin/go"; then
    info "Using $(GOTOOLCHAIN=local "$GO_BIN" version)"
    return
  fi
  info "Installing a private Go toolchain (minimum $REQUIRED_GO); system Go is unchanged ..."
  fetch 'https://go.dev/dl/?mode=json' "$WORK_DIR/go-downloads.json"
  local filename checksum version
  read -r filename checksum version < <(python3 - "$WORK_DIR/go-downloads.json" "$GO_ARCH" "$REQUIRED_GO" <<'PY'
import json, re, sys
minimum = tuple(map(int, sys.argv[3].split('.')))
for release in json.load(open(sys.argv[1])):
    version = release.get('version', '')
    m = re.fullmatch(r'go(\d+)\.(\d+)\.(\d+)', version)
    if not release.get('stable') or not m or tuple(map(int, m.groups())) < minimum: continue
    for f in release.get('files', []):
        if f.get('os') != 'linux' or f.get('arch') != sys.argv[2] or f.get('kind') != 'archive': continue
        name, digest = f.get('filename', ''), f.get('sha256', '')
        if name != version + '.linux-' + sys.argv[2] + '.tar.gz' or not re.fullmatch('[0-9a-f]{64}', digest): raise SystemExit('Invalid Go download metadata')
        print(name, digest, version)
        raise SystemExit(0)
raise SystemExit('No suitable stable Go archive found')
PY
) || die 'Could not select a verified Go download'
  fetch "https://go.dev/dl/$filename" "$WORK_DIR/go.tar.gz"
  printf '%s  %s\n' "$checksum" "$WORK_DIR/go.tar.gz" | sha256sum -c -
  mkdir "$WORK_DIR/toolchain"
  tar --no-same-owner --no-same-permissions -xzf "$WORK_DIR/go.tar.gz" -C "$WORK_DIR/toolchain"
  candidate="$WORK_DIR/toolchain/go/bin/go"
  usable_go "$candidate" || die 'Downloaded Go cannot run on this host'
  mkdir -p "$INSTALL_DIR/toolchains"
  # Each toolchain gets a fresh directory; never unpack over a system Go tree.
  local destination="$INSTALL_DIR/toolchains/$version-$(date +%s)-$$"
  mv "$WORK_DIR/toolchain/go" "$destination"
  ln -s "$destination" "$INSTALL_DIR/toolchains/.current-$$"
  python3 -c 'import os,sys;os.replace(sys.argv[1],sys.argv[2])' "$INSTALL_DIR/toolchains/.current-$$" "$INSTALL_DIR/toolchains/current"
  GO_BIN="$destination/bin/go"
}
build_binaries() {
  info "Building native Linux/$GO_ARCH gateway ..."
  (
    cd "$WORK_DIR/source"
    GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$GO_ARCH" "$GO_BIN" build -mod=readonly -trimpath -o "$WORK_DIR/proxy" ./cmd/proxy
    GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$GO_ARCH" "$GO_BIN" build -mod=readonly -trimpath -o "$WORK_DIR/genkey" ./cmd/genkey
  )
  "$WORK_DIR/proxy" -h >/dev/null 2>&1
}
ensure_user() {
  if ! getent group "$APP_USER" >/dev/null; then groupadd --system "$APP_USER"; fi
  if ! getent passwd "$APP_USER" >/dev/null; then
    useradd --system --gid "$APP_USER" --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$APP_USER"
  fi
  [[ "$(id -u "$APP_USER")" != 0 ]] || die 'The service account must not be root'
}
backup_file() {
  if [[ -e "$1" ]]; then
    [[ -f "$1" && ! -L "$1" ]] || die "Managed path is not a regular file: $1"
    cp -p "$1" "$WORK_DIR/backup/$2"
  fi
}
restore_file() {
  if [[ -f "$WORK_DIR/backup/$2" ]]; then
    cp -p "$WORK_DIR/backup/$2" "$1"
  else
    rm -f "$1"
  fi
}
restore_labels() {
  if command -v restorecon >/dev/null; then
    restorecon -RF "$INSTALL_DIR/bin" "$CONFIG_DIR" "$STATE_DIR" "$UNIT_DIR/$SERVICE"
  fi
}
prepare_install() {
  ensure_user
  install -d -m 755 "$INSTALL_DIR" "$INSTALL_DIR/bin" "$UNIT_DIR"
  install -d -m 750 -o root -g "$APP_USER" "$CONFIG_DIR"
  install -d -m 750 -o "$APP_USER" -g "$APP_USER" "$STATE_DIR"
  if [[ ! -e "$CONFIG_DIR/key.bin" ]]; then "$WORK_DIR/genkey" -out "$CONFIG_DIR/key.bin"; fi
  chown "root:$APP_USER" "$CONFIG_DIR/key.bin"
  chmod 640 "$CONFIG_DIR/key.bin"
  mkdir "$WORK_DIR/backup"
  backup_file "$INSTALL_DIR/bin/proxy" proxy
  backup_file "$INSTALL_DIR/bin/genkey" genkey
  backup_file "$CONFIG_DIR/gateway.env" gateway.env
  backup_file "$UNIT_DIR/$SERVICE" service
  backup_file "$INSTALL_DIR/install-info.json" install-info.json
  if systemctl is-active --quiet "$SERVICE"; then WAS_ACTIVE=1; fi
  if systemctl is-enabled --quiet "$SERVICE"; then WAS_ENABLED=1; fi
  python3 - "$CONFIG_DIR/gateway.env" "$WORK_DIR/gateway.env" "$PUBLIC_IP" "$PORT" <<'PY'
import pathlib, sys
old = pathlib.Path(sys.argv[1])
lines = old.read_text().splitlines() if old.exists() else []
keys = {'PUBLIC_IP', 'UDP_PORT', 'HTTP_ADDR'}
lines = [s for s in lines if s.split('=', 1)[0].strip() not in keys]
lines += ['PUBLIC_IP=' + sys.argv[3], 'UDP_PORT=' + sys.argv[4], 'HTTP_ADDR=0.0.0.0:' + sys.argv[4]]
pathlib.Path(sys.argv[2]).write_text('\n'.join(lines) + '\n')
PY
  cat > "$WORK_DIR/$SERVICE" <<UNIT
[Unit]
Description=H264Proxy WebRTC TCP Gateway
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$APP_USER
Group=$APP_USER
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$CONFIG_DIR/gateway.env
ExecStart=$INSTALL_DIR/bin/proxy -role gateway -net -public-ip \${PUBLIC_IP} -udp-port \${UDP_PORT} -http \${HTTP_ADDR} -key $CONFIG_DIR/key.bin -connect-timeout 24h -report $STATE_DIR/gateway-report.json
Restart=always
RestartSec=2
TimeoutStopSec=15
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$STATE_DIR
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
UMask=0077
UNIT
  if ((PORT < 1024)); then
    printf 'AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n' >> "$WORK_DIR/$SERVICE"
  fi
  printf '\n[Install]\nWantedBy=multi-user.target\n' >> "$WORK_DIR/$SERVICE"
  python3 - "$WORK_DIR/install-info.json" "$SOURCE_SHA" "$REPOSITORY" "$GO_BIN" <<'PY'
import datetime, json, pathlib, subprocess, sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({'repository':sys.argv[3], 'commit':sys.argv[2], 'go':subprocess.check_output([sys.argv[4], 'version'], text=True).strip(), 'installed_at':datetime.datetime.now(datetime.timezone.utc).isoformat()}, indent=2) + '\n')
PY
}
configure_firewall() {
  ((MANAGE_FIREWALL)) || { info "Host firewall unchanged; allow $PORT/tcp and $PORT/udp yourself"; return; }
  local protocol zone interface
  if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    interface="$(ip -4 route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++)if($i=="dev"){print $(i+1);exit}}' || true)"
    zone=""
    if [[ -n "$interface" ]]; then zone="$(firewall-cmd --get-zone-of-interface="$interface" 2>/dev/null || true)"; fi
    if [[ -z "$zone" || "$zone" == 'no zone' ]]; then zone="$(firewall-cmd --get-default-zone)"; fi
    [[ "$zone" =~ ^[A-Za-z0-9_-]+$ ]] || die 'Could not determine firewalld zone'
    for protocol in tcp udp; do
      if ! firewall-cmd --zone="$zone" --query-port="$PORT/$protocol" >/dev/null; then
        FIREWALL_UNDO+=("firewalld:$zone:$protocol:runtime")
        firewall-cmd --zone="$zone" --add-port="$PORT/$protocol"
      fi
      if ! firewall-cmd --permanent --zone="$zone" --query-port="$PORT/$protocol" >/dev/null; then
        FIREWALL_UNDO+=("firewalld:$zone:$protocol:permanent")
        firewall-cmd --permanent --zone="$zone" --add-port="$PORT/$protocol"
      fi
    done
    info "Opened $PORT/tcp and $PORT/udp in firewalld zone $zone (runtime + permanent)"
  elif command -v ufw >/dev/null && ufw status | grep -q '^Status: active'; then
    for protocol in tcp udp; do
      if ! ufw status | grep -Eq "^$PORT/$protocol[[:space:]]+ALLOW([[:space:]]+IN)?[[:space:]]+Anywhere([[:space:]]|$)"; then
        FIREWALL_UNDO+=("ufw:unused:$protocol:runtime")
        ufw allow "$PORT/$protocol"
      fi
    done
    info "Opened $PORT/tcp and $PORT/udp in the already-active UFW"
  else
    warn "No active firewalld/UFW found. Other host firewalls are unchanged; allow $PORT/tcp and $PORT/udp if required"
  fi
}
undo_firewall() {
  local item manager zone protocol mode
  for item in "${FIREWALL_UNDO[@]:-}"; do
    [[ -n "$item" ]] || continue
    IFS=: read -r manager zone protocol mode <<< "$item"
    if [[ "$manager" == firewalld ]]; then
      if [[ "$mode" == permanent ]]; then
        firewall-cmd --permanent --zone="$zone" --remove-port="$PORT/$protocol" >/dev/null 2>&1
      else
        firewall-cmd --zone="$zone" --remove-port="$PORT/$protocol" >/dev/null 2>&1
      fi
    else
      ufw --force delete allow "$PORT/$protocol" >/dev/null 2>&1
    fi
  done
}
install_files() {
  ROLLBACK_NEEDED=1
  install -m 755 "$WORK_DIR/proxy" "$INSTALL_DIR/bin/proxy.next"
  install -m 755 "$WORK_DIR/genkey" "$INSTALL_DIR/bin/genkey.next"
  mv -f "$INSTALL_DIR/bin/proxy.next" "$INSTALL_DIR/bin/proxy"
  mv -f "$INSTALL_DIR/bin/genkey.next" "$INSTALL_DIR/bin/genkey"
  install -m 640 -o root -g "$APP_USER" "$WORK_DIR/gateway.env" "$CONFIG_DIR/gateway.env"
  install -m 644 "$WORK_DIR/$SERVICE" "$UNIT_DIR/$SERVICE"
  restore_labels
  systemd-analyze verify "$UNIT_DIR/$SERVICE"
  configure_firewall
  systemctl daemon-reload
  systemctl enable "$SERVICE"
  systemctl restart "$SERVICE"
}
wait_ready() {
  local attempt pid
  for ((attempt=0; attempt<30; attempt++)); do
    if systemctl is-active --quiet "$SERVICE"; then
      pid="$(systemctl show "$SERVICE" --property=MainPID --value)"
      if curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 "http://127.0.0.1:$PORT/offer" -o "$WORK_DIR/offer.json" && python3 - "$WORK_DIR/offer.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1])).get('sdp', {})
raise SystemExit(0 if s.get('type') == 'offer' and 'H264/90000' in s.get('sdp', '') and 'm=video' in s['sdp'] else 1)
PY
      then
        sleep 1
        if systemctl is-active --quiet "$SERVICE" && [[ "$pid" == "$(systemctl show "$SERVICE" --property=MainPID --value)" ]]; then return; fi
      fi
    fi
    sleep 1
  done
  journalctl -u "$SERVICE" -n 25 --no-pager >&2 || true
  die 'Gateway did not become healthy; rolling back'
}
rollback() {
  warn 'Installation failed; restoring previous binaries, configuration and service state'
  systemctl stop "$SERVICE" >/dev/null 2>&1
  if ((WAS_ENABLED == 0)); then systemctl disable "$SERVICE" >/dev/null 2>&1; fi
  restore_file "$INSTALL_DIR/bin/proxy" proxy
  restore_file "$INSTALL_DIR/bin/genkey" genkey
  restore_file "$CONFIG_DIR/gateway.env" gateway.env
  restore_file "$UNIT_DIR/$SERVICE" service
  restore_file "$INSTALL_DIR/install-info.json" install-info.json
  rm -f "$INSTALL_DIR/bin/proxy.next" "$INSTALL_DIR/bin/genkey.next"
  restore_labels >/dev/null 2>&1
  undo_firewall
  systemctl daemon-reload
  if ((WAS_ENABLED)); then systemctl enable "$SERVICE" >/dev/null 2>&1; fi
  if ((WAS_ACTIVE)); then systemctl restart "$SERVICE"; fi
}
cleanup() {
  local status=$?
  trap - EXIT ERR
  set +e
  if ((status != 0 && ROLLBACK_NEEDED)); then rollback; fi
  if [[ "$WORK_DIR" == /tmp/h264proxy-install.* && -d "$WORK_DIR" ]]; then rm -rf -- "$WORK_DIR"; fi
  exit "$status"
}
main() {
  for arg in "$@"; do if [[ "$arg" == -h || "$arg" == --help ]]; then usage; return; fi; done
  parse_args "$@"
  check_system
  ensure_dependencies
  acquire_lock
  WORK_DIR="$(mktemp -d /tmp/h264proxy-install.XXXXXXXX)"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'printf "[h264proxy] Failed at installer line %s (exit %s)\n" "$LINENO" "$?" >&2' ERR
  resolve_configuration
  check_ports
  download_source
  ensure_go
  build_binaries
  prepare_install
  install_files
  wait_ready
  install -m 644 "$WORK_DIR/install-info.json" "$INSTALL_DIR/install-info.json"
  mkdir -p "$INSTALL_DIR/sources"
  if [[ ! -e "$INSTALL_DIR/sources/$SOURCE_SHA" ]]; then cp -R "$WORK_DIR/source" "$INSTALL_DIR/sources/$SOURCE_SHA"; fi
  ROLLBACK_NEEDED=0
  info "INSTALLED: $REPOSITORY ($SOURCE_SHA)"
  info "Gateway: $PUBLIC_IP:$PORT; allow $PORT/TCP + $PORT/UDP in any cloud firewall"
  info "Shared key: $CONFIG_DIR/key.bin (copy to your client over SSH/scp; never publish it)"
  info "Status: systemctl status $SERVICE --no-pager"
  info "Logs: journalctl -u $SERVICE -f"
  info "Client: proxy -role client -net -http $PUBLIC_IP:$PORT -key ./key.bin -socks 127.0.0.1:1080 -stun stun:stun.l.google.com:19302"
}
# BASH_SOURCE is empty when this self-contained script is piped to bash.
if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
