#!/usr/bin/env bash
# Install an exact upstream release; keep it independent of the Go module.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[[ "$(uname -s)" == Darwin ]] || { echo '目前仅支持 macOS。' >&2; exit 1; }
case "$(uname -m)" in
  arm64) ARCH=arm64; SHA=0d8c7f5ed5d587b0f1a5e9772a708ec3bfa4ac4c9329604b01d8d9c2ba406838 ;;
  x86_64) ARCH=amd64; SHA=eae86b9b033bba50b7c44ab719e4bfac0aca05efd28483390ed3aa510f32db25 ;;
  *) echo '不支持的架构。' >&2; exit 1 ;;
esac
mkdir -p "$ROOT/build/tools"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NAME="sing-box-1.14.4-darwin-$ARCH"
curl --fail --location --retry 2 "https://github.com/SagerNet/sing-box/releases/download/v1.14.4/$NAME.tar.gz" -o "$TMP/archive.tar.gz"
python3 - "$TMP/archive.tar.gz" "$SHA" "$TMP" "$NAME" <<'PY'
import hashlib, pathlib, sys, tarfile
archive, expected, dest, name = sys.argv[1:]
assert hashlib.sha256(pathlib.Path(archive).read_bytes()).hexdigest() == expected, 'SHA256 mismatch'
with tarfile.open(archive) as source:
    for filename in ('sing-box', 'LICENSE'):
        member = source.getmember(name + '/' + filename)
        assert member.isfile()
        pathlib.Path(dest, filename).write_bytes(source.extractfile(member).read())
PY
install -m 755 "$TMP/sing-box" "$ROOT/build/tools/sing-box"
install -m 644 "$TMP/LICENSE" "$ROOT/build/tools/sing-box.LICENSE"
cd "$ROOT"
go build -o build/proxy ./cmd/proxy
"$ROOT/build/tools/sing-box" version
echo '已构建。启动：H264_TUN_SERVICE=Wi-Fi bash scripts/start_proxy.sh --sing-box'
