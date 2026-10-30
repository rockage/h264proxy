# 一键安装服务器端

适用于带有 **systemd 的 Linux VPS**，支持 **amd64 / arm64**。依赖安装支持
Ubuntu/Debian、Rocky/AlmaLinux/RHEL/CentOS Stream 和 Fedora 的 apt/dnf/yum。
需要能访问 GitHub、Go 官方下载站和 Go 模块代理，至少有一个可供客户端访问的公网 IPv4。
不适用于 Windows、macOS、OpenRC 或未运行 systemd 的普通容器。

客户端和服务器端必须同时升级到 DataChannel 版本，不能与旧 H.264 代理互通。
详见 [DATACHANNEL.md](DATACHANNEL.md)。启动命令、共享密钥和端口无需更换。

## 一条命令安装

以 **root** 登录服务器后执行：

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh | bash
```

普通管理员账号使用：

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh | sudo bash
```

默认检测公网 IPv4，使用同一端口号 **9300/TCP（信令）+ 9300/UDP（WebRTC DataChannel）**。
如果服务器有云安全组，还需要在云控制台放行这两种协议；脚本无法代操作云安全组。

指定公网地址、端口：

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh \
  | bash -s -- --public-ip YOUR_PUBLIC_IPV4 --port 9300
```

将 `YOUR_PUBLIC_IPV4` 换成服务器的公网 IPv4。经过 NAT、具有多个出口或 IP 自动检测
失败时，请显式指定。也支持 `PUBLIC_IP` 和 `PORT` 环境变量，命令行参数优先。

想先查看脚本再执行，可以下载到文件：

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh -o /tmp/h264proxy-install.sh
less /tmp/h264proxy-install.sh
bash /tmp/h264proxy-install.sh --help
bash /tmp/h264proxy-install.sh
```

## 脚本做什么

