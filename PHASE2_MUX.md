# Phase 2 — Full-Duplex Multi-Stream Multiplexing

This phase adds a full-duplex, multi-stream layer on top of the real WebRTC
H.264 SRTP channel from phase 1. The phase-1 one-way file transfer
(`internal/protocol` + `internal/transfer`, protocol v1) is unchanged and still
passes; the mux is a separate v2 layer.

## What it adds

- **Full duplex.** A single sendrecv H.264 transceiver carries RTP in both
  directions; each peer has one send track and one received track. Verified:
  both `OnTrack` callbacks fire and each side reads the other's media.
- **Many concurrent logical streams** in each direction over the one media
  channel, each an `io.ReadWriteCloser` with independent in-order reassembly and
  duplicate/reorder/loss detection.
- **Reliability (selective-repeat ARQ).** Lost DATA/CLOSE frames are recovered
  by retransmission driven by receiver ACKs (cumulative + selective ranges) and
  an RTO timer. A lost frame no longer fails a stream.
- **Congestion control (TCP-Reno-style AIMD).** A session-level congestion
  window limits bytes in flight and reacts to loss (slow start → congestion
  avoidance; halve on loss, restart on timeout), so the sender adapts its rate
  to the path instead of relying only on a fixed pace.
- **Credit-based flow control** per stream (idempotent cumulative WINDOW credit)
  plus a session-level send pace, so a fast stream can't overflow the media
  receive path or starve others, and a lost WINDOW cannot deadlock the sender.
- **Per-direction keys.** Each direction uses its own HKDF-SHA256 subkey derived
  from the master key + session id, so the two directions never share a key —
  eliminating any cross-direction nonce-reuse concern (an improvement over the
  v1 note about the 4-byte prefix).

## Design

### Topology
One `sendrecv` video transceiver (one `m=video` line). The listener (offerer)
adds its track and creates the offer; the dialer (answerer) adds its track and
answers. Each side sends on its local `TrackLocalStaticRTP` and receives on the
remote `TrackRemote` delivered by `OnTrack`.

Because `OnTrack` only fires once the peer sends its first RTP packet, the mux
Session is built with a `media.DeferredRTPReader` as its source: the demux
goroutine blocks until the remote track appears, while the send side is usable
immediately. Both peers start sending on connect, so each bootstraps the other.

### v2 frame (`internal/muxproto`)
42-byte header + AEAD payload (header authenticated as AAD):

```
Magic "H2TL" | Version(=2) | FrameType | SessionID(8) | StreamID(4) |
Seq(8) | NoncePrefix(4) | NonceCounter(8) | PayloadLength(4) | payload
```

FrameTypes: `OPEN` (new stream), `DATA` (stream bytes, `Seq` orders them),
`CLOSE` (`Seq` = total DATA frames sent, so the receiver can detect a short
stream), `WINDOW` (payload = absolute uint64 credit), `ACK` (cumulative and
selective receipt), and `RESET` (phase-3 cancellation of both stream directions).

### Keys
`DeriveKey(master, sessionID, dir)` = HKDF-SHA256 with salt = sessionID and
info = direction label. The sender uses its direction's subkey; the receiver
opens with the same subkey. Nonces remain prefix(4)‖counter(8) with a strictly
monotonic per-direction counter (reuse-guarded by `cryptox.Sealer`).

### Streams and flow control (`internal/mux`)
- Stream ids: listener allocates even, dialer odd (no collisions). A frame that
  references an unknown peer-allocated id auto-creates the stream and enqueues
  it for `Accept` (tolerates OPEN/DATA reordering).
- `Stream.Write` fragments into ≤900-byte DATA frames and blocks on the peer's
  advertised window; `Stream.Read` returns in-order bytes and, as it consumes,
  sends WINDOW refills (initial window 256 KiB, refill at half).
- A single writer goroutine seals, paces, and writes every frame — keeping the
  nonce counter monotonic, serializing RTP writes, and applying the send pace.
- Out-of-order DATA is buffered by `Seq`; duplicates and reorders are counted.
  A gap at CLOSE waits for ARQ recovery before EOF is delivered.

### Reliability — selective-repeat ARQ
DATA frames carry a per-stream `Seq`; CLOSE occupies the seq just past the last
DATA. The receiver sends `ACK` frames carrying a cumulative ack (lowest seq not
yet received) plus up to 32 selective ranges of out-of-order received seqs, and
a close flag. ACKs are sent on a detected gap, on CLOSE, every 8 delivered
frames, and each maintenance tick (50 ms) while a stream is active. WINDOW
credit is re-advertised every 200 ms.

