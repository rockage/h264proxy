# H264TunnelLab — Test Report

Recorded on the development host. Reproduce with `scripts/run.sh` (one-click)
or `go test -race ./...` (unit + integration).

## Environment

- Go: `go1.24.7 linux/amd64`
- Pion: `github.com/pion/webrtc/v4 v4.2.26`, `github.com/pion/rtp v1.10.6`
- Transport: two independent OS processes over `127.0.0.1` (loopback), real
  ICE + DTLS + SRTP. No STUN/TURN, no external hosts.

## Quality gates

| Gate | Command | Result |
|------|---------|--------|
| Format | `gofmt -l .` | clean (no files listed) |
| Vet | `go vet ./...` | pass (exit 0) |
| Unit + integration, race | `go test -race ./...` | **all pass** |
| Build | `go build ./...` | pass |

```
ok  github.com/roca/h264-tunnel-lab/internal/crypto      (race)
ok  github.com/roca/h264-tunnel-lab/internal/protocol    (race)
ok  github.com/roca/h264-tunnel-lab/internal/transfer    (race)
ok  github.com/roca/h264-tunnel-lab/tests/integration    (real two-process WebRTC)
```

## Proof the channel is really H.264 over SRTP (not a bypass)

**1. SDP offer** served by the server (media + codec lines):

```
m=video 9 UDP/TLS/RTP/SAVPF 96
a=rtpmap:96 H264/90000
a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f
```

`UDP/TLS/RTP/SAVPF` is DTLS-SRTP; the only payload type offered is `96 = H264`.
No VP8/VP9/AV1 line is present, so negotiation cannot fall back to another codec.

**2. Negotiated codec asserted on both ends** (from the JSON reports): both
server and client record `negotiated_codec = video/H264`, `payload_type = 96`.
Both ends abort if the codec is not H.264.

**3. Same SSRC on both ends.** The client's received-track SSRC equals the
server's sender SSRC in every run (e.g. `1746397649` for the 100 MB run),
confirming the client read the server's actual SRTP media stream — not a side
channel. Data flows: Server RTP → Pion SRTP → loopback UDP → Client SRTP →
`TrackRemote.ReadRTP`.

**4. No DataChannel, no plain UDP, no HTTP data path.** HTTP carries only the
SDP offer/answer. All file bytes travel on the media track.

## Functional transfers (AEAD = AES-256-GCM, chunk = 900 B)

| File size | Chunks | RTP pkts (recv) | Missing | Dup | Reordered | Transfer time | SHA-256 | Peak RSS |
|-----------|--------|-----------------|---------|-----|-----------|---------------|---------|----------|
| 1 KB      | 2      | 4               | 0       | 0   | 0         | < 1 ms        | ✅ match | 13.1 MiB |
| 1 MB      | 1 166  | 1 168           | 0       | 0   | 0         | 0.68 s        | ✅ match | ~17 MiB  |
| 10 MB     | 11 651 | 11 653          | 0       | 0   | 0         | 4.59 s        | ✅ match | 18.3 MiB |
| 100 MB    | 116 509| 116 511         | 0       | 0   | 0         | 19.75 s       | ✅ match | 18.2 MiB |

(RTP packet count = chunks + START + END.)

- **Peak resident memory stays ~18 MiB even for a 100 MB file**, confirming the
  sender and receiver stream to/from disk and never buffer the whole file.
- Encrypt time (server, 100 MB): 0.33 s. Decrypt time (client, 100 MB): 0.16 s.
- Connection establishment: ~0.6 s wall (dominated by the deliberate startup
  delay that lets the receiver attach its track before the first packet).

## M2 — raw payload replacement (NullCipher), byte-exact recovery

Sent over the **same real SRTP path** in `raw` mode and recovered byte-for-byte:

| Vector | Result |
|--------|--------|
| `Hello World` (11 B) | ✅ identical |
| Chinese UTF-8 (`你好，世界！UTF-8 中文测试`) | ✅ identical |
| Data containing `0x00` bytes | ✅ identical (unit) |
| Random binary | ✅ identical (unit) |
| 1 KB | ✅ identical |
| 1 MB | ✅ identical (one-click `MODE=raw`) |

