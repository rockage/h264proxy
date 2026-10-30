# VPS 手动测试（2026-10-30）

服务端在 `root@152.53.13.153`，由 VPS 原生 Go 构建；本机客户端位于
`/Users/roca/H264Proxy/build/proxy`。

## 手动使用

客户端运行后，在需要测试的软件中设置：

- 代理类型：SOCKS5
- 地址：`127.0.0.1`
- 端口：`1080`
- 用户名 / 密码：留空
- 域名解析：选择通过 SOCKS5 / 代理解析（如果软件提供该选项）

客户端现在默认开启 `-remote-dns-only=true`，拒绝已经由本地解析成 IP 的请求，
日志事件为 `local_dns_rejected`。域名请求由 VPS 解析；SOCKS4a 可用，普通 SOCKS4
需要显式关闭此策略。Chrome SOCKS5 无需额外 DNS 开关。

连接自己的 VPS 后，可以执行 `bash scripts/chrome_proxy.command` 打开独立浏览器
窗口，强制 SOCKS5 并禁用本地目标域名解析。该浏览器脚本只复用已运行的本机
1080 客户端，不再自动启动连接开发者 VPS 的旧脚本。

终端测试命令：

```bash
curl --noproxy '' --socks5-hostname 127.0.0.1:1080 --max-time 60 https://api.ipify.org
```

正常应返回 VPS 的出口 IPv4：`152.53.13.153`。

## 启动、停止客户端

双击 `scripts/start_client.command` 由 macOS launchd 在后台启动，随后可以关闭
终端窗口；双击 `scripts/stop_client.command` 停止。也可以在终端执行：

```bash
/Users/roca/H264Proxy/scripts/start_client_background.sh
# 停止：
/Users/roca/H264Proxy/scripts/stop_client.sh
```

若客户端已在后台运行，启动脚本会提示已运行，不会启动第二份。
本次部署时会启动一份后台客户端供手动测试，日志位于：

- `build/logs/proxy-client-console.log`：启动过程和控制台日志
- `build/logs/proxy-client.log`：代理结构化日志
- `build/logs/proxy-client-report.json`：客户端退出时生成的统计

启动脚本会通过免密 SSH 重启 `h264proxy.service`，给单客户端网关创建新的
SDP 会话。因此，手动停止后重新测试直接再运行启动脚本即可。
本机 SOCKS5 监听只在 WebRTC 连通后开启。客户端关闭后不会自动重连；
网络断开时重新运行启动脚本。后台任务标签为
`com.roca.h264proxy.client`，配置文件放在项目的 `build/deploy/` 下，
没有设置 Mac 登录后自动启动。需要前台调试时可运行 `scripts/start_client.sh`。

## VPS 服务与端口

VPS 公网开放 **9300/TCP + 9300/UDP**：相同端口号，TCP 只传 SDP 信令，
UDP 传 WebRTC DataChannel（SCTP/DTLS）。SOCKS5 不在 VPS 上公开监听；本机为 `127.0.0.1:1080`。
firewalld 的运行时和永久配置均保留这两条规则。

服务已设置开机启动、进程退出后自动重启。进程在连接前等待客户端最多 24 小时，
连接成功后没有该运行时长限制。当前实现一对一配对，一个服务端进程对应一条
客户端会话；启动脚本的重启只影响此代理服务。

```bash
ssh root@152.53.13.153 'systemctl status h264proxy.service --no-pager'
ssh root@152.53.13.153 'journalctl -u h264proxy.service -n 60 --no-pager'
ssh root@152.53.13.153 'systemctl restart h264proxy.service'
```

远端文件：

- `/opt/h264proxy/src/`：上传的源码
- `/opt/h264proxy/bin/proxy`：在 VPS 上构建的网关程序
- `/etc/systemd/system/h264proxy.service`：服务定义
- `/etc/h264proxy/gateway.env`：监听端口与公网 IP
- `/etc/h264proxy/key.bin`：共享密钥（root 与专用服务用户可读）
- `/var/lib/h264proxy/gateway-report.json`：进程退出时生成的统计

服务以专用用户 `h264proxy` 运行。服务模板和安装脚本在本地 `scripts/deploy/`。
Go 使用[官方发布包](https://go.dev/dl/)，下载时进行了 SHA-256 校验，安装到 `/usr/local/go`；
服务程序本身不需要 Go 环境才能运行。

本机密钥为 `build/proxy-vps.key`（仅当前用户可读），通过 scp 取回，
不会包含在源码包或信令中。已有 v1 性能测试用的 `key.bin` 不用于这套服务。

## 重新编译

```bash
# VPS（更新源码后；构建失败不会覆盖正在运行的程序）
ssh root@152.53.13.153 'cd /opt/h264proxy/src && CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -o /opt/h264proxy/bin/proxy.next ./cmd/proxy && chmod 755 /opt/h264proxy/bin/proxy.next && mv /opt/h264proxy/bin/proxy.next /opt/h264proxy/bin/proxy && restorecon /opt/h264proxy/bin/proxy && systemctl restart h264proxy.service'

# Mac（先停止客户端再构建）
cd /Users/roca/H264Proxy
go build -o build/proxy ./cmd/proxy
```

## Chrome 使用与兼容

2026-10-30 排查记录：Chrome 的代理插件中 `WebRTC` 配置显示 SOCKS5，
但实际送到客户端的握手首字节为 `0x04`（SOCKS4）。旧入口只接受
`0x05`（SOCKS5），因此浏览器会出现连接重置，而使用明确 SOCKS5 的
curl 可以成功。现在本机入口同时接受 SOCKS5、SOCKS4 和 SOCKS4a，
且已在原 Chrome 测试页面中确认出口为 `152.53.13.153`。

现有 Chrome 插件中的 `WebRTC` 配置为 `127.0.0.1:1080`，已经启用。
普通 HTTP/HTTPS 代理协议仍不支持，插件中不要选择 HTTP 或 HTTPS。

也可以双击 `scripts/chrome_proxy.command`：该脚本先确保客户端运行，
再启动独立的 Chrome 配置目录，明确使用 `socks5://127.0.0.1:1080`，
禁用该测试窗口的扩展与 QUIC。这个独立配置目录为
`build/chrome-proxy-profile/`，不复用日常浏览器的书签、Cookie 和登录状态。

如果需要在原 Chrome 中停用代理，关闭代理插件里 `WebRTC` 的开关即可。
停止整个客户端后，也需要关闭这个开关，否则 Chrome 会继续尝试连接 1080。

建议优先使用真正的 SOCKS5，域名由 VPS 解析。SOCKS4 的域名先由浏览器
在本地解析为 IPv4；SOCKS4a 可以将域名交给网关解析。Chrome 对这两种
SOCKS 协议的解析方式见 [Chromium 代理说明](https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#SOCKSv4-proxy-scheme)。
