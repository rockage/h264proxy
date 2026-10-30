# Phase 3 — SOCKS5 TCP proxy

`cmd/proxy` connects a local SOCKS5 listener to a remote TCP gateway over one
long-lived, AES-256-GCM-protected mux session on a reliable, ordered WebRTC DataChannel.

## Run locally

Requires Go 1.24.7+ and two terminals in the project directory.

```bash
mkdir -p build
go build -o build/genkey ./cmd/genkey
go build -o build/proxy ./cmd/proxy
build/genkey -out build/key.bin -force

# Terminal 1: remote TCP gateway (loopback mode by default)
build/proxy -role gateway -http 127.0.0.1:9300 -key build/key.bin

# Terminal 2: local SOCKS5 listener
build/proxy -role client -http 127.0.0.1:9300 -key build/key.bin \
  -socks 127.0.0.1:1080
```

Configure an application for SOCKS5 at `127.0.0.1:1080`, with proxy-side DNS.
For example, against a reachable HTTP target:

```bash
curl --socks5-hostname 127.0.0.1:1080 http://example.com/
```

Start both processes within the default 45-second setup window, or increase
`-connect-timeout`. The SOCKS listener opens only after WebRTC connects. Stop
with Ctrl-C; active sockets and streams are canceled and the process exits.

## VPS deployment

For a fresh server, use **[INSTALL.md](INSTALL.md)** for the native one-command
installer, persistent service and host-firewall setup. Manual build steps follow.

First check `ssh "$SSH" uname -m`. The supplied test VPS is aarch64, requiring
`GOARCH=arm64`. Use `amd64` for x86_64 hosts.

```bash
mkdir -p dist/linux-arm64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/linux-arm64/proxy ./cmd/proxy
scp dist/linux-arm64/proxy "$SSH":/tmp/h264proxy/proxy
ssh "$SSH" 'chmod +x /tmp/h264proxy/proxy'
```

Copy the same private 32-byte key to both endpoints over SSH/scp, as described
in [DEPLOY.md](DEPLOY.md); it never goes through signaling HTTP. Open
9300/TCP and 9301/UDP on the gateway host firewall and any cloud firewall.

```bash
# VPS
/tmp/h264proxy/proxy -role gateway -net -public-ip 152.53.13.153 \
  -udp-port 9301 -http 0.0.0.0:9300 -key /tmp/h264proxy/key.bin

# Mac (same key contents as the VPS)
build/proxy -role client -net -http 152.53.13.153:9300 \
  -stun stun:stun.l.google.com:19302 -key ./key.bin \
  -socks 127.0.0.1:1080
```

Both peers must run the updated mux implementation: phase 3 adds RESET frame
kind 6 to protocol v2. The v1 file protocol is unchanged.

## Supported behavior

- SOCKS5 no-authentication method, TCP CONNECT only.
- Default command policy is `-remote-dns-only=true`: accept hostname requests
  (SOCKS5 or SOCKS4a) and reject IP-only requests before opening a mux stream.
  Original names cannot reliably be recovered from already resolved IPs.
  `-remote-dns-only=false` explicitly restores direct-IP compatibility.
- SOCKS4/4a ingress parsing on the same port. SOCKS4 IPv4 requests and
  SOCKS4a domain requests are converted to the same reliable SOCKS5 CONNECT
  record inside the tunnel, so gateways need no compatibility update.
- IPv4/IPv6 literals when explicitly allowed, and domain destinations by default.
  Domain names are sent unchanged through
  encrypted DATA and resolved by the gateway; IPv6 targets work even though
  the underlying ICE transport currently uses IPv4.
- Concurrent connections share one WebRTC session and use distinct mux streams.
- Success is returned to the SOCKS client only after the gateway's TCP dial
  succeeds. Refused/unreachable/timeout targets produce SOCKS error replies.
- Bidirectional TCP half-close: upload EOF closes only the remote write side,
  allowing a response that is generated after the target sees EOF.