## Negative / integrity tests (failures are detected and reported)

| Scenario | How tested | Expected | Observed |
|----------|-----------|----------|----------|
| Wrong key | client given a different key, over real WebRTC | fail, no output file | client exit **1**, no output file, error `no START frame decoded (wrong key or no data received)` |
| Tampered ciphertext | flip a byte in a DATA payload (unit) | AEAD auth fails → chunk missing → fail | `ErrIncomplete`, no output file |
| Tampered header (AAD) | alter header field (unit) | AEAD auth fails | decryption rejected |
| Missing chunk (loss) | drop a DATA frame (unit) | detected, fail | `missing_chunks > 0`, `ErrIncomplete`, no output |
| Duplicate chunk | duplicate DATA frames (unit) | counted, still recovers | `duplicate_chunks = 2`, SHA match |
| Reordered chunks | shuffle DATA frames (unit) | tolerated, counted | `reordered_chunks > 0`, SHA match |
| Nonce reuse | force counter backwards (unit) | rejected | `ErrNonceReuse` |
| Unlimited send rate | `-rate 0` burst | loss detected if buffer overflows | with 16 MiB RX buffer: 0 loss; if overflowed, correctly reported missing and transfer fails (no corrupt output) |

**Receiver safety property, verified in every failure case: no output file is
produced unless the full-file SHA-256 matches.** The output is written to a temp
file and atomically renamed only on success.

## Unit test inventory

- `internal/protocol`: frame encode/decode round-trip; bad magic; bad version;
  unknown type; bad length; short buffer; START/END metadata round-trip and
  truncation.
- `internal/crypto`: AEAD round-trip; wrong key; tampered ciphertext; tampered
  AAD; wrong nonce; key-size validation; monotonic nonce uniqueness; nonce-reuse
  guard; key fingerprint stability; mode selection.
- `internal/transfer`: happy-path reassembly; reorder tolerance; duplicate
  detection; missing-chunk failure; wrong-key failure; tamper failure; M2 raw
  byte-exact vectors.
- `tests/integration`: real two-process WebRTC transfer with H.264 assertion,
  SSRC-match assertion, packet-count assertion, and byte comparison.

## SHA-256 records (examples)

```
100 MB run:
  expected 863c5a541fe55a9a4a216b8fb3399306c2e570828efedafdd475a6dbf0dda6d9
  actual   863c5a541fe55a9a4a216b8fb3399306c2e570828efedafdd475a6dbf0dda6d9   MATCH
```

Every functional run above reported `sha256_match = true` with identical
expected/actual digests.

## Conclusion

Two independent Go processes establish a real WebRTC session, negotiate H.264 in
SDP, and transfer AES-256-GCM-encrypted data over the genuine SRTP media
channel. The receiver authenticates, decrypts, reassembles, and recovers the
original file with a matching SHA-256, for sizes from 1 KB to 100 MB, while
holding peak memory near 18 MiB. Loss, duplication, reordering, tampering, and
wrong-key conditions are all detected and cause a clean failure with no output
file. The acceptance criterion is met.


## Real-network follow-up — 2026-10-30 (Asia/Shanghai)

Mac darwin/arm64 (Go 1.27.1) to VPS `152.53.13.153`, Linux **aarch64**.
The earlier amd64 deployment could not execute; this run cross-compiled ARM64
binaries from local source. firewalld also had no test ports open. Runtime
rules were added for measurements and removed afterward; future runs must
open the ports as described in DEPLOY.md. No cloud firewall change was needed.

### A: v1 single-direction file, 50 MiB

| Sender rate setting | Sender duration | Reported sender RTP payload rate | Missing DATA chunks | SHA-256 / complete |
| --- | ---: | ---: | ---: | --- |
| Unlimited | 2.132 s | 26.067 MB/s | 9,193 / 58,255 (15.78%) | Failed |
| 2,000,000 B/s | 40.973 s | 1.356 MB/s | 865 / 58,255 (1.48%) | Failed |

