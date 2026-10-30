# RTP loss recovery correction — 2026-10-30

The selected carrier remains **H.264-negotiated RTP over WebRTC/SRTP**. No
DataChannel proxy backend, carrier fallback or video encoder was introduced.
The benchmark command from the previous investigation remains diagnostic only.
Traffic resemblance to real video and resistance to blocking are not validated.

## Reproduction and correction

A pipelined in-memory WAN fixture models 80ms RTT and 900 KB/s serialization.
Every 29th DATA packet is delayed by 20ms, without dropping a packet. Before
this correction a 1 MiB transfer took 10.84s, scheduled 22 retransmissions and
recorded 21 congestion reductions. Thus low throughput was reproducible without
physical packet loss.

The corrected version completed the same fixture in approximately 1.49s with
zero retransmissions and zero congestion reductions, in three consecutive race
runs. Other cases cover a 20ms path, an 80ms path and bandwidth limitation
without reordering. They also completed without manufactured losses.

Loss detection now:

- Includes the actual 50ms ACK/maintenance scheduling granularity in the RTO
  estimate, rather than assuming a 1ms scheduling granularity.
- Uses current delivery timing and RTT variation before interpreting a selective
  ACK gap as loss. A smoothed RTT alone lags queue growth/drain.
- Retains the observed queue-delay peak for packets still in that older flight.
  It releases the peak when that flight drains, so a transient does not constrain
  a long-running stream indefinitely.
- Gives the feedback path one additional round-trip plus its scheduling tick
  before recovery reduces the congestion window. The shared recovery budget is
  `max(estimated RTO, 2*SRTT + 50ms)`, clamped to 100ms–2s. Per-stream in-flight
  queue guards and the existing per-frame exponential backoff still apply.

The prior investigation's writer-clock correction, bounded queued retransmits,
feedback priority/fairness and slower idle heartbeats are also retained. Every
retransmission still receives a fresh nonce; protocol v2 framing, per-direction
keys, authentication and reliable CLOSE/RESET remain unchanged.

## Observability

The proxy logs `mux_stats` every five seconds (disable with `-stats-interval 0`):

- `FastRetransmits` and `TimeoutRetransmits` split the scheduled recovery attempts.
- `RTOMillis` is the shared estimator; `RecoveryRTOMillis` includes the feedback
  guard. An older in-flight stream may retain an additional queue-delay guard.
- `rtp_receive` reports authenticated RTP packets, forward sequence gaps, out-of-
  order packets and recovered/unrecovered gaps. Reordering is not final loss.
  Tracking starts at the first received packet and uses a bounded 4096-slot
  late-recovery history, so these are observations, not a packet-capture proof
  of physical network loss. Sequence-wrap and duplicate-recovery tests pass.

## Real-network evidence

All tests used isolated port 19301 and test SOCKS port 11080 on the same VPS.
The production service was left running during experiments. The current Mac
route traversed a TUN interface; no system routing or other proxy settings were
changed. HTTPS checks used the same Cloudflare download endpoint, sequentially.

Before the additional feedback-round guard:

| Download | Time | Payload average |
| --- | ---: | ---: |
| 5,000,000 bytes | 5.516s | 906,445 B/s |
| 20,000,000 bytes | 27.065s | 738,975 B/s |

The first download scheduled 233 retransmissions alongside about 231 RTP gaps
at the receiver. During the larger download, scheduled recovery rose by 267
while unrecovered RTP gaps rose by only two. This motivated the additional
feedback-round guard rather than treating every timeout as real packet loss.

The original production version's earlier 5 MB check was 22.678s (220,478 B/s).
These are sequential observations under variable WAN load, not a guaranteed
multiplier. The final guarded run and deployment checks are recorded below.

## Validation

```bash
go test -race ./... -count=1
go test -race ./internal/mux -run TestLossFreeWANDoesNotManufactureLoss -count=3 -v
go vet ./...
go build ./...
```

Regression tests also cover true induced loss, queued-frame timing, selective
ACK repetition, recovery-budget expiry, flow control, stream cancellation,
half-close and real two-process WebRTC transfers. Raw reports are local ignored
artifacts under `build/diagnostics2/`.

## Final guarded RTP run

| Download | Time | Payload average |
| --- | ---: | ---: |
| 5,000,000 bytes | 4.438s | 1,126,556 B/s |
| 20,000,000 bytes | 18.938s | 1,056,067 B/s |

Both returned HTTP 200. Across these two transfers the gateway scheduled 83
retransmissions (46 ACK-driven, 37 timeout-driven), with five congestion loss
events. The client observed 89 unrecovered RTP gaps; not all missing packets
need be DATA. The gateway's received RTP gaps all recovered as reordered packets.
There were zero mux authentication/decode failures. Path loss differed between
runs, so these counters must not be presented as a controlled loss-rate study.

Full fresh race tests, vet, builds and Linux-native media/mux tests passed.
This is improved sustained HTTPS throughput while retaining the RTP carrier;
it does not prove stable 1080P/4K playback or video-traffic indistinguishability.
