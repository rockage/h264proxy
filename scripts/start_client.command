#!/bin/bash
cd "$(dirname "$0")/.." || exit 1
bash scripts/start_client_background.sh
status=$?
printf '\n启动操作完成（退出码 %s）。可以关闭此窗口。\n' "$status"
read -r -p '按回车关闭窗口…' _
exit "$status"