1. 检查 root、systemd、CPU 架构，补齐 curl、证书、tar、Python 3 等依赖。
2. 检测或复用服务地址，检查 TCP/UDP 端口是否被其他服务占用。
3. 把 GitHub 分支/标签解析成具体提交，下载该提交的源码，避免安装过程中分支变化。
4. 复用满足 `go.mod` 要求的本机 Go；没有合适版本时，从 [Go 官方下载元数据](https://go.dev/dl/?mode=json)
   选择稳定版本并校验 SHA-256，安装到本项目私有的 toolchains 目录，不覆盖系统 Go。
5. 在服务器本机编译网关和密钥生成器，编译完成后才开始替换服务文件。
6. 创建专用 `h264proxy` 用户；首次生成随机 32 字节密钥，已有合法密钥始终保留。
7. 写入 systemd 服务，恢复 SELinux 文件标签，设置开机启动和进程退出后重启。
8. 若 firewalld 或 UFW 已启用，添加应用端口规则；不清空规则、不启用原本关闭的防火墙。
9. 启动服务并检查 DataChannel/SCTP SDP 信令响应；配置、启动或健康检查失败时，恢复旧二进制、
   旧配置、旧服务状态，并撤销本次新增的防火墙规则。

成功时输出 `INSTALLED`、安装的提交 SHA、网关地址、密钥路径和管理命令。
它不会在终端打印完整密钥，也不会把密钥上传到 GitHub。

## 安装位置与服务管理

| 路径 | 内容 |
| --- | --- |
| `/opt/h264proxy/bin/proxy` | 网关程序 |
| `/opt/h264proxy/bin/genkey` | 密钥生成器 |
| `/opt/h264proxy/sources/<提交SHA>/` | 安装时的源码快照 |
| `/opt/h264proxy/toolchains/` | 仅在需要时下载的私有 Go 工具链 |
| `/opt/h264proxy/install-info.json` | 源码提交和 Go 版本记录 |
| `/etc/h264proxy/gateway.env` | 公网 IPv4、端口及监听配置 |
| `/etc/h264proxy/key.bin` | 共享密钥，root 与服务组可读 |
| `/var/lib/h264proxy/` | 网关运行数据和退出时的统计 |
| `/etc/systemd/system/h264proxy.service` | systemd 服务 |

```bash
systemctl status h264proxy --no-pager
journalctl -u h264proxy -f
systemctl restart h264proxy
systemctl stop h264proxy
```

脚本不会修改 SSH 配置，也不需要开放额外的远程 SOCKS 端口。公网只需所选端口的 TCP
和 UDP；SOCKS 入口在客户端机器上。

## 客户端连接

服务器启动后，把同一份密钥通过 SSH/scp 等安全通道取回客户端。例如有 root SSH 权限时，
在客户端机器执行（替换主机名）：

```bash
umask 077
scp root@YOUR_SERVER:/etc/h264proxy/key.bin ./key.bin
```

在客户端机器下载源码并编译：

```bash
git clone https://github.com/rockage/h264proxy.git
cd h264proxy
go build -o proxy ./cmd/proxy
```

将 `key.bin` 放到当前目录，然后连接刚安装的服务器：

```bash
./proxy -role client -net -http YOUR_SERVER:9300 \
  -key ./key.bin -socks 127.0.0.1:1080 \
  -stun stun:stun.l.google.com:19302
```

浏览器设置为 SOCKS5 `127.0.0.1:1080`，不填写账号密码。客户端默认启用
`-remote-dns-only=true`：只接收携带域名的 SOCKS5 / SOCKS4a 请求，把域名原样送到
VPS 后解析；拒绝 SOCKS4 和已经在本地解析为 IP 的请求。请求若已丢失域名，代理
无法可靠还原它，不会尝试反向 DNS 猜测。直接访问 IP 的程序需要显式加
`-remote-dns-only=false`，这也会恢复普通 SOCKS4 兼容，但不能防止本地 DNS 污染。

Chrome 的 SOCKS5 本身采用代理端域名解析，不需要扩展提供 DNS 开关。如果扩展
实际使用 SOCKS4、PAC 中有本地解析或其他配置干扰，可在客户端运行后执行：

```bash
bash scripts/chrome_proxy.command
```

这个脚本复用已经运行的 `127.0.0.1:1080` 客户端，打开独立 Chrome 配置，强制
SOCKS5，并用 `--host-resolver-rules` 禁用本地目标域名解析。它不重启网关、不修改
系统 DNS、不自动连接开发者 VPS，也不安装扩展。请使用它打开的专用窗口；其他
Chrome 窗口保持原有设置。相关行为见 [Chromium 代理文档](https://github.com/chromium/chromium/blob/main/net/docs/proxy.md)
和 [SOCKS 启动参数说明](https://chromium.googlesource.com/playground/chromium-org-site/+/refs/heads/main/developers/design-documents/network-stack/socks-proxy.md)。

验证出口：

```bash
curl --noproxy '' --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

`YOUR_SERVER:9300` 是信令地址；若安装时改了端口，客户端也要同步修改。
本仓库的 Mac 便捷脚本最初针对开发者测试 VPS 配置。连接自己的 VPS 请用上面的通用命令，
或为脚本设置 `H264_SSH`、`H264_ENDPOINT`、`H264_KEY` 等相应变量。

## 更新、重复执行与参数

重复运行相同安装命令会重新获取/编译源码，保留现有密钥、IPv4 和端口，并重启网关。
更新会断开当前客户端会话；请安排好测试时机，再重新连接客户端。
不会删除旧的源码快照，也不会覆盖以前手动部署使用的 `/opt/h264proxy/src/` 目录。

| 参数 | 默认值 / 行为 |
| --- | --- |
| `--public-ip IPv4` | 命令行 > `PUBLIC_IP` 环境变量 > 已有配置 > 自动检测 |
| `--port N` | 命令行 > `PORT` 环境变量 > 已有配置 > 9300；TCP/UDP 相同 |
| `--ref REF` | `main`；可指定分支、标签或完整的 40 位提交 SHA |
| `--no-firewall` | 跳过主机防火墙配置，端口由管理员自行放行 |
| `--help` | 显示帮助，不安装、不要求 root |

需要固定安装版本时：

```bash
bash /tmp/h264proxy-install.sh --ref FULL_40_CHARACTER_COMMIT_SHA
```

完整提交 SHA 会直接使用 codeload 下载，避免 GitHub 未登录 API 的请求限额。
安装器依赖的 Go 模块下载可通过常规 `GOPROXY` 环境变量配置；脚本不替换全局 Go 配置。

低于 1024 的端口会给服务增加绑定低端口所需的 `CAP_NET_BIND_SERVICE`，仍以专用用户运行。
更换端口不会主动删除旧端口的既有防火墙规则，管理员可以确认无其他服务依赖后自行清理。
如果手工配置了独立信令地址/ICE UDP 端口，请继续使用手动部署，不要用这个单端口安装器覆盖。

未检测到已运行的 firewalld/UFW 时，脚本会提示自行检查其他 iptables/nftables 或云防火墙；
它不会修改未知防火墙配置。健康检查证明服务器进程和本机信令可用，公网 UDP/NAT 路径仍需
客户端实际连接验证。

当前网关每个进程配对一个客户端；断线后可重启网关建立新会话。服务端进程自动重启并不
等于客户端自动重连。目前不提供 SOCKS UDP ASSOCIATE，代理数据通过 SCTP/DTLS DataChannel 传输，不包含视频轨道。

## 安装器验证

```bash
bash -n install.sh
python3 -m unittest discover -s tests/installer -v
go test -race ./...
```

安装器测试以隔离的命令/文件系统夹具覆盖首次安装、重复安装、私有 Go 下载与校验失败、
旧密钥保留、编译失败、端口占用、启动回退、firewalld/UFW 和低端口能力配置，不需要 root
或联网。真实 Linux VPS 的部署与连通验证记录在 `TEST_REPORT.md`。