Rates above include the protocol header and AEAD overhead carried inside RTP;
they are sender-side rates, not successfully delivered file goodput. A rate
setting is a ceiling, not a measured throughput guarantee. v1 has no ARQ, so
neither run recovered a complete file. The paced trial overlapped only the
tail of the mux trial; these are observations, not a controlled link-capacity
benchmark. Missing chunks can include loss, late arrival and local drops;
these reports do not isolate the source.

Evidence: `cliA.json`, `srvA.json`, `cliA.log`, `srvA.log` (unlimited), and
`build/perf-paced/{cliA,srvA}.{json,log}` (paced). The first failed client
attempt hit the closed host firewall; after opening it, the client was retried
against the same server. Thus unlimited server `connect_seconds` includes that
wait (63.53 s); the successful client connection took 2.07 s. Paced client
connection took 2.21 s.

### B: mux, simultaneous 8 × 1 MiB in each direction

Both endpoints received and hash-verified **8/8 streams**, zero failed streams,
`duplex_confirmed=true`, `video/H264`, and zero AEAD decode failures.

| Metric | Mac dialer | VPS listener |
| --- | ---: | ---: |
| Connection (includes signaling/gathering and listener wait) | 2.876 s | 5.034 s |
| SRTT | 178.95 ms | 194.77 ms |
| RTO | 212.44 ms | 233.14 ms |
| Final cwnd | 11,131 B | 9,403 B |
| Maximum cwnd | 23,660 B | 59,756 B |
| Retransmitted frames | 658 | 864 |
| Congestion reduction events | 155 | 234 |

Dialer workload duration inferred from its own connection and workload-done
log timestamps is approximately 162.8 s: about **50.3 KiB/s of verified file
body per direction**, approximately 100.6 KiB/s aggregate. This is reliable but
slow on this path. Congestion event/retransmit counts are not network packet
loss percentages, and this run does not establish which reductions were
caused by loss versus reordering or timing.

Evidence: `muxcli.json`, `muxsrv.json`, `muxcli.log`, `muxsrv.log`. These reports
were produced before adding `workload_seconds` and progress logging; elapsed
time above is explicitly derived from the logs.

### Changes made during this follow-up

- `scripts/vps_perf.sh`: SSH architecture detection and static cross-build;
  stop on setup failures; archive old reports; collect client/server logs;
  report a failing exit status when a trial fails; PID-file cleanup instead
  of broad process-name matching.
- `cmd/muxnode`: periodic progress, explicit workload duration, and preserve
  errors/session statistics on failure for future runs. No mux transport
  algorithm changes in this follow-up.
- `DEPLOY.md`: corrected the architecture and deployment instructions.

Validation: shell syntax checks, `go test -race ./...`, `go vet ./...`, and
`go build ./...`. The remote ARM64 server/genkey/muxnode were actually run.
The overall script exited **1**, correctly reflecting failed test A despite
successful test B. SOCKS5 implementation remains the next development stage;
the measured mux throughput is a baseline for further tuning.

## Phase 3 — SOCKS5 continuation, 2026-10-30

Implemented `cmd/proxy` with local SOCKS5 TCP CONNECT and a remote TCP gateway.
Default listener `127.0.0.1:1080`; IPv4/IPv6/domain targets, gateway-side DNS,
connection-result replies, concurrent streams, half-close, handshake/dial/idle
timeouts, connection limits and shutdown cancellation are supported.

Targets and replies travel in reliable encrypted DATA at the start of each
stream, followed by TCP bytes. OPEN remains a best-effort announcement. Added
reliable v2 RESET (kind 6), immediate local stream abort notification, congestion
reservation release, and reclamation with coalesced retired IDs and duplicate
final-frame ACKs. v1 file-transfer behavior remains unchanged.

Verified locally on the Mac:

