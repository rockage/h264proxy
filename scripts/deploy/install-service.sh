#!/usr/bin/env bash
# Run on the VPS after a native build in /opt/h264proxy/src.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PUBLIC_IP="${PUBLIC_IP:?请设置当前 VPS 的 PUBLIC_IP，不再默认使用历史测试地址}"
PORT="${PORT:-9300}"
if ! getent passwd h264proxy >/dev/null; then
  useradd --system --home-dir /var/lib/h264proxy --shell /sbin/nologin h264proxy
fi
install -d -m 755 /opt/h264proxy /opt/h264proxy/bin
install -d -m 750 -o root -g h264proxy /etc/h264proxy
install -d -m 750 -o h264proxy -g h264proxy /var/lib/h264proxy
if [[ ! -f /etc/h264proxy/key.bin ]]; then
  /opt/h264proxy/bin/genkey -out /etc/h264proxy/key.bin
fi
chown root:h264proxy /etc/h264proxy/key.bin
chmod 640 /etc/h264proxy/key.bin
cat > /etc/h264proxy/gateway.env <<CONFIG
PUBLIC_IP=$PUBLIC_IP
UDP_PORT=$PORT
HTTP_ADDR=0.0.0.0:$PORT
CONFIG
chmod 640 /etc/h264proxy/gateway.env
chown root:h264proxy /etc/h264proxy/gateway.env
install -m 644 "$HERE/h264proxy.service" /etc/systemd/system/h264proxy.service
# Go's temporary build output can retain user_tmp_t when renamed into /opt.
# Restore the deployment paths' normal SELinux labels before systemd executes it.
if command -v restorecon >/dev/null; then
  restorecon -RF /opt/h264proxy/bin /etc/h264proxy /var/lib/h264proxy /etc/systemd/system/h264proxy.service
fi
systemd-analyze verify /etc/systemd/system/h264proxy.service
firewall-cmd --zone=public --add-port="$PORT/tcp" --add-port="$PORT/udp"
firewall-cmd --permanent --zone=public --add-port="$PORT/tcp" --add-port="$PORT/udp"
systemctl daemon-reload
systemctl enable --now h264proxy.service
sleep 1
systemctl is-active --quiet h264proxy.service
systemctl --no-pager --full status h264proxy.service
