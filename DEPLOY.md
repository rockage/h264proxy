> Current SOCKS gateway uses DataChannel; follow [INSTALL.md](INSTALL.md) and
> [DATACHANNEL.md](DATACHANNEL.md). The file/RTP performance procedure below
> is a historical lab experiment, not the production proxy deployment.

# Real-network deployment & performance test

For a new gateway installation, use the generic **[one-command installer](INSTALL.md)**:

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh | bash
```

The rest of this document describes the earlier manual file/mux performance
tests. Their signaling/media port choices are independent of the installer,
which defaults to port 9300 for both TCP signaling and UDP media.

Server on a public-IP Linux VPS, client on your Mac. The performance script
detects the VPS architecture over SSH and cross-compiles static binaries into
`dist/linux-amd64/` or `dist/linux-arm64/` — no Go needed on the server.
Your Mac builds its own client from source (you have Go 1.27).

Run both tests with `SSH=user@host PUBLIC_IP=host bash scripts/vps_perf.sh`.
The script collects four JSON reports and four logs, archives previous reports
under `build/perf-<timestamp>/`, and exits nonzero if a test or report collection
fails. The historical Nuremberg VPS used Linux ARM64; the currently selected
`207.148.110.180` reports `x86_64` (Linux AMD64). Always detect the target
architecture instead of reusing binaries from a previous node.

Fill in these first:

```
PUBLIC_IP=<server public IP>      # on the server: curl -s ifconfig.me
SIG_PORT=9300                     # TCP, signaling (SDP offer/answer)
UDP_PORT=9301                     # UDP, the actual SRTP media
SSH=user@$PUBLIC_IP               # your SSH login
```

## 1. Open the firewall on the server

Open **TCP SIG_PORT** and **UDP UDP_PORT** to the internet (your cloud security
group *and* any host firewall):

```bash
# ufw example
sudo ufw allow 9300/tcp
sudo ufw allow 9301/udp
# firewalld example
sudo firewall-cmd --add-port=9300/tcp --add-port=9301/udp --permanent && sudo firewall-cmd --reload
```
On AWS/GCP/Azure/Aliyun also add these to the instance's security group.

## 2. Copy the server binaries up

From the folder that contains `dist/` (this project root on your Mac):

```bash
# Check the server first: uname -m returns x86_64 (amd64) or aarch64 (arm64).
ssh "$SSH" uname -m
# Set GOARCH=amd64 instead for an x86_64 server.
mkdir -p dist/linux-arm64
for program in server genkey muxnode; do
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "dist/linux-arm64/$program" "./cmd/$program"
done
scp dist/linux-arm64/server dist/linux-arm64/genkey dist/linux-arm64/muxnode "$SSH":/tmp/
ssh "$SSH" 'chmod +x /tmp/server /tmp/genkey /tmp/muxnode'
```

## 3. Generate a shared key and get it to BOTH sides

The AEAD key must be identical on server and client and must travel over a
secure channel (scp), never over the signaling HTTP.

```bash
# on the server
ssh "$SSH" '/tmp/genkey -out /tmp/key.bin -force'
# bring the same key to your Mac
scp "$SSH":/tmp/key.bin ./key.bin
```

## 4. Performance test A — single-stream file throughput

Measures connect time, payload throughput, packet loss and SHA-256 over the
real path.

```bash
# --- on the server: make a test file and serve it ---
ssh "$SSH" 'head -c 52428800 /dev/urandom > /tmp/test_50mb.bin'   # 50 MB
ssh "$SSH" "/tmp/server -net -public-ip $PUBLIC_IP -udp-port $UDP_PORT \
    -http 0.0.0.0:$SIG_PORT -key /tmp/key.bin -in /tmp/test_50mb.bin \
    -rate 0 -report /tmp/srv.json -timeout 5m"
# (leave it running; it exits after the client finishes)

# --- on your Mac, in this project folder ---
go build -o build/client ./cmd/client
build/client -net -stun stun:stun.l.google.com:19302 \
    -http $PUBLIC_IP:$SIG_PORT -key ./key.bin -out ./recv_50mb.bin \
    -report ./cli.json -timeout 5m
```

Then look at `cli.json` on the Mac:
- `connect_seconds` — ICE+DTLS setup time across the real path
- `payload_throughput_bytes_per_sec` (server side, `srv.json`)
- `transfer_seconds`, `rtp_packets_received`, `missing_chunks`, `sha256_match`

`-rate 0` lets it send as fast as possible — if you see loss, set e.g.
`-rate 2000000` (2 MB/s) on the server to pace it.

## 5. Performance test B — multi-stream + congestion/RTT (muxnode)

Shows adaptive RTO, congestion window and retransmits over the real path.

```bash
# --- on the server (listener) ---
ssh "$SSH" "/tmp/muxnode -role listener -net -public-ip $PUBLIC_IP \
    -udp-port $UDP_PORT -http 0.0.0.0:$SIG_PORT -key /tmp/key.bin \
    -streams 8 -size 1048576 -report /tmp/mux_srv.json -timeout 5m"

# --- on your Mac (dialer) ---
go build -o build/muxnode ./cmd/muxnode
build/muxnode -role dialer -net -stun stun:stun.l.google.com:19302 \
    -http $PUBLIC_IP:$SIG_PORT -key ./key.bin \
    -streams 8 -size 1048576 -report ./mux_cli.json -timeout 5m
```

Look at `mux_cli.json`:
- `srtt_ms`, `rto_ms` — measured RTT and adaptive timeout over the real path
- `cwnd_bytes`, `max_cwnd_bytes`, `ssthresh_bytes`, `cong_loss_events`
- `retransmits`, `streams_recv_ok` / `streams_recv_fail`, `duplex_confirmed`

## Troubleshooting

- **Server cannot execute the binary** → compare `ssh "$SSH" uname -m` with
  `file dist/linux-*/server`; executable permissions alone cannot fix an
  amd64/ARM64 architecture mismatch.
- **Client stalls at "waiting / offer_fetched" then times out** → signaling TCP
  port not reachable: re-check the firewall/security group for `SIG_PORT`.
- **ICE never reaches `connected` (stuck in `checking`)** → the UDP media port
  isn't open, or the server's advertised candidate is wrong. Confirm
  `-public-ip` is the real public IP (`curl ifconfig.me` on the server) and
  `UDP_PORT` is open for UDP.
- **Connects but transfer is slow / lossy** → pace the sender with `-rate`
  (server) and compare `cong_loss_events` / `missing_chunks` across rates.
- **Server also behind NAT (no public IP)** → this guide assumes a public-IP
  server. A double-NAT setup needs a TURN relay; tell me and I'll add
  `-turn`/credentials support.

## Notes

- The media really traverses WebRTC/SRTP; only SDP + a done-barrier use HTTP.
- The server binary is static (no libc); its CPU architecture must match the VPS.
- Keep `key.bin` private; delete the `/tmp` copies when done.
