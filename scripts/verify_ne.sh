#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
go test -race ./internal/socks ./internal/tun ./internal/tunnel
(cd apple/engine && go test -race ./... && go vet ./...)
bash scripts/build_ne.sh
xcrun clang -mmacosx-version-min=13.0 -I apple/engine tests/apple/bridge_smoke.c \
 build/apple/libH264Engine.a -framework Security -framework CoreFoundation \
 -framework SystemConfiguration -lresolv -o build/apple/bridge-smoke
build/apple/bridge-smoke
/usr/bin/plutil -lint apple/macos/H264Proxy.xcodeproj/project.pbxproj
python3 - <<'PY'
from pathlib import Path
import plistlib
app=Path('build/apple/H264Proxy.app');extension=app/'Contents/PlugIns/H264PacketTunnel.appex'
a=plistlib.loads((app/'Contents/Info.plist').read_bytes());e=plistlib.loads((extension/'Contents/Info.plist').read_bytes())
assert e['NSExtension']['NSExtensionPointIdentifier']=='com.apple.networkextension.packet-tunnel'
assert a['H264KeychainGroup']==e['H264KeychainGroup']
assert a['H264KeychainGroup'].startswith('UNSIGNED.')
assert (extension/'Contents/MacOS/H264PacketTunnel').is_file()
print('Bundle layout, extension registration metadata and unsigned signing gate: passed.')
PY
echo 'NE verification passed. Signed system-VPN execution has NOT been tested.'
