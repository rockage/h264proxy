#!/usr/bin/env bash
#
# One-shot real-network performance test: deploy the server to a public-IP VPS
# over your own SSH, run two tests from this Mac (client), and collect the JSON
# reports back here. Uses YOUR ssh/key (via ssh-agent or ~/.ssh); this script
# never handles the key itself.
#
# Prereqs (once): open TCP $SIG_PORT and UDP $UDP_PORT on the server firewall
# AND the cloud security group. See DEPLOY.md step 1.
#
# Usage:
#   SSH=user@1.2.3.4 PUBLIC_IP=1.2.3.4 bash scripts/vps_perf.sh
# or edit the defaults below and run:  bash scripts/vps_perf.sh
set -euo pipefail

# ==== configuration (override via environment) ====
SSH="${SSH:?set SSH=user@host}"
PUBLIC_IP="${PUBLIC_IP:?set PUBLIC_IP=<server public IP>}"
SIG_PORT="${SIG_PORT:-9300}"      # TCP signaling
UDP_PORT="${UDP_PORT:-9301}"      # UDP media
STUN="${STUN:-stun:stun.l.google.com:19302}"
SIZE_A="${SIZE_A:-52428800}"      # Test A file size (50 MB)
STREAMS="${STREAMS:-8}"           # Test B streams
SIZE_B="${SIZE_B:-1048576}"       # Test B per-stream bytes (1 MB)
RATE="${RATE:-0}"                 # server send pace (0 = unlimited)
RDIR="${RDIR:-/tmp/h264proxy}"    # remote working dir
# ==================================================

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }
err()  { printf '\033[31m%s\033[0m\n' "$*"; }

case "$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$SSH" uname -m)" in
  x86_64) remote_arch=amd64 ;;
  aarch64|arm64) remote_arch=arm64 ;;
  *) err "Unsupported VPS architecture"; exit 1 ;;
esac
remote_dist="dist/linux-$remote_arch"
mkdir -p build "$remote_dist"
info "Building static Linux/$remote_arch server binaries ..."
for program in server genkey muxnode; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$remote_arch" go build -o "$remote_dist/$program" "./cmd/$program"
done

info "Building local client + muxnode (this Mac) ..."
go build -o build/client ./cmd/client || { err "go build client failed"; exit 1; }
go build -o build/muxnode ./cmd/muxnode || { err "go build muxnode failed"; exit 1; }

info "Uploading server binaries to $SSH:$RDIR ..."
ssh "$SSH" "mkdir -p $RDIR" || { err "ssh failed — check SSH=$SSH and your key"; exit 1; }
scp "$remote_dist/server" "$remote_dist/genkey" "$remote_dist/muxnode" "$SSH:$RDIR/" || { err "scp failed"; exit 1; }
ssh "$SSH" "chmod +x $RDIR/server $RDIR/genkey $RDIR/muxnode"

info "Generating shared key on server and fetching it ..."
ssh "$SSH" "$RDIR/genkey -out $RDIR/key.bin -force"
scp "$SSH:$RDIR/key.bin" ./key.bin

stop_remote() { ssh "$SSH" "for f in $RDIR/server.pid $RDIR/muxnode.pid; do if test -f \"\$f\"; then kill \"\$(cat \"\$f\")\" 2>/dev/null || true; rm -f \"\$f\"; fi; done" || true; }
trap stop_remote EXIT
stop_remote
# Archive prior reports so failed runs cannot appear to have fresh results.
archive="build/perf-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$archive"
for report in cliA.json srvA.json muxcli.json muxsrv.json; do
  if [[ -f "$report" ]]; then mv "$report" "$archive/"; fi
done
ssh "$SSH" "rm -f $RDIR/srvA.json $RDIR/muxsrv.json"
failed=0

collect_report() {
  local name="$1"
  for ((i=0; i<15; i++)); do
    if scp "$SSH:$RDIR/$name.json" "./$name.json" 2>/dev/null; then return 0; fi
    sleep 1
  done
  err "Missing server report: $name.json"
  return 1
}

# ---------- TEST A: single-stream throughput ----------
info "TEST A — single-stream file throughput (${SIZE_A} bytes) ..."
ssh "$SSH" "head -c $SIZE_A /dev/urandom > $RDIR/testA.bin"
ssh "$SSH" "nohup $RDIR/server -net -public-ip $PUBLIC_IP -udp-port $UDP_PORT -http 0.0.0.0:$SIG_PORT \
    -key $RDIR/key.bin -in $RDIR/testA.bin -rate $RATE -report $RDIR/srvA.json -timeout 5m \
    >$RDIR/srvA.log 2>&1 </dev/null & echo \$! >$RDIR/server.pid"
sleep 2
build/client -net -stun "$STUN" -http "$PUBLIC_IP:$SIG_PORT" -key ./key.bin \
    -out ./recvA.bin -report ./cliA.json -timeout 5m >cliA.log 2>&1 || { err "client A failed (see cliA.log)"; failed=1; }
collect_report srvA || failed=1
scp "$SSH:$RDIR/srvA.log" ./srvA.log
stop_remote

# ---------- TEST B: multi-stream mux (RTT / cwnd) ----------
info "TEST B — multi-stream mux (${STREAMS} x ${SIZE_B} bytes each way) ..."
ssh "$SSH" "nohup $RDIR/muxnode -role listener -net -public-ip $PUBLIC_IP -udp-port $UDP_PORT \
    -http 0.0.0.0:$SIG_PORT -key $RDIR/key.bin -streams $STREAMS -size $SIZE_B \
    -report $RDIR/muxsrv.json -timeout 5m >$RDIR/muxsrv.log 2>&1 </dev/null & echo \$! >$RDIR/muxnode.pid"
sleep 2
build/muxnode -role dialer -net -stun "$STUN" -http "$PUBLIC_IP:$SIG_PORT" -key ./key.bin \
    -streams "$STREAMS" -size "$SIZE_B" -report ./muxcli.json -timeout 5m >muxcli.log 2>&1 || { err "mux dialer failed (see muxcli.log)"; failed=1; }
collect_report muxsrv || failed=1
scp "$SSH:$RDIR/muxsrv.log" ./muxsrv.log

if [[ "$failed" == 0 ]]; then
  ok "PASS. Reports written to: cliA.json  srvA.json  muxcli.json  muxsrv.json"
else
  err "FAIL. Inspect the JSON reports and client/server logs."
fi
echo "---- TEST A (client) ----"; cat ./cliA.json 2>/dev/null || true
echo "---- TEST B (dialer) ----"; cat ./muxcli.json 2>/dev/null || true
exit "$failed"
