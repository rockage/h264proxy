# H264TunnelLab

## Install the gateway on a Linux VPS

Run as root on a Linux **systemd** server (amd64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh | bash
```

For a regular administrator account, pipe to `sudo bash` instead. The installer
detects the public IPv4, builds natively, installs/reuses Go, preserves existing
keys, configures a systemd service and opens **9300/TCP + 9300/UDP** in an active
firewalld/UFW. Allow both protocols in your cloud security group too.

Custom address/port:

```bash
curl -fsSL https://raw.githubusercontent.com/rockage/h264proxy/main/install.sh \
  | bash -s -- --public-ip YOUR_PUBLIC_IPV4 --port 9300
```

See **[INSTALL.md](INSTALL.md)** for supported systems, updates, rollback,
firewall behavior, key retrieval and client connection instructions. This is a
single-client TCP gateway; installing it does not add automatic client reconnect.

---

A proof-of-concept that carries arbitrary, AEAD-encrypted binary data inside a
**real WebRTC session** whose SDP negotiates the **H.264** video codec. The
payload bytes are *not* a valid H.264 bitstream and are never meant to be
decoded by a video decoder — the point is to prove that:

1. a genuine WebRTC connection can be established (ICE + DTLS + SRTP),
2. the media format negotiated in SDP is really H.264 (90 000 Hz),
3. the server can replace the RTP payload with encrypted application data and
   push it through the **real SRTP media channel**, and
4. the client can read it back from `TrackRemote.ReadRTP`, authenticate,
   decrypt, reassemble, and recover the original file **byte-for-byte**
   (verified by SHA-256).

Phase 1 runs entirely on one machine as **two independent Go processes** over
loopback. There is no SOCKS5, TUN/TAP, proxy forwarding, STUN/TURN, public
server, or video codec — those are explicitly out of scope for this phase.

> **Phase 2 (full-duplex multi-stream mux)** builds on this: a bidirectional
> channel carrying many concurrent, independently flow-controlled logical
> streams, with per-direction keys. See **[PHASE2_MUX.md](PHASE2_MUX.md)** and
> the `cmd/muxnode` demo. Phase 1 below is unchanged.

> **Phase 3 (SOCKS5 TCP proxy)** is implemented in `cmd/proxy`: concurrent
> CONNECT streams, remote DNS, half-close, reliable per-stream cancellation,
> and connection cleanup over the same encrypted WebRTC mux. See
> **[PHASE3_SOCKS.md](PHASE3_SOCKS.md)** for local and VPS commands.

> **What this proves, and what it does not.** This validates *private encrypted
> payload transport over an H.264-negotiated media channel*. It does **not**
> validate compatibility with a real H.264 decoder, nor that the resulting
> traffic is statistically indistinguishable from genuine video. Those are
> non-goals here and are not claimed.

---

## Requirements

- **Go 1.24.7+** (the version required by `go.mod`).
- A platform that allows binding loopback UDP (Linux, macOS, Windows).
- Network access to the Go module proxy on first build (to fetch Pion).

Core dependencies: [`github.com/pion/webrtc/v4`](https://github.com/pion/webrtc),
`github.com/pion/rtp`, and the Go standard library (`crypto/aes`,
`crypto/cipher`, `crypto/sha256`, `net/http`, `log/slog`).

## Quick start (one click)

**Linux / macOS**

```bash
scripts/run.sh
```

**Windows (PowerShell)**

```powershell
powershell -ExecutionPolicy Bypass -File scripts\run.ps1
```

The script checks Go, builds, generates a key, generates a random test file,
starts the server, waits for it, runs the client, transfers the file over the
real WebRTC/SRTP H.264 channel, verifies the SHA-256, writes logs and JSON
reports under `build/logs/`, and **exits non-zero on any failure**. It does not
report success merely because the build compiled.

Environment overrides:

| Variable | Default    | Meaning                                  |
|----------|------------|------------------------------------------|
| `SIZE`   | `4194304`  | Test file size in bytes                  |
| `MODE`   | `aead`     | `aead` (AES-256-GCM) or `raw` (M2 experiment, no encryption) |
| `CHUNK`  | `900`      | Plaintext bytes per DATA chunk           |
| `RATE`   | `20000000` | Payload send rate limit (bytes/s; `0` = unlimited) |
| `PORT`   | `8080`     | Local signaling port                     |

Example: `SIZE=104857600 scripts/run.sh` runs a 100 MB transfer.

## Manual run (for debugging each side separately)

```bash
# 1. Build
go build -o build/genkey ./cmd/genkey
go build -o build/server ./cmd/server
go build -o build/client ./cmd/client

# 2. Generate a 32-byte key (never hard-coded, never logged in full)
build/genkey -out build/data/key.bin -force

# 3. Start the server (owns the file and the send-only H.264 track)
build/server -http 127.0.0.1:8080 -key build/data/key.bin \
  -in path/to/input.bin -mode aead -chunk 900 -rate 20000000 \
  -log build/logs/server.log -report build/logs/server_report.json

# 4. In another terminal, start the client
build/client -http 127.0.0.1:8080 -key build/data/key.bin \
  -out build/data/output.bin -mode aead \
  -log build/logs/client.log -report build/logs/client_report.json
```

Signaling (SDP offer/answer) is exchanged over the local HTTP endpoint only;
**no test data ever crosses HTTP**. The client exits non-zero unless the file
is recovered exactly.

## How it works

### WebRTC / media (`internal/rtc`, `internal/media`)

- The `MediaEngine` registers **only** H.264 (PT 96, `H264/90000`,
  `packetization-mode=1;profile-level-id=42e01f`). Because nothing else is
  registered, SDP cannot silently fall back to VP8/VP9/AV1; if the peer offered
  only those, negotiation would fail. Both ends assert the negotiated codec is
  `video/H264` and abort otherwise.
- The `SettingEngine` includes the loopback candidate, disables mDNS (so
  candidates are raw `127.0.0.1` addresses, not `*.local`), and restricts to
  UDP/IPv4. No STUN/TURN is configured.
- ICE runs over a **UDP mux on a loopback socket with a 16 MiB receive buffer**
  so short bursts of RTP packets are absorbed rather than dropped by the kernel.
- The interceptor registry is **empty**: no NACK/retransmission, so loss is not
  silently repaired and the application-level loss accounting stays meaningful.
- The server adds a `TrackLocalStaticRTP` and writes packets with
  `WriteRTP`; the client reads with `TrackRemote.ReadRTP`. Note that Pion
  **overwrites SSRC and PayloadType** inside `WriteRTP` with the negotiated
  values, so the application never relies on those fields to carry state — all
  state lives in the authenticated payload.

### Frame protocol (`internal/protocol`)

Each RTP payload is one frame: a fixed 38-byte header followed by the payload.

```
Magic "H2TL" | Version | Type(START/DATA/END) | SessionID(8) |
ChunkIndex(8) | NoncePrefix(4) | NonceCounter(8) | PayloadLength(4) | payload...
```

The header is authenticated as AEAD **Associated Data**, so tampering with any
header field is detected on decryption. START carries file name/size/chunk
count/SHA-256; DATA carries one file chunk; END echoes the integrity info.

### Crypto (`internal/crypto`)

- **AES-256-GCM**. The 32-byte key is loaded from a file (generated by
  `genkey`), never hard-coded and never written to logs (only an 8-hex-char
  fingerprint is logged).
- **Nonce = 4-byte random per-session prefix ‖ 8-byte strictly-monotonic
  counter.** A `Sealer` guards against counter reuse, preventing nonce reuse
  under a fixed key within a session.
- A `NullCipher` implements the same interface with no encryption; it backs the
  `raw` mode used for the M2 "does the byte survive the SRTP path" experiments.

### Transfer (`internal/transfer`)

- The sender streams the input in two sequential passes (hash, then send) and
  **never holds the whole file in memory**.
- The receiver writes each chunk directly to its offset in a **temp file**
  (`WriteAt`), tracks a received-chunk bitmap, and detects **missing,
  duplicate, out-of-range, and reordered** chunks. It commits (atomic rename)
  the output file **only** after every chunk is present and the full-file
  SHA-256 matches both the START metadata and a fresh stream hash. A failed or
  wrong-key transfer therefore never yields an output file.

### Logs & reports (`internal/logging`, `internal/metrics`)

- Structured JSON logs (one object per line) with stable fields
  (`session_id`, `event`, `peer_connection_state`, `ice_state`, `codec`,
  `rtp_sequence`, `chunk_index`, `bytes_sent/received`, `transfer_status`,
  `error`, …). No key material is ever logged.
- Each side writes a machine-readable JSON report (codec, PT, SSRC, packet
  counts, byte counts, timings, encrypt/decrypt time, missing/duplicate/
  reordered counts, SHA-256 match, peak memory).

## Project layout

```
cmd/server   cmd/client   cmd/genkey        # phase-1: one-way file transfer
cmd/muxnode                                # phase-2: full-duplex multi-stream demo
cmd/proxy                                  # phase-3: SOCKS5 client + remote gateway
internal/tunnel     long-lived WebRTC mux setup and lifetime management
internal/socks      SOCKS5 CONNECT handshake, remote dialing, TCP relay
internal/rtc        WebRTC API + signaling helpers (H.264-only, loopback)
internal/media      H.264 codec params, track, RTP read/write adapters (+ DeferredRTPReader)
internal/protocol   v1 file-transfer frame format + START/END metadata
internal/crypto     AES-256-GCM, null cipher, key loading, nonce management
internal/transfer   streaming sender + verifying receiver (+ PacketSink/Source)
internal/muxproto   phase-2: v2 mux frame + per-direction HKDF subkeys
internal/mux        phase-2: full-duplex Session + Stream + flow control
internal/metrics    JSON report + peak-memory sampling
internal/logging    slog JSON wrapper
tests/integration   real two-process tests (file transfer + mux duplex)
scripts/run.sh      one-click (Linux/macOS)
scripts/run.ps1     one-click (Windows)
```

## Testing

```bash
gofmt -l .            # formatting (no output == clean)
go vet ./...          # static checks
go test -race ./...   # unit tests + real two-process integration test
go build ./...        # compile everything
```

The integration test (`tests/integration`) spawns the real `server` and
`client` binaries and transfers a file over actual SRTP. If the environment
cannot bind loopback UDP, it **skips with an explicit message** rather than
pretending to pass. Use `-short` to skip it.

See [TEST_REPORT.md](TEST_REPORT.md) for recorded results.

## Known limitations (phase 1)

- Loopback / single host only; no NAT traversal, STUN/TURN, or public server.
- No retransmission: loss is detected and reported, not repaired. Flow control
  is a simple byte-rate limiter plus an enlarged receive buffer; under an
  unlimited rate on a host with a small `rmem_max`, packets can still be dropped
  (and are correctly reported as missing).
- The per-session nonce prefix is 4 bytes. Within a session, nonce uniqueness is
  guaranteed by the monotonic counter. Across many sessions **reusing the same
  key file**, a larger random prefix (or an HKDF per-session subkey) would be
  preferable; this is noted for a later phase.
- Payload is opaque data, not a decodable H.264 bitstream; traffic is not
  optimized to resemble real video.

## Next-phase extension points

- `transfer.PacketSink` / `transfer.PacketSource` isolate the transport, so a
  future SOCKS5 / TUN data source can feed the same framed, encrypted pipeline.
- `protocol.Cipher` allows swapping the AEAD scheme or adding a per-session
  subkey without touching the transfer logic.
- The signaling is a thin HTTP shim and can be replaced by a real signaling
  path for multi-host deployment.