| Test | Result |
| --- | --- |
| Four concurrent 300 KiB requests and full SHA-256/body responses | Passed |
| Remote domain lookup and IPv4/IPv6 target connections | Passed |
| Response generated only after upload TCP FIN | Passed |
| Lost first OPEN/DATA in both directions | Recovered |
| Lost RESET and ACK, blocked writer cancellation | Recovered; no credit leak |
| Target refusal, unsupported auth/command/address, idle timeout | Correct rejection/cancellation |
| Later connection after individual cancellation | Passed on same session |
| Forty closed mux streams, delayed duplicate OPEN/CLOSE | Reclaimed; no reopened stream |
| Real two-process proxy, both subprocesses built with `-race` | Passed; H.264 asserted |
| Final proxy process statistics | Zero active streams; zero decode failures |

The full suite includes the existing v1 and mux/induced-loss integration tests.
Checks: `go test -race ./...`, `go vet ./...`, `go build ./...`, `gofmt -l`.
Local and Linux ARM64 proxy executables are built under `build/proxy` and
`dist/linux-arm64/proxy`. Usage and limits are in `PHASE3_SOCKS.md`.

These phase-3 proxy tests use real WebRTC/SRTP on loopback. The earlier VPS
throughput measurements above are unchanged; no improved WAN performance or
phase-3 WAN proxy measurement is claimed. Automatic reconnection, multi-peer
service, SOCKS authentication and UDP ASSOCIATE remain future work.

## VPS deployment and manual-test readiness — 2026-10-30

Uploaded source and built `cmd/proxy` **natively on Linux ARM64** at
`152.53.13.153`, using Go installed under `/usr/local/go` after validating its
official archive checksum. The VPS build passed `go test ./internal/...` and
`go vet ./cmd/proxy ./internal/...`.

Deployment is `/opt/h264proxy`, managed by enabled systemd service
`h264proxy.service`, running as dedicated user `h264proxy`. SELinux remains
Enforcing; executable labels were restored and the service permits AF_NETLINK
for Go's interface enumeration. The host firewall permanently allows port
**9300/TCP and 9300/UDP** (same numeric port for signaling/media).

The Mac client is managed in the current GUI session by launchd job
`com.roca.h264proxy.client`, exposes SOCKS5 at `127.0.0.1:1080`, and uses a new
private key fetched over scp into `build/proxy-vps.key` (mode 0600). The gateway
key is `/etc/h264proxy/key.bin`, readable only by root and its service group.
No key material was sent through signaling or bundled with source.

Real-WAN acceptance: HTTPS through the local SOCKS5 client to api.ipify.org
returned **152.53.13.153**. Both endpoints logged `video/H264` and matching
session IDs. Verified launchd stop cleans the PID/lock, then a fresh start
restarts the paired gateway and establishes a usable new session. The client
is left running for manual testing. This is a connectivity/HTTPS smoke test,
not a new throughput benchmark.

Manual instructions: `MANUAL_TEST.md`. Double-click helpers:
`scripts/start_client.command` / `scripts/stop_client.command`.

## Chrome compatibility fix — 2026-10-30

Observed repeated ingress rejections from browser requests. A bounded protocol
version diagnostic identified `0x04` (SOCKS4), despite the Chrome extension's
saved proxy UI showing SOCKS5. The original listener accepted only SOCKS5, which
explained why explicit SOCKS5 curl requests worked while Chrome connections reset.

Added local SOCKS4/4a CONNECT parsing and eight-byte replies on the same port.
Both forms become existing reliable SOCKS5 CONNECT records inside the mux;
the already-running VPS gateway is unchanged. Version sniffing consumes only
the protocol byte and preserves pipelined application data. USERID/domain fields
are bounded to 255 bytes; unsupported commands, malformed/truncated fields and
empty SOCKS4a domains are rejected. Target refusal returns SOCKS4 status 91.

New tests cover SOCKS4 and SOCKS4a through the lossy mux fixture, coalesced
CONNECT/application writes, half-close, refusal replies and malformed requests.
`go test -race ./... -count=1` and `go vet ./...` passed. Built and restarted the
Mac client. Real-WAN HTTPS checks via curl using both `--socks4` and
`--socks5-hostname` returned `152.53.13.153`; the original Chrome test tab then
visibly displayed the same VPS IP after enabling its existing WebRTC profile.