- Per-stream RESET on I/O errors, canceled handshakes, shutdown and inactivity;
  cancellation does not close unrelated streams.
- Completed streams release receive/retransmit buffers. Retired stream-ID ranges
  coalesce and prevent delayed OPEN/DATA/CLOSE/RESET from recreating old streams;
  duplicate final control frames can still be acknowledged after reclamation.
- Per-connection and service shutdown cancel blocked reads/writes and TCP dials.

## Wire sequence

```text
SOCKS client        local proxy       reliable mux stream        gateway
 greeting  -------> negotiate no-auth locally
 CONNECT   -------> DATA: original CONNECT request ------------> parse target
                                                                TCP dial
 reply     <------- DATA: SOCKS reply + bound address <---------- result
 TCP bytes <======> DATA with per-direction sequence/flow credit <=====> TCP bytes
 FIN       -------> CLOSE (write half only) --------------------> TCP CloseWrite
 error/cancel -----> RESET, delivered reliably by SCTP -----------------> close TCP
```

The request/reply bytes are ordinary SOCKS5 CONNECT records without an extra
SOCKS authentication exchange inside the mux. Keeping them in reliable DATA
handles lost/reordered OPEN and DATA without a separate metadata retry protocol.
Only SDP is sent over HTTP. Application traffic, destination addresses and
connection replies travel inside AEAD-protected DataChannel messages.

## Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-role` | required | `gateway` or `client` |
| `-http` | `127.0.0.1:9300` | Gateway signaling bind / client remote endpoint |
| `-socks` | `127.0.0.1:1080` | Client SOCKS bind; no user/password authentication |
| `-key` | `build/key.bin` | Shared 32-byte master key |
| `-connect-timeout` | `45s` | WebRTC/signaling setup window |
| `-handshake-timeout` | `15s` | SOCKS greeting/request and tunneled CONNECT reply window |
| `-dial-timeout` | `10s` | Gateway TCP dial timeout |
| `-idle-timeout` | `5m` | Connection inactivity timeout; `0` disables |
| `-max-connections` | `128` | Concurrent handlers per endpoint |
| `-rate` | `0` | Aggregate sender pace ceiling, B/s; `0` unlimited |
| `-net` | false | Real-network ICE rather than loopback |
| `-public-ip`, `-udp-port`, `-stun` | empty / 0 / empty | Existing ICE network options |
| `-log`, `-report` | empty | Structured log / final mux statistics JSON |

## Scope and current limits

One gateway process pairs with one client process for its lifetime. A failed
transport exits; automatic reconnection, multiple authenticated peers, SOCKS
username/password, BIND and UDP ASSOCIATE are future work. SOCKS is loopback by
default; choosing another bind address exposes its no-auth listener to that
network. SCTP handles reliability and congestion control; application ARQ/Reno are disabled.
The mux retains stream flow control, delivery ACKs for reclamation and AEAD.
See [DATACHANNEL.md](DATACHANNEL.md) for migration and current measurements.

## Verification

```bash
go test -race ./...
go vet ./...
go build ./...
```

`internal/socks` verifies fragmented negotiation, remote DNS, IPv4/IPv6,
unsupported methods/commands/addresses, refusal replies, concurrent transfers
larger than the flow window, half-close, idle cancellation and session reuse.
Its transport deliberately drops the first OPEN and DATA in each direction.
`internal/mux` additionally tests reliable RESET with dropped control/ACK,
blocked-writer cancellation, congestion-credit release and stream reclamation.
SOCKS4/4a tests cover IPv4/domain targets, eight-byte success/refusal replies,
truncation and bounded field validation, pipelined application bytes,
and responses after TCP FIN, using the same lossy mux fixture.
`TestTwoProcessSOCKSProxy` builds the real proxy with `-race`, starts independent
gateway/client processes, sends four concurrent 300 KiB HTTP bodies through
actual WebRTC DataChannel and verifies their SHA-256/body responses, then tests TCP FIN
before response. Both processes must negotiate SCTP with no video media section, exit cleanly and report
zero active streams and zero decode failures.
