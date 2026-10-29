#!/usr/bin/env bash
#
# One-click end-to-end test for H264TunnelLab (Linux/macOS).
#
# Steps: check Go, build, generate a key, generate a test file, start the
# server, wait for it, start the client, transfer over real WebRTC/SRTP, verify
# SHA-256, and report success/failure. Exits non-zero on any failure.
#
# Usage:
#   scripts/run.sh                 # default: 4 MiB random file, aead mode
#   SIZE=104857600 scripts/run.sh  # custom size in bytes
#   MODE=raw scripts/run.sh        # raw (unencrypted) transport experiment
#
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

SIZE="${SIZE:-4194304}"          # bytes of random test data (default 4 MiB)
MODE="${MODE:-aead}"             # aead | raw
CHUNK="${CHUNK:-900}"            # plaintext bytes per DATA chunk
RATE="${RATE:-20000000}"        # payload bytes/sec (0 = unlimited)
PORT="${PORT:-8080}"
ADDR="127.0.0.1:${PORT}"

BUILD="${ROOT}/build"
LOGS="${ROOT}/build/logs"
DATA="${ROOT}/build/data"
mkdir -p "${BUILD}" "${LOGS}" "${DATA}"

KEY="${DATA}/key.bin"
INFILE="${DATA}/input.bin"
OUTFILE="${DATA}/output.bin"
SRV_REPORT="${LOGS}/server_report.json"
CLI_REPORT="${LOGS}/client_report.json"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
info()  { printf '\033[36m[*]\033[0m %s\n' "$*"; }

SRV_PID=""
cleanup() {
  if [[ -n "${SRV_PID}" ]] && kill -0 "${SRV_PID}" 2>/dev/null; then
    kill "${SRV_PID}" 2>/dev/null || true
    wait "${SRV_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# 1. Check Go.
info "Checking Go toolchain..."
if ! command -v go >/dev/null 2>&1; then
  red "Go is not installed or not on PATH. Install Go 1.21+ from https://go.dev/dl/"
  exit 1
fi
go version

# 2. Build.
info "Downloading dependencies and building..."
go build ./... >/dev/null
go build -o "${BUILD}/genkey" ./cmd/genkey
go build -o "${BUILD}/server" ./cmd/server
go build -o "${BUILD}/client" ./cmd/client
green "Build OK"

# 3. Key (aead mode only).
if [[ "${MODE}" == "aead" ]]; then
  info "Generating 32-byte key..."
  "${BUILD}/genkey" -out "${KEY}" -force
fi

# 4. Test file.
info "Generating ${SIZE}-byte random test file..."
head -c "${SIZE}" /dev/urandom > "${INFILE}"
SRC_SHA="$(shasum -a 256 "${INFILE}" 2>/dev/null | cut -d' ' -f1 || sha256sum "${INFILE}" | cut -d' ' -f1)"
info "source SHA-256: ${SRC_SHA}"

# 5. Start server.
info "Starting server on ${ADDR}..."
"${BUILD}/server" -http "${ADDR}" -key "${KEY}" -in "${INFILE}" -mode "${MODE}" \
  -chunk "${CHUNK}" -rate "${RATE}" -log "${LOGS}/server.log" \
  -report "${SRV_REPORT}" -timeout 600s >"${LOGS}/server.stdout" 2>&1 &
SRV_PID=$!

# 6. Wait for server HTTP readiness (poll /offer).
info "Waiting for server to be ready..."
ready=0
for _ in $(seq 1 100); do
  if ! kill -0 "${SRV_PID}" 2>/dev/null; then
    red "Server exited early. Last log lines:"; tail -n 20 "${LOGS}/server.stdout"; exit 1
  fi
  if curl -sf "http://${ADDR}/offer" -o /dev/null 2>/dev/null; then ready=1; break; fi
  sleep 0.2
done
if [[ "${ready}" -ne 1 ]]; then red "Server did not become ready in time"; exit 1; fi
green "Server ready"

# 7-12. Run client (drives negotiation, transfer, recovery, verification).
info "Starting client..."
if "${BUILD}/client" -http "${ADDR}" -key "${KEY}" -out "${OUTFILE}" -mode "${MODE}" \
     -log "${LOGS}/client.log" -report "${CLI_REPORT}" -timeout 600s \
     >"${LOGS}/client.stdout" 2>&1; then
  CLIENT_OK=1
else
  CLIENT_OK=0
fi

wait "${SRV_PID}"; SRV_EXIT=$?; SRV_PID=""

# 13. Verify.
DST_SHA="$(shasum -a 256 "${OUTFILE}" 2>/dev/null | cut -d' ' -f1 || sha256sum "${OUTFILE}" 2>/dev/null | cut -d' ' -f1 || echo MISSING)"
echo
info "server exit=${SRV_EXIT} client ok=${CLIENT_OK}"
info "source      SHA-256: ${SRC_SHA}"
info "recovered   SHA-256: ${DST_SHA}"
info "reports: ${SRV_REPORT} , ${CLI_REPORT}"

if [[ "${CLIENT_OK}" -eq 1 && "${SRV_EXIT}" -eq 0 && "${SRC_SHA}" == "${DST_SHA}" ]]; then
  green "SUCCESS: file transferred over real WebRTC H.264 SRTP channel and recovered byte-for-byte."
  exit 0
else
  red "FAILURE: transfer did not verify. See logs in ${LOGS}."
  [[ -f "${LOGS}/client.stdout" ]] && { echo "--- client tail ---"; tail -n 20 "${LOGS}/client.stdout"; }
  exit 1
fi