The sender keeps every unacked DATA/CLOSE frame in a retransmit buffer and
resends it (a) immediately when an ACK shows it is still missing (fast
retransmit) and (b) when its RTO elapses. After 60 unacknowledged attempts a
stream is failed as "peer unreachable". Retransmissions reuse the frame's `Seq`
(so the receiver dedups) but get a fresh nonce counter, so AEAD nonce uniqueness
is preserved. Flow-control WINDOW credit is cumulative and re-advertised
periodically, so a lost WINDOW is self-correcting rather than a deadlock.
ACK/WINDOW frames are themselves best-effort; the heartbeat and RTO timer make
the loop converge.

**Adaptive RTO.** The timeout is not fixed. A per-path (per-session) estimator
follows Jacobson/Karels (RFC 6298): `SRTT = 7/8·SRTT + 1/8·R`,
`RTTVAR = 3/4·RTTVAR + 1/4·|SRTT−R|`, `RTO = SRTT + max(4·RTTVAR, G)`, with
`G = 50ms` matching ACK/maintenance scheduling, clamped to
[100 ms, 2 s] with a 250 ms initial value. RTT samples R are taken only from
frames acknowledged without ever being retransmitted (Karn's algorithm), so an
ambiguous sample never corrupts the estimate. Each frame additionally backs off
exponentially by its own retry count (`RTO << min(tries−1, 4)`), so a
repeatedly-lost frame is retried less aggressively while fresh samples keep the
base RTO tracking the path. Timing starts when the writer attempts a packet
send, excluding residence in the application send queue. Recovery also leaves
one additional feedback round plus the maintenance tick (`2*SRTT + 50ms`,
bounded by the same limits), and retains an older flight's queue-delay budget
until that flight drains. Repeated selective ACKs cannot repeatedly reschedule
the same queued frame. See `RTP_REORDER_FIX.md` for regression evidence. The
current base and recovery RTO are exposed in transport statistics.

### Congestion control — AIMD
A session-level controller (TCP Reno style) bounds the bytes in flight across
all streams with a congestion window `cwnd`. New DATA from `Stream.Write` is
admitted only while `inFlight + size ≤ cwnd` (a frame larger than the whole
window is allowed only when nothing else is in flight, to avoid deadlock);
ACK, WINDOW and retransmit frames are never gated, so feedback always flows.

- **Slow start** (`cwnd < ssthresh`): `cwnd += bytes_acked` per ACK — doubles
  per RTT.
- **Congestion avoidance** (`cwnd ≥ ssthresh`): `cwnd += MSS·bytes_acked/cwnd`
  — ~1 MSS per RTT.
- **Loss**: at most once per RTT, `ssthresh = cwnd/2`; a fast-retransmit signal
  (ACK-revealed gap) sets `cwnd = ssthresh`, while an RTO timeout restarts slow
  start (`cwnd = initial`). Floor `2·MSS`, initial `10·MSS` (RFC 6928).

The effective send limit is `min(per-stream flow window, session cwnd)`. The
fixed pacer remains as a coarse safety cap; `cwnd` is the adaptive control. Set
`Config.NoCongestion` to disable it. `muxnode` reports `cwnd_bytes`,
`max_cwnd_bytes`, `ssthresh_bytes`, `cong_loss_events`.

### Deterministic shutdown
`muxnode` uses a two-party rendezvous over the HTTP control channel (`/done`):
neither side tears down the media channel until both have finished, so no
in-flight stream is cut off.

## Why pacing is still used

Pacing (default 16 MB/s) keeps the loss-free path loss-free: an early unpaced
8+8-stream run overflowed the media receive path and dropped frames. With the
ARQ now in place, such loss is recovered rather than fatal — but pacing still
avoids paying retransmission cost for self-inflicted overflow. Reliability
handles the loss that pacing cannot prevent (a genuinely lossy path).

## Results (real two-process runs, loopback, AES-256-GCM unless noted)

All runs: both processes exit 0, codec `video/H264`, SSRCs cross-matched
(each side's local SSRC equals the other's remote SSRC), duplex confirmed.

| Scenario | Streams each way | Size/stream | recv OK | recv fail | dup | reorder | decode fail |
|----------|------------------|-------------|---------|-----------|-----|---------|-------------|
| Baseline | 8                | 256 KiB     | 8 / 8   | 0         | 0   | 0       | 0           |
| High concurrency | 32       | 128 KiB     | 32 / 32 | 0         | 0   | 0       | 0           |
| Large streams | 4           | 2 MiB       | 4 / 4   | 0         | 0   | 0       | 0           |
| Raw mode (NullCipher) | 8   | 256 KiB     | 8 / 8   | 0         | 0   | 0       | 0           |

"recv OK" is counted on **both** peers (each verifies the SHA-256 embedded in
every stream it receives). High concurrency = 64 simultaneous logical streams
across the two directions on one media channel.

### Reliability under induced loss

Loss is injected with `netsim.LossySink`, which drops a fraction of outbound
frames (the sender believes it sent them). Recovery is byte-exact in every case.

| Scenario | Loss each way | Streams each way | Size | recv OK | decode fail | retransmits (l / d) | frames dropped (l / d) |
|----------|---------------|------------------|------|---------|-------------|---------------------|------------------------|
| In-memory unit (`TestMuxReliableUnderLoss`) | 20% | 3 | 200 KiB | all | 0 | 165 / 214 | 309 / 337 |
| Real WebRTC, two processes | 10% | 6 | 256 KiB | 6 / 6 | 0 | ~409 / ~397 | ~413 / ~395 |

Both directions dropped hundreds of frames and retransmitted hundreds; every
stream still arrived complete and SHA-256-correct, confirming the ARQ.

The RTO adapts rather than sitting at a constant. In one 12%-loss run of 8×512 KiB
streams each way, the two ends settled at different operating points —
dialer SRTT ≈ 176 ms → RTO ≈ 179 ms, listener SRTT ≈ 372 ms → RTO ≈ 1109 ms —
reflecting each side's actual ACK latency under its queue, both still recovering
every stream.

### Congestion window responds to loss

The congestion window opens on a clean path and collapses under loss (6×512 KiB
streams each way):

| Path | recv OK | max cwnd | final cwnd | ssthresh | loss events |
|------|---------|----------|------------|----------|-------------|
| No injected loss | all | ~836 KB | ~65 KB | ~55 KB | few (capacity probing) |
| 12% injected loss | all | ~139 KB | ~15 KB | ~3.5 KB | ~215 |

Under heavy loss the window rides near its floor (classic AIMD sawtooth),
throttling the sender to what the path sustains, while reliability still
delivers every stream byte-exactly.

## Tests

- `internal/muxproto`: v2 encode/decode round-trip, bad magic/version/type/
  length, payload and header (AAD) tamper detection, per-direction key
  derivation (distinct, deterministic, session-bound).
- `internal/mux` (race): single stream; concurrent **bidirectional** 4+4 streams
  of 600 KiB each (exceeds the window, exercising WINDOW refills) with no
  cross-talk; raw mode; **reliable recovery under 20% loss** with retransmits
  asserted > 0; the RTO estimator math (first sample, convergence, clamps, Karn,
  backoff); and the congestion controller (slow start, avoidance, per-RTT loss
  halving, severe reset, floor, grant blocking/abort).
- `tests/integration` (real two processes): `TestTwoProcessMuxDuplex` (each side
  opens N streams, recovers all N, asserts H.264, duplex, SSRC cross-match, zero
  decode failures) and `TestTwoProcessMuxReliableUnderLoss` (10% induced loss,
  all streams recovered, retransmits and drops asserted > 0).

```
go test -race ./...   # all unit tests + both integration tests
```

## Try it

```bash
go build -o build/genkey ./cmd/genkey
go build -o build/muxnode ./cmd/muxnode
build/genkey -out build/key.bin -force

# terminal 1
build/muxnode -role listener -http 127.0.0.1:8080 -key build/key.bin -streams 16 -size 131072

# terminal 2
build/muxnode -role dialer   -http 127.0.0.1:8080 -key build/key.bin -streams 16 -size 131072
```

Flags: `-mode aead|raw`, `-streams N`, `-size BYTES`, `-rate BYTES/SEC`
(0 = unlimited), `-loss FRACTION` (drop a fraction of outbound frames to
exercise the ARQ), `-report FILE`, `-log FILE`.

## Known limitations (this phase)

- Reliability uses an adaptive RTO (RFC 6298) with a fixed retry cap (60) and a
  fixed [100 ms, 2 s] clamp. Congestion control is classic Reno AIMD (loss-based,
  not delay-based like BBR/Vegas) with fixed MSS/initial/floor constants; it is
  single-path and does not coordinate across sessions.
- The retransmit buffer holds a stream's unacked frames in memory (bounded by
  the flow window); there is no on-disk spill.
- Shutdown barrier is demo-grade (HTTP rendezvous in `muxnode`); a production
  path would carry teardown in-band as mux control frames.

## SOCKS5 continuation (implemented)

Each SOCKS5 CONNECT uses one bidirectional `mux.Stream`. The target request and
connection-result reply use reliable DATA at the beginning of the stream;
subsequent DATA carries TCP bytes. This avoids relying on best-effort OPEN
metadata. `CloseWrite` (also the existing `Close` behavior) preserves the read
direction; reliable RESET cancels both directions. Completed streams are
reclaimed, with compact retired-ID ranges preventing late frames from reopening
them. See [PHASE3_SOCKS.md](PHASE3_SOCKS.md).
