#!/bin/bash
cd "$(dirname "$0")/.." || exit 1
bash scripts/stop_client.sh
read -r -p '按回车关闭窗口…' _
