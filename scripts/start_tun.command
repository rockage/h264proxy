#!/bin/bash
cd "$(dirname "$0")/.." || exit 1
if [[ -z "${H264_TUN_SERVICE:-}" ]]; then
  /usr/sbin/networksetup -listallnetworkservices
  read -r -p '请输入当前网络服务名称（例如 Ethernet 或 Wi-Fi）：' H264_TUN_SERVICE
  export H264_TUN_SERVICE
fi
bash scripts/start_tun.sh
status=$?
printf '\nTUN 客户端已退出（退出码 %s）。\n' "$status"
read -r -p '按回车关闭窗口…' _
exit "$status"
