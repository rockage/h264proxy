#!/bin/bash
cd "$(dirname "$0")/.." || exit 1
echo "旁路模式：仅启动 SOCKS 代理，不接管系统路由和 DNS。"
bash scripts/start_client_background.sh
status=$?
printf '\n启动操作完成（退出码 %s）。可以关闭此窗口。\n' "$status"
read -r -p '按回车关闭窗口…' _
exit "$status"