Added `scripts/chrome_proxy.command` as an explicit SOCKS5 test-browser launcher.
Also fixed a launchd stop/start race where a job could disappear between querying
and unloading it, and wait for the previous launcher's lock cleanup on restart.
The client's SOCKS listener and Chrome proxy selection are left enabled.

## One-command Linux installer — 2026-10-30

Added the self-contained root `install.sh` and `INSTALL.md`. Eight isolated
Python unittest cases passed, covering fresh/repeated installation, preserving
keys/configuration, private Go bootstrap and actual checksum rejection, build
failure, occupied ports, service validation/health rollback, firewalld/UFW,
disabled firewall management, low-port capability and invalid arguments.
`bash -n install.sh`, `git diff --check`, `go test -race ./...`, `go vet ./...`
and `go build ./...` passed.

Real-host verification used a separate service/account and port 19300 on the
Rocky Linux 10.2 ARM64 VPS, with SELinux Enforcing and active firewalld.
Forced the no-existing-Go path: downloaded the official Go 1.27.3 ARM64 archive,
verified its SHA-256 and built gateway/key generator natively from source commit
`5571134c20ffbff0c1ca5bd5d1229dc654b84e3a`. The installer enabled and started
the non-root systemd service and opened both protocols in runtime/permanent
firewall configuration. A temporary Mac SOCKS client on port 11080 established
H.264-negotiated WebRTC; HTTPS to api.ipify.org returned the VPS public IP.

A second installation without IP/port arguments preserved the key and
configuration byte for byte. An injected post-start failure while changing to
port 19301 restored the previous binary, key, configuration and unit hashes,
left the old service active/enabled, removed only the newly added 19301 rules
and retained 19300 rules. The production 9300 service retained its original PID
throughout these tests. Disposable test services, accounts and firewall rules
were removed afterward. No Ubuntu/amd64 live installation is claimed.

## Remote DNS enforcement — 2026-10-30

Diagnosed the current Mac-to-207.148.110.180 session using server logs and
paired curl checks: Google HTTPS with SOCKS5 hostname resolution returned
HTTP 200, while local resolution timed out. The Mac resolver returned
174.132.167.252 for www.google.com; that address also dominated gateway dial
timeouts. The VPS could resolve and connect to Google directly. This supports
local DNS interference rather than a failed WebRTC tunnel.

The proxy command now defaults to `-remote-dns-only=true`. Tests verify local
rejection with protocol-correct replies before any mux stream opens for SOCKS4,
SOCKS5 IPv4/IPv6 and IP literals disguised as domain requests. Domain requests
over SOCKS5 and SOCKS4a retain relay and half-close behavior over the lossy mux
fixture. The real two-process WebRTC integration test now checks the command's
default IP rejection and transfers four concurrent HTTP payloads using hostnames.
Explicit `-remote-dns-only=false` retains the original literal-IP behavior;
existing library compatibility tests still exercise that behavior.

The Mac Chrome helper uses explicit SOCKS5 and Chromium host resolver rules in
a separate profile, reuses the running local client and no longer starts a
launcher targeting the old developer VPS. Shell syntax was checked; this change
does not claim an automated visual browser acceptance test. It does not change
system DNS, recover hostnames from IPs, or control DNS in other applications.
Validation passed: `go test -race ./...`, `go vet ./...`, `go build ./...`,
`bash -n scripts/chrome_proxy.command` and `git diff --check`. Rebuilt the Mac
client; a running old process must be restarted to enable the new default.

## First transport performance corrections — 2026-10-30

Saved the usable version before editing as local commit `ae44556` and annotated
tag `stable-pre-performance-20261030`. Corrected pacing timer credit, batched
ACK RTT sampling and premature fast retransmit on mild reordering. Full race
tests, vet, builds and added transport regression tests passed. WAN A/B results,
including rejected experiments and measurement limits, are recorded in
[PERFORMANCE_TUNING.md](PERFORMANCE_TUNING.md).
