#!/usr/bin/env bash
# Build an unsigned reviewable App/Packet Tunnel bundle with CLT or Xcode.
# Signing and installation are deliberately separate operations.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
ENGINE_ONLY=false
if [[ $# -gt 1 ]]; then echo '用法：build_ne.sh [--engine-only]' >&2; exit 2; fi
if [[ $# == 1 ]]; then
  if [[ "$1" != --engine-only ]]; then echo '未知构建参数。' >&2; exit 2; fi
  ENGINE_ONLY=true
fi
if [[ "$(uname -s)" != Darwin ]]; then echo 'Network Extension 构建需要 macOS。' >&2; exit 1; fi
SDK="$(xcrun --sdk macosx --show-sdk-path)"
ARCH="$(uname -m)"
case "$ARCH" in arm64|x86_64) ;; *) echo "Unsupported architecture: $ARCH" >&2; exit 1;; esac
OUT="$ROOT/build/apple"
APP="$OUT/H264Proxy.app"
EXT="$APP/Contents/PlugIns/H264PacketTunnel.appex"
mkdir -p "$OUT/include" "$OUT/swift-cache" "$APP/Contents/MacOS" "$EXT/Contents/MacOS"
(
 cd apple/engine
 MACOSX_DEPLOYMENT_TARGET=13.0 CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=13.0" go build -trimpath -buildmode=c-archive -o "$OUT/libH264Engine.a" .
)
cp apple/engine/H264Engine.h "$OUT/include/H264Engine.h"
if [[ "$ENGINE_ONLY" == true ]]; then echo "Go archive ready: $OUT/libH264Engine.a"; exit 0; fi
xcrun swiftc -swift-version 5 -target "${ARCH}-apple-macosx13.0" -sdk "$SDK" \
 -module-cache-path "$OUT/swift-cache" -parse-as-library -module-name H264ProxyApp \
 apple/macos/Shared/*.swift apple/macos/App/*.swift \
 -framework SwiftUI -framework NetworkExtension -framework Security \
 -o "$APP/Contents/MacOS/H264Proxy"
xcrun swiftc -swift-version 5 -target "${ARCH}-apple-macosx13.0" -sdk "$SDK" \
 -module-cache-path "$OUT/swift-cache" -parse-as-library -application-extension \
 -module-name H264PacketTunnel -import-objc-header apple/macos/PacketTunnel/Bridge.h \
 -I "$OUT/include" -L "$OUT" -lH264Engine \
 apple/macos/Shared/*.swift apple/macos/PacketTunnel/*.swift \
 -framework Foundation -framework NetworkExtension -framework Network \
 -framework Security -framework CoreFoundation -framework SystemConfiguration -lresolv \
 -Xlinker -e -Xlinker _NSExtensionMain \
 -o "$EXT/Contents/MacOS/H264PacketTunnel"
python3 - "$ROOT" <<'PY'
import pathlib,plistlib,sys,json,hashlib,datetime
root=pathlib.Path(sys.argv[1]);out=root/'build/apple';app=out/'H264Proxy.app';ext=app/'Contents/PlugIns/H264PacketTunnel.appex'
for source,target in [('App/Info.plist',app/'Contents/Info.plist'),('PacketTunnel/Info.plist',ext/'Contents/Info.plist'),('App/App.entitlements',out/'App.unsigned.entitlements'),('PacketTunnel/PacketTunnel.entitlements',out/'PacketTunnel.unsigned.entitlements')]:
 text=(root/'apple/macos'/source).read_text().replace('$(AppIdentifierPrefix)','UNSIGNED.')
 target.write_text(text)
 plistlib.loads(target.read_bytes())
# Include dependency license texts; this step copies no runtime config or keys.
import subprocess,shutil
text=subprocess.check_output(['go','list','-m','-json','all'],cwd=root/'apple/engine',text=True)
decoder=json.JSONDecoder();modules=[]
while text.strip():
    text=text.lstrip();item,end=decoder.raw_decode(text);text=text[end:];modules.append(item)
licenses=app/'Contents/Resources/Licenses';licenses.mkdir(parents=True,exist_ok=True)
manifest=[]
for module in modules:
    if module['Path'].startswith('github.com/roca/h264-tunnel-lab'):continue
    directory=module.get('Dir')
    if not directory:continue
    target=licenses/module['Path'].replace('/','_');names=[]
    for file in pathlib.Path(directory).iterdir():
        if file.is_file() and file.name.upper().startswith(('LICENSE','COPYING','COPYRIGHT')):
            target.mkdir(exist_ok=True);shutil.copyfile(file,target/file.name);names.append(file.name)
    manifest.append({'module':module['Path'],'version':module.get('Version'),'license_files':names})
(licenses/'Dependencies.json').write_text(json.dumps(manifest,indent=2)+'\n')
files=['Contents/MacOS/H264Proxy','Contents/PlugIns/H264PacketTunnel.appex/Contents/MacOS/H264PacketTunnel']
(out/'BUILDINFO.json').write_text(json.dumps({'built_at':datetime.datetime.now().astimezone().isoformat(),'signed':False,'system_vpn_usable':False,'architecture':__import__('platform').machine(),'binary_sha256':{f:hashlib.sha256((app/f).read_bytes()).hexdigest() for f in files}},indent=2)+'\n')
PY
/usr/bin/plutil -lint "$APP/Contents/Info.plist" "$EXT/Contents/Info.plist"
echo "未签名构建完成：$APP"
echo '已编译 App、Packet Tunnel 扩展和 Go 静态库；实际启动 VPN 仍需开发者签名、匹配的配置文件及系统授权。'
